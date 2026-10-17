package store

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burnrate.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("versions = %d", n)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("versions after reopen = %d", n)
	}
}

func TestSnapshotLatestAndDedup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	resets := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	first := provider.Snapshot{
		Provider:   "codex",
		ObservedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		LimitID:    "codex",
		PlanType:   "plus",
		SourceKey:  "hash",
		Origin:     "origin-1",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 40, WindowMinutes: 300, ResetsAt: resets,
		}},
	}
	if err := s.SaveSnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Origin = "origin-2"
	second.ObservedAt = first.ObservedAt.Add(time.Hour)
	second.Windows = []provider.Window{{
		Key: "5h", Slot: "primary", UsedPercent: 70, WindowMinutes: 300, ResetsAt: resets,
	}}
	if err := s.SaveSnapshot(ctx, second); err != nil {
		t.Fatal(err)
	}
	// A null-window snapshot is never inserted. An older percent must not be replaced by nothing.
	series, err := s.LatestSeries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 {
		t.Fatalf("series = %#v", series)
	}
	if series[0].Samples[len(series[0].Samples)-1].Used != 70 {
		t.Fatalf("latest = %#v", series[0].Samples)
	}
	if len(series[0].Samples) != 2 {
		t.Fatalf("samples = %#v", series[0].Samples)
	}

	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("snapshot rows = %d", rows)
	}
}

func TestTokensKeepMaxOutput(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	early := provider.TokenUsage{
		Provider: "claude", MessageID: "msg_1", Model: "partial",
		ObservedAt:  time.Date(2026, 4, 8, 13, 10, 57, 0, time.UTC),
		InputTokens: 1, OutputTokens: 3, CacheCreationInputTokens: 2, CacheReadInputTokens: 3,
	}
	late := provider.TokenUsage{
		Provider: "claude", MessageID: "msg_1", Model: "full",
		ObservedAt:  early.ObservedAt.Add(time.Second),
		InputTokens: 50, OutputTokens: 120, CacheCreationInputTokens: 100, CacheReadInputTokens: 80,
	}
	if err := s.SaveTokens(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTokens(ctx, early); err != nil {
		t.Fatal(err)
	}
	got, err := s.Usage(ctx, "claude", "msg_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputTokens != 120 || got.Model != "full" || got.InputTokens != 50 || got.CacheCreationInputTokens != 100 || got.CacheReadInputTokens != 80 {
		t.Fatalf("partial overwrote the row: %#v", got)
	}
	if !got.ObservedAt.Equal(late.ObservedAt) {
		t.Fatalf("observed = %s", got.ObservedAt)
	}
	bigger := late
	bigger.Model = "final"
	bigger.OutputTokens = 200
	bigger.InputTokens = 90
	bigger.CacheCreationInputTokens = 7
	bigger.CacheReadInputTokens = 8
	bigger.ObservedAt = late.ObservedAt.Add(time.Second)
	if err := s.SaveTokens(ctx, bigger); err != nil {
		t.Fatal(err)
	}
	got, err = s.Usage(ctx, "claude", "msg_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputTokens != 200 || got.Model != "final" || got.InputTokens != 90 || got.CacheCreationInputTokens != 7 || got.CacheReadInputTokens != 8 {
		t.Fatalf("larger row did not replace: %#v", got)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	off, fileID, err := s.Cursor(ctx, "codex", "abc")
	if err != nil || off != 0 || fileID != "" {
		t.Fatalf("off=%d id=%q err=%v", off, fileID, err)
	}
	if err := s.SetCursor(ctx, "codex", "abc", 42, "9:8:7"); err != nil {
		t.Fatal(err)
	}
	off, fileID, err = s.Cursor(ctx, "codex", "abc")
	if err != nil || off != 42 || fileID != "9:8:7" {
		t.Fatalf("off=%d id=%q err=%v", off, fileID, err)
	}
}

func TestAlertDedupKey(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	resets := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if err := s.MarkAlert(ctx, "claude", "subscription", "5h", resets, 50, 55, now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAlert(ctx, "claude", "subscription", "5h", resets, 50, 60, now); err != nil {
		t.Fatal(err)
	}
	sent, err := s.SentThresholds(ctx, "claude", "subscription", "5h", resets)
	if err != nil {
		t.Fatal(err)
	}
	if !sent[50] || sent[75] {
		t.Fatalf("sent = %#v", sent)
	}
	other, err := s.SentThresholds(ctx, "claude", "subscription", "5h", resets.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other window = %#v", other)
	}
}

func TestDBFilesArePrivate(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	path := filepath.Join(t.TempDir(), "burnrate.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", p, info.Mode().Perm())
		}
	}
}

func TestShortBusyTimeoutDropsSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burnrate.db")
	hold, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	conn, err := hold.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)

	start := time.Now()
	other, err := OpenOptions(path, Options{BusyTimeout: StatuslineBusyTimeout})
	if err != nil {
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("open stalled %s: %v", time.Since(start), err)
		}
		return
	}
	defer other.Close()
	start = time.Now()
	err = other.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "claude", ObservedAt: time.Unix(1, 0).UTC(), Origin: "busy",
		Windows: []provider.Window{{
			Key: "5h", Slot: "five_hour", UsedPercent: 10, WindowMinutes: 300,
			ResetsAt: time.Unix(2, 0).UTC(),
		}},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected SQLITE_BUSY")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("write stalled %s: %v", elapsed, err)
	}
}

func TestRejectEmptySnapshot(t *testing.T) {
	s := openTest(t)
	err := s.SaveSnapshot(context.Background(), provider.Snapshot{Provider: "codex"})
	if err == nil {
		t.Fatal("expected error")
	}
}
