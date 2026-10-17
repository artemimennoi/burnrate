package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	if n != 4 {
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
	if n != 4 {
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

func TestResetJitterSharesWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC)
	observed := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	save := func(origin string, used float64, resets time.Time) {
		t.Helper()
		err := s.SaveSnapshot(ctx, provider.Snapshot{
			Provider: "codex", ObservedAt: observed, LimitID: "codex", PlanType: "plus", Origin: origin,
			Windows: []provider.Window{{
				Key: "5h", Slot: "primary", UsedPercent: used, WindowMinutes: 300, ResetsAt: resets,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	save("a", 40, base)
	save("b", 40, base.Add(time.Second))
	n, err := s.CountSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("jittered duplicate stored %d rows", n)
	}
	save("c", 92, base.Add(time.Second))
	n, err = s.CountSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("changed percent stored %d rows", n)
	}
	rows, err := s.db.Query(`SELECT used_percent, resets_at FROM snapshot_windows ORDER BY used_percent`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var used float64
		var resets int64
		if err := rows.Scan(&used, &resets); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]int64{int64(used), resets})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0][0] != 40 || got[1][0] != 92 || got[0][1] != base.Unix() || got[1][1] != base.Unix() {
		t.Fatalf("windows = %#v", got)
	}
	if err := s.MarkAlert(ctx, "codex", "codex", "5h", base, 90, 92, observed); err != nil {
		t.Fatal(err)
	}
	sent, err := s.SentThresholds(ctx, "codex", "codex", "5h", base)
	if err != nil || !sent[90] {
		t.Fatalf("sent = %#v err %v", sent, err)
	}

	save("d", 92, base.Add(121*time.Second))
	n, err = s.CountSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("window outside slack stored %d rows", n)
	}
}

func TestDedupeRetentionAndIndexes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	resets := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	snap := provider.Snapshot{
		Provider: "claude", ObservedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		LimitID: "subscription", PlanType: "max", Origin: "one",
		Windows: []provider.Window{{
			Key: "5h", Slot: "five_hour", UsedPercent: 10, WindowMinutes: 300, ResetsAt: resets,
		}},
	}
	if err := s.SaveSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	snap.Origin = "two"
	snap.ObservedAt = snap.ObservedAt.Add(time.Minute)
	if err := s.SaveSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("identical consecutive snapshots = %d", n)
	}
	snap.Origin = "three"
	snap.Windows[0].UsedPercent = 11
	if err := s.SaveSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	n, err = s.CountSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("changed snapshot count = %d", n)
	}

	var indexes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_snapshots_observed', 'idx_snapshots_provider_limit_time')`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 2 {
		t.Fatalf("indexes = %d", indexes)
	}

	path := filepath.Join(t.TempDir(), "burnrate.db")
	kept, err := OpenOptions(path, Options{Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	oldAt := time.Now().Add(-100 * 24 * time.Hour).UTC()
	recentAt := time.Now().Add(-24 * time.Hour).UTC()
	window := func(origin string, at time.Time) provider.Snapshot {
		return provider.Snapshot{
			Provider: "codex", ObservedAt: at, LimitID: "codex", PlanType: "plus", Origin: origin,
			Windows: []provider.Window{{
				Key: "7d", Slot: "secondary", UsedPercent: 20, WindowMinutes: 10080, ResetsAt: at.Add(24 * time.Hour),
			}},
		}
	}
	if err := kept.SaveSnapshot(ctx, window("old", oldAt)); err != nil {
		t.Fatal(err)
	}
	if err := kept.SaveSnapshot(ctx, window("recent", recentAt)); err != nil {
		t.Fatal(err)
	}
	if err := kept.MarkAlert(ctx, "codex", "codex", "7d", oldAt.Add(24*time.Hour), 50, 20, oldAt); err != nil {
		t.Fatal(err)
	}
	if err := kept.MarkAlert(ctx, "codex", "codex", "7d", recentAt.Add(24*time.Hour), 50, 20, recentAt); err != nil {
		t.Fatal(err)
	}
	if err := kept.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	n, err = kept.CountSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after prune snapshots = %d", n)
	}
	oldSent, err := kept.SentThresholds(ctx, "codex", "codex", "7d", oldAt.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if oldSent[50] {
		t.Fatal("old alert was kept")
	}
	recentSent, err := kept.SentThresholds(ctx, "codex", "codex", "7d", recentAt.Add(24*time.Hour))
	if err != nil || !recentSent[50] {
		t.Fatalf("recent alert = %#v err %v", recentSent, err)
	}

	keepAll, err := OpenOptions(filepath.Join(t.TempDir(), "all.db"), Options{Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer keepAll.Close()
	if err := keepAll.SaveSnapshot(ctx, window("ancient", oldAt)); err != nil {
		t.Fatal(err)
	}
	if err := keepAll.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	n, err = keepAll.CountSnapshots(ctx)
	if err != nil || n != 1 {
		t.Fatalf("negative retention count = %d err %v", n, err)
	}
}

func TestUnwritableDBPath(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(filepath.Join(parent, "burnrate.db"))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "out of memory") || strings.Contains(err.Error(), "(14)") {
		t.Fatalf("misleading sqlite error: %v", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v", err)
	}

	dir := t.TempDir()
	_, err = Open(dir)
	if err == nil {
		t.Fatal("expected error for a directory path")
	}
	if strings.Contains(err.Error(), "out of memory") || strings.Contains(err.Error(), "(14)") {
		t.Fatalf("misleading sqlite error: %v", err)
	}
}

func TestOpenDoesNotPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burnrate.db")
	s, err := OpenOptions(path, Options{Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC()
	if err := s.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "codex", ObservedAt: old, LimitID: "codex", PlanType: "plus", Origin: "old",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 10, WindowMinutes: 300, ResetsAt: old.Add(time.Hour),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	n, err := reopened.CountSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("open pruned snapshots, count = %d", n)
	}
}

func TestReadOnlyDoesNotBlockOnWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burnrate.db")
	rw, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	conn, err := rw.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)

	start := time.Now()
	ro, err := OpenOptions(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.CountSnapshots(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("read-only open blocked for %s", time.Since(start))
	}
	err = ro.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "codex", ObservedAt: time.Now().UTC(), LimitID: "codex", Origin: "ro",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 1, WindowMinutes: 300, ResetsAt: time.Now().Add(time.Hour),
		}},
	})
	if err == nil {
		t.Fatal("read-only store wrote a snapshot")
	}
}

func TestReadOnlyImmutableWithoutWALSidecars(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(time.Millisecond)
	if err := src.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "codex", ObservedAt: observed, LimitID: "codex", PlanType: "plus", Origin: "immutable",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 62, WindowMinutes: 300, ResetsAt: observed.Add(time.Hour),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	src.Close()

	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "burnrate.db")
	body, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(dest + "-wal")
	os.Remove(dest + "-shm")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	ro, err := OpenOptions(dest, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	n, err := ro.CountSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("snapshots = %d", n)
	}
}

func TestRejectEmptySnapshot(t *testing.T) {
	s := openTest(t)
	err := s.SaveSnapshot(context.Background(), provider.Snapshot{Provider: "codex"})
	if err == nil {
		t.Fatal("expected error")
	}
}
