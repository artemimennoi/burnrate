package store

import (
	"context"
	"path/filepath"
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
	if n != 2 {
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
	if n != 2 {
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
		Provider: "claude", MessageID: "msg_1", Model: "claude-opus",
		ObservedAt:  time.Date(2026, 4, 8, 13, 10, 57, 0, time.UTC),
		InputTokens: 5, OutputTokens: 3, CacheCreationInputTokens: 100, CacheReadInputTokens: 50,
	}
	late := early
	late.OutputTokens = 120
	late.ObservedAt = early.ObservedAt.Add(time.Second)
	if err := s.SaveTokens(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTokens(ctx, early); err != nil {
		t.Fatal(err)
	}
	var output int64
	var observed int64
	if err := s.db.QueryRow(`SELECT output_tokens, observed_at FROM token_usage WHERE message_id = ?`, "msg_1").Scan(&output, &observed); err != nil {
		t.Fatal(err)
	}
	if output != 120 {
		t.Fatalf("output = %d", output)
	}
	if observed != late.ObservedAt.UnixMilli() {
		t.Fatalf("observed = %d", observed)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	off, err := s.Cursor(ctx, "codex", "abc")
	if err != nil || off != 0 {
		t.Fatalf("off=%d err=%v", off, err)
	}
	if err := s.SetCursor(ctx, "codex", "abc", 42); err != nil {
		t.Fatal(err)
	}
	off, err = s.Cursor(ctx, "codex", "abc")
	if err != nil || off != 42 {
		t.Fatalf("off=%d err=%v", off, err)
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

func TestRejectEmptySnapshot(t *testing.T) {
	s := openTest(t)
	err := s.SaveSnapshot(context.Background(), provider.Snapshot{Provider: "codex"})
	if err == nil {
		t.Fatal("expected error")
	}
}
