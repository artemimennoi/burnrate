package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
