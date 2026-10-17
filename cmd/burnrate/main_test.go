package main

import (
	"context"
	"database/sql"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/artemimennoi/burnrate/internal/store"
	_ "modernc.org/sqlite"
)

func withStdio(t *testing.T, stdin string, fn func() int) (int, string, string) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	t.Cleanup(func() {
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	})
	go func() {
		_, _ = io.WriteString(inW, stdin)
		inW.Close()
	}()
	code := fn()
	outW.Close()
	errW.Close()
	out, _ := io.ReadAll(outR)
	errb, _ := io.ReadAll(errR)
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	return code, string(out), string(errb)
}

func quietEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BURNRATE_TELEGRAM_TOKEN", "")
	t.Setenv("BURNRATE_TELEGRAM_CHAT_ID", "")
	t.Setenv("BURNRATE_TIMEZONE", "")
	prev, had := os.LookupEnv("BURNRATE_STATUSLINE_DEPTH")
	os.Unsetenv("BURNRATE_STATUSLINE_DEPTH")
	t.Cleanup(func() {
		if had {
			os.Setenv("BURNRATE_STATUSLINE_DEPTH", prev)
			return
		}
		os.Unsetenv("BURNRATE_STATUSLINE_DEPTH")
	})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestSubcommandFlags(t *testing.T) {
	code, stdout, stderr := withStdio(t, "", func() int {
		return run([]string{"forecast", "--help"})
	})
	if code != 0 || !strings.Contains(stdout, "burnrate forecast") {
		t.Fatalf("help code %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = withStdio(t, "", func() int {
		return run([]string{"status", "--json"})
	})
	if code == 0 || stdout != "" || !strings.Contains(stderr, "json") {
		t.Fatalf("json code %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
}

func TestStatuslineDegradesOnBrokenConfig(t *testing.T) {
	quietEnv(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	payload := "STATUSLINE_PAYLOAD"
	if err := os.WriteFile(cfg, []byte("poll_interval = [\n[claude]\nstatusline_command = \"cat\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BURNRATE_CONFIG", cfg)
	code, stdout, stderr := withStdio(t, payload, func() int {
		return run([]string{"statusline"})
	})
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	if stdout != payload {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "config") || strings.Contains(stdout, "poll_interval") {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}

	if err := os.WriteFile(cfg, []byte("poll_interval = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = withStdio(t, payload, func() int {
		return run([]string{"statusline"})
	})
	if code != 0 || stdout != "" {
		t.Fatalf("quiet code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "config") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestStatuslineDepthExitsQuietly(t *testing.T) {
	quietEnv(t)
	t.Setenv("BURNRATE_STATUSLINE_DEPTH", "")
	t.Setenv("BURNRATE_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	code, stdout, stderr := withStdio(t, "SECRET_STATUS", func() int {
		return run([]string{"statusline"})
	})
	if code != 0 || stdout != "" || strings.Contains(stdout, "SECRET_STATUS") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "ignoring") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestReportsAndStatuslineSurviveLockedDB(t *testing.T) {
	quietEnv(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "burnrate.db")
	cfg := filepath.Join(dir, "config.toml")
	body := "db_path = " + strconv.Quote(dbPath) + "\n[claude]\nenabled = false\nstatusline_command = \"printf hi\"\n[codex]\nenabled = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BURNRATE_CONFIG", cfg)
	created, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	created.Close()

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	u.RawQuery = "_pragma=busy_timeout(2000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)

	start := time.Now()
	code, stdout, stderr := withStdio(t, "", func() int { return run([]string{"status"}) })
	if code != 0 || strings.Contains(stderr, "locked") || time.Since(start) > time.Second {
		t.Fatalf("status code %d after %s stdout %q stderr %q", code, time.Since(start), stdout, stderr)
	}
	start = time.Now()
	code, stdout, stderr = withStdio(t, "", func() int { return run([]string{"forecast"}) })
	if code != 0 || strings.Contains(stderr, "locked") || time.Since(start) > time.Second {
		t.Fatalf("forecast code %d after %s stdout %q stderr %q", code, time.Since(start), stdout, stderr)
	}
	start = time.Now()
	code, stdout, stderr = withStdio(t, `{"rate_limits":{}}`, func() int { return run([]string{"statusline"}) })
	if code != 0 || stdout != "hi" || time.Since(start) > time.Second {
		t.Fatalf("statusline code %d after %s stdout %q stderr %q", code, time.Since(start), stdout, stderr)
	}
}

func TestPollIntervalZeroAllowsReports(t *testing.T) {
	quietEnv(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	body := "poll_interval = \"0s\"\n[claude]\nenabled = false\n[codex]\nenabled = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BURNRATE_CONFIG", cfg)
	code, stdout, stderr := withStdio(t, "", func() int {
		return run([]string{"status"})
	})
	if code != 0 {
		t.Fatalf("status code %d stderr %s stdout %s", code, stderr, stdout)
	}
	if strings.Contains(stderr, "poll_interval") {
		t.Fatalf("status rejected interval: %s", stderr)
	}
	code, stdout, stderr = withStdio(t, "", func() int {
		return run([]string{"forecast"})
	})
	if code != 0 {
		t.Fatalf("forecast code %d stderr %s stdout %s", code, stderr, stdout)
	}
	code, _, stderr = withStdio(t, "", func() int {
		return run([]string{"agent"})
	})
	if code == 0 || !strings.Contains(stderr, "poll_interval") {
		t.Fatalf("agent code %d stderr %s", code, stderr)
	}
}
