package codex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/artemimennoi/burnrate/internal/store"
)

func TestCollectSkipsPremiumAndResumes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SECRET_PATH_DO_NOT_STORE")
	dir := filepath.Join(root, "2026", "10", "17")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-demo.jsonl")
	var body bytes.Buffer
	body.WriteString(`{"timestamp":"2026-10-17T12:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"plus","primary":{"used_percent":62,"window_minutes":300,"resets_at":1792300000},"secondary":{"used_percent":81,"window_minutes":10080,"resets_at":1792600000}}}}` + "\n")
	body.WriteString(`{"timestamp":"2026-10-17T12:05:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"premium","primary":null,"secondary":null}}}` + "\n")
	body.WriteString(`{"timestamp":"2026-10-17T12:10:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":`)
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "burnrate.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := New(root).Collect(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.Snapshots != 1 || stats.Skipped != 1 {
		t.Fatalf("stats = %#v", stats)
	}

	series, err := st.LatestSeries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var five float64
	for _, s := range series {
		if s.WindowKey == "5h" {
			five = s.Samples[len(s.Samples)-1].Used
		}
	}
	if five != 62 {
		t.Fatalf("5h after premium null = %v (series %#v)", five, series)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"limit_id":"premium","primary":null,"secondary":null}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"timestamp":"2026-10-17T13:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"plus","primary":{"used_percent":70,"window_minutes":300,"resets_at":1792300000},"secondary":null}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	stats, err = New(root).Collect(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Snapshots != 1 || stats.Skipped != 1 {
		t.Fatalf("second stats = %#v", stats)
	}
	series, err = st.LatestSeries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	five = 0
	var samples int
	for _, s := range series {
		if s.WindowKey == "5h" {
			five = s.Samples[len(s.Samples)-1].Used
			samples = len(s.Samples)
		}
	}
	if five != 70 || samples != 2 {
		t.Fatalf("latest 5h = %v samples %d", five, samples)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(dbPath + "-wal")
	raw = append(raw, wal...)
	if bytes.Contains(raw, []byte("SECRET_PATH_DO_NOT_STORE")) {
		t.Fatal("database contains a file path")
	}
}

func TestCollectMissingDir(t *testing.T) {
	stats, err := New(filepath.Join(t.TempDir(), "missing")).Collect(context.Background(), nil)
	if err != nil || stats.Files != 0 {
		t.Fatalf("stats=%#v err=%v", stats, err)
	}
}
