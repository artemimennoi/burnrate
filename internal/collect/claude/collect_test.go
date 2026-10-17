package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"
	"github.com/artemimennoi/burnrate/internal/store"
)

func TestCollectTranscripts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SECRET_PATH_DO_NOT_STORE", "demo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "burnrate.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := New(filepath.Dir(root)).Collect(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.Tokens != 4 {
		t.Fatalf("stats = %#v", stats)
	}
	usage, err := st.Usage(context.Background(), "claude", "msg_1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.OutputTokens != 120 || usage.InputTokens != 5 || usage.CacheCreationInputTokens != 100 {
		t.Fatalf("usage = %#v", usage)
	}
	n, err := st.CountTokens(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("tokens=%d err=%v", n, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(dbPath + "-wal")
	db = append(db, wal...)
	if bytes.Contains(db, []byte("SECRET_PROMPT_DO_NOT_STORE")) || bytes.Contains(db, []byte("SECRET_PATH_DO_NOT_STORE")) {
		t.Fatal("database contains a prompt or a file path")
	}

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stats, err = New(filepath.Dir(root)).Collect(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tokens != 0 {
		t.Fatalf("second pass tokens = %d", stats.Tokens)
	}
}

func TestStatuslinePipesAndRecords(t *testing.T) {
	raw, err := os.ReadFile("testdata/statusline.json")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "burnrate.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var stdout, stderr bytes.Buffer
	observed := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	err = Run(Options{
		Stdin:   bytes.NewReader(raw),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Command: "cat",
		Now:     observed,
		Save: func(snap provider.Snapshot) error {
			return st.SaveSnapshot(context.Background(), snap)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), raw) {
		t.Fatalf("stdout changed")
	}
	series, err := st.LatestSeries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("series = %d", len(series))
	}
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(dbPath + "-wal")
	db = append(db, wal...)
	if bytes.Contains(db, []byte("SECRET_PATH_DO_NOT_STORE")) || bytes.Contains(db, []byte("SECRET_PROMPT_DO_NOT_STORE")) {
		t.Fatal("statusline database contains path or prompt text")
	}

	stdout.Reset()
	err = Run(Options{
		Stdin:   bytes.NewReader([]byte("{not json")),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Command: "cat",
		Now:     observed,
		Save: func(provider.Snapshot) error {
			t.Fatal("saved invalid json")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "{not json" {
		t.Fatalf("piped = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "statusline json") {
		t.Fatalf("stderr = %s", stderr.String())
	}

	stdout.Reset()
	err = Run(Options{
		Stdin:   bytes.NewReader(raw),
		Stdout:  &stdout,
		Stderr:  io.Discard,
		Command: "cat",
		Now:     observed.Add(time.Second),
		Save: func(provider.Snapshot) error {
			return errors.New("disk full")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), raw) {
		t.Fatal("record error blocked the status line")
	}
}

func TestStatuslineStartsCommandBeforeSave(t *testing.T) {
	raw := []byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1792300000}}}`)
	release := make(chan struct{})
	saveEntered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(Options{
			Stdin:   bytes.NewReader(raw),
			Stdout:  lockedWriter{mu: &mu, w: &stdout},
			Stderr:  io.Discard,
			Command: "echo ready",
			Now:     time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
			Save: func(provider.Snapshot) error {
				once.Do(func() { close(saveEntered) })
				<-release
				return nil
			},
		})
	}()
	select {
	case <-saveEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("save was not called")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := stdout.String()
		mu.Unlock()
		if strings.Contains(got, "ready") {
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	<-done
	t.Fatal("command output did not appear while save was blocked")
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestStatuslineDepthWithoutAncestorStillRuns(t *testing.T) {
	t.Setenv(StatuslineDepthEnv, "")
	marker := filepath.Join(t.TempDir(), "ran")
	var stdout, stderr bytes.Buffer
	saved := false
	err := Run(Options{
		Stdin:   bytes.NewReader([]byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1792300000}}}`)),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Command: "touch " + marker + "; printf out",
		Save: func(provider.Snapshot) error {
			saved = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out" || !saved {
		t.Fatalf("stdout %q saved %v", stdout.String(), saved)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "ignoring") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestStatuslineAncestorStopsNested(t *testing.T) {
	if StatuslineAncestor() {
		t.Fatal("this test process is nested under statusline")
	}
	for _, mode := range []string{"parent", "unsets"} {
		cmd := exec.Command(os.Args[0], "statusline")
		cmd.Env = append(dropEnv(os.Environ(), "BURNRATE_ANCESTOR_MODE", StatuslineDepthEnv), "BURNRATE_ANCESTOR_MODE="+mode, StatuslineDepthEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", mode, err, out)
		}
		if strings.TrimSpace(string(out)) != "nested" {
			t.Fatalf("%s output = %q", mode, out)
		}
	}
}

func TestStatuslineCommandTimeoutKillsGroup(t *testing.T) {
	old := statuslineTimeout
	statuslineTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statuslineTimeout = old })
	start := time.Now()
	err := Run(Options{
		Stdin:   bytes.NewReader(nil),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Command: "sleep 30",
	})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatal(err)
	}
}

func TestMain(m *testing.M) {
	switch os.Getenv("BURNRATE_ANCESTOR_MODE") {
	case "parent", "unsets":
		runAncestorFixture()
	default:
		os.Exit(m.Run())
	}
}

func runAncestorFixture() {
	cmd := exec.Command(os.Args[0], "statusline")
	env := dropEnv(os.Environ(), "BURNRATE_ANCESTOR_MODE", StatuslineDepthEnv)
	if os.Getenv("BURNRATE_ANCESTOR_MODE") == "unsets" {
		env = append(env, "BURNRATE_ANCESTOR_MODE=child")
	} else {
		env = append(env, "BURNRATE_ANCESTOR_MODE=child", StatuslineDepthEnv+"=1")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: %v\n%s", err, out)
		os.Exit(1)
	}
	fmt.Print(string(out))
	os.Exit(0)
}

func dropEnv(env []string, names ...string) []string {
	var out []string
	for _, e := range env {
		skip := false
		for _, name := range names {
			if e == name || strings.HasPrefix(e, name+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

func TestStatuslineLeavesBackgroundChildren(t *testing.T) {
	for _, name := range []string{"background", "setsid"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			script := filepath.Join(dir, "run.sh")
			var body string
			if name == "background" {
				body = "#!/bin/sh\nsleep 30 &\necho $! > " + strconv.Quote(pidFile) + "\necho FAST-OUTPUT\n"
			} else {
				body = "#!/bin/sh\nsetsid -f sh -c 'echo $$ > " + pidFile + "; exec sleep 30'\necho FAST-OUTPUT\n"
			}
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			start := time.Now()
			err := Run(Options{
				Stdin:   bytes.NewReader([]byte(`{"session_id":"` + name + `"}`)),
				Stdout:  &stdout,
				Stderr:  &stderr,
				Command: "sh " + strconv.Quote(script),
			})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("err %v after %s stderr %s", err, elapsed, stderr.String())
			}
			if elapsed > time.Second {
				t.Fatalf("took %s stdout %q", elapsed, stdout.String())
			}
			if stdout.String() != "FAST-OUTPUT\n" {
				t.Fatalf("stdout %q", stdout.String())
			}
			pidText, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("child %d is not alive: %v", pid, err)
			}
		})
	}
}

func TestDepthCapExitsImmediately(t *testing.T) {
	t.Setenv(StatuslineDepthEnv, "2")
	marker := filepath.Join(t.TempDir(), "ran")
	var stdout bytes.Buffer
	err := Run(Options{
		Stdin:   bytes.NewReader([]byte(`{"session_id":"depth-cap"}`)),
		Stdout:  &stdout,
		Stderr:  &stdout,
		Command: "touch " + strconv.Quote(marker),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("printed %q", stdout.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran: %v", err)
	}
}

func TestSessionLockStopsDetachedRecursion(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	raw := []byte(`{"session_id":"locked-session","rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1792300000}}}`)
	bin := os.Args[0]
	for _, variant := range []string{"direct", "background", "setsid", "nohup", "deep"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "ran")
			result := filepath.Join(dir, "result")
			inPath := filepath.Join(dir, "in.json")
			if err := os.WriteFile(inPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			inner := fmt.Sprintf("BURNRATE_LOCK_MODE=inner BURNRATE_LOCK_MARKER=%s BURNRATE_LOCK_RESULT=%s %s < %s",
				strconv.Quote(marker), strconv.Quote(result), strconv.Quote(bin), strconv.Quote(inPath))
			var body string
			switch variant {
			case "direct":
				body = inner + "\n"
			case "background":
				body = "( " + inner + " & )\n"
			case "setsid":
				body = "setsid -f sh -c " + strconv.Quote(inner) + "\n"
			case "nohup":
				body = "nohup sh -c " + strconv.Quote(inner) + " >/dev/null 2>&1 &\n"
			case "deep":
				chain := filepath.Join(dir, "chain.sh")
				chainBody := "#!/bin/sh\nif [ \"${BURNRATE_CHAIN:-0}\" -lt 16 ]; then\n  BURNRATE_CHAIN=$((${BURNRATE_CHAIN:-0}+1))\n  export BURNRATE_CHAIN\n  exec sh \"$0\"\nfi\n" + inner + "\n"
				if err := os.WriteFile(chain, []byte(chainBody), 0o700); err != nil {
					t.Fatal(err)
				}
				body = "sh " + strconv.Quote(chain) + "\n"
			}
			body += "i=0\nwhile [ ! -f " + strconv.Quote(result) + " ] && [ \"$i\" -lt 80 ]; do\n  i=$((i+1))\n  sleep 0.05\ndone\n"
			script := filepath.Join(dir, "outer.sh")
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			err := Run(Options{
				Stdin:   bytes.NewReader(raw),
				Stdout:  &stdout,
				Stderr:  &stderr,
				Command: "sh " + strconv.Quote(script),
			})
			if err != nil {
				t.Fatalf("err %v stdout %q stderr %q", err, stdout.String(), stderr.String())
			}
			got, err := os.ReadFile(result)
			if err != nil {
				t.Fatalf("no result: %v stdout %q stderr %q script:\n%s", err, stdout.String(), stderr.String(), body)
			}
			if string(got) != "blocked" {
				t.Fatalf("result %q", got)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("nested command ran: %v", err)
			}
		})
	}
}

func TestSessionLockAllowsAnotherSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	hold := filepath.Join(t.TempDir(), "hold")
	other := filepath.Join(t.TempDir(), "other")
	started := make(chan struct{})
	go func() {
		close(started)
		_ = Run(Options{
			Stdin:   bytes.NewReader([]byte(`{"session_id":"hold"}`)),
			Stdout:  io.Discard,
			Stderr:  io.Discard,
			Command: "touch " + strconv.Quote(hold) + "; sleep 2",
		})
	}()
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(hold); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal("holder did not start")
	}
	err := Run(Options{
		Stdin:   bytes.NewReader([]byte(`{"session_id":"other"}`)),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Command: "touch " + strconv.Quote(other),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
}

func init() {
	if os.Getenv("BURNRATE_ANCESTOR_MODE") == "child" {
		if StatuslineAncestor() {
			fmt.Println("nested")
		} else {
			fmt.Println("not-nested")
		}
		os.Exit(0)
	}
	if os.Getenv("BURNRATE_LOCK_MODE") == "inner" {
		runLockInner()
		os.Exit(0)
	}
}

func runLockInner() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.WriteFile(os.Getenv("BURNRATE_LOCK_RESULT"), []byte("error"), 0o600)
		return
	}
	marker := os.Getenv("BURNRATE_LOCK_MARKER")
	_ = Run(Options{
		Stdin:   bytes.NewReader(raw),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Command: "touch " + strconv.Quote(marker),
	})
	status := "blocked"
	if _, err := os.Stat(marker); err == nil {
		status = "ran"
	}
	_ = os.WriteFile(os.Getenv("BURNRATE_LOCK_RESULT"), []byte(status), 0o600)
}

func TestStatuslineCommandStillRunsWithTwoSpaces(t *testing.T) {
	raw := []byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1792300000}}}`)
	var stdout bytes.Buffer
	saved := 0
	err := Run(Options{
		Stdin:   bytes.NewReader(raw),
		Stdout:  &stdout,
		Stderr:  io.Discard,
		Command: `printf 'burnrate  statusline:%s' "$BURNRATE_STATUSLINE_DEPTH"`,
		Now:     time.Unix(10, 0).UTC(),
		Save: func(provider.Snapshot) error {
			saved++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "burnrate  statusline:1" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if saved != 1 {
		t.Fatalf("saved = %d", saved)
	}
}

func TestCollectContinuesAfterUnreadable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"2026-10-17T12:00:00Z","message":{"id":"msg_ok","model":"m","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "good.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "bad.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/no/such/transcript", filepath.Join(root, "missing.jsonl")); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	c := New(root)
	c.Log = log.New(&logs, "", 0)
	st, err := store.Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stats, err := c.Collect(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tokens != 1 {
		t.Fatalf("stats = %#v log=%s", stats, logs.String())
	}
	if !strings.Contains(logs.String(), "bad.jsonl") || !strings.Contains(logs.String(), "missing.jsonl") {
		t.Fatalf("log = %s", logs.String())
	}
}
