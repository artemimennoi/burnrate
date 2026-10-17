// Package store is the local SQLite database.
// File paths and prompts are not columns. Collectors key cursors by a hash.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"

	_ "modernc.org/sqlite"
)

const (
	migration1 = `
CREATE TABLE snapshots (
    id INTEGER PRIMARY KEY,
    provider TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    limit_id TEXT NOT NULL DEFAULT '',
    plan_type TEXT NOT NULL DEFAULT '',
    source_key TEXT NOT NULL DEFAULT '',
    origin TEXT NOT NULL,
    UNIQUE (provider, origin)
);

CREATE TABLE snapshot_windows (
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    window_key TEXT NOT NULL,
    slot TEXT NOT NULL,
    used_percent REAL NOT NULL,
    window_minutes INTEGER NOT NULL,
    resets_at INTEGER NOT NULL,
    PRIMARY KEY (snapshot_id, window_key)
);

CREATE TABLE token_usage (
    provider TEXT NOT NULL,
    message_id TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    PRIMARY KEY (provider, message_id)
);

CREATE TABLE cursors (
    provider TEXT NOT NULL,
    source_key TEXT NOT NULL,
    byte_offset INTEGER NOT NULL,
    PRIMARY KEY (provider, source_key)
);

CREATE TABLE alerts (
    provider TEXT NOT NULL,
    limit_id TEXT NOT NULL DEFAULT '',
    window_key TEXT NOT NULL,
    resets_at INTEGER NOT NULL,
    threshold INTEGER NOT NULL,
    used_percent REAL NOT NULL,
    sent_at INTEGER NOT NULL,
    PRIMARY KEY (provider, limit_id, window_key, resets_at, threshold)
);
`
	migration2 = `
CREATE INDEX idx_snapshots_provider_time ON snapshots(provider, observed_at);
CREATE INDEX idx_windows_reset ON snapshot_windows(window_key, resets_at);
`
	migration3 = `
ALTER TABLE cursors ADD COLUMN file_id TEXT NOT NULL DEFAULT '';
`
	migration4 = `
CREATE INDEX idx_snapshots_observed ON snapshots(observed_at);
CREATE INDEX idx_snapshots_provider_limit_time ON snapshots(provider, limit_id, observed_at);
`
)

var migrations = []string{migration1, migration2, migration3, migration4}

// DefaultRetention is how long snapshots and alerts are kept.
const DefaultRetention = 90 * 24 * time.Hour

// ResetSlack treats resets_at values this close as the same window.
const ResetSlack = 120 * time.Second

// StatuslineBusyTimeout is the SQLite busy wait for the statusline process.
// A locked database drops the sample instead of holding up the user's command.
const StatuslineBusyTimeout = 50 * time.Millisecond

const defaultBusyTimeout = 5 * time.Second

// Options controls a database connection.
type Options struct {
	// BusyTimeout is how long SQLite waits on a lock. Zero uses five seconds.
	BusyTimeout time.Duration
	// Retention drops snapshots and alerts older than this. Zero uses DefaultRetention.
	// A negative value keeps every row. Prune runs only when the caller asks.
	Retention time.Duration
	// ReadOnly serves queries without a write lock. status and forecast use it.
	ReadOnly bool
}

// Store is one process-local connection to the burnrate database.
type Store struct {
	db        *sql.DB
	path      string
	retention time.Duration
}

// Open creates parent directories, opens the database, and applies migrations.
func Open(path string) (*Store, error) {
	return OpenOptions(path, Options{})
}

// OpenOptions is Open with a caller-chosen busy timeout.
// Prune is not run here. The agent deletes old rows on its own schedule.
// ReadOnly opens an existing database without taking a write lock. A missing
// file is created once, then reopened for reading, so a first status still
// has a database to print from.
func OpenOptions(path string, opts Options) (*Store, error) {
	if opts.ReadOnly {
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			created, err := openWrite(path, opts)
			if err != nil {
				return nil, err
			}
			created.Close()
		}
		return openRead(path, opts)
	}
	return openWrite(path, opts)
}

func openWrite(path string, opts Options) (*Store, error) {
	if err := writableDB(path); err != nil {
		return nil, err
	}
	return openDB(path, opts, false, false)
}

func openRead(path string, opts Options) (*Store, error) {
	// A WAL database needs to create -wal and -shm files beside itself. A
	// directory that is not writable fails that open with SQLITE_READONLY_DIRECTORY.
	// immutable=1 reads the main file without those sidecars.
	immutable := !dirWritable(filepath.Dir(path))
	s, err := openDB(path, opts, true, immutable)
	if err != nil && !immutable && readonlyDirectory(err) {
		return openDB(path, opts, true, true)
	}
	return s, err
}

func dirWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o200 == 0 {
		return false
	}
	f, err := os.CreateTemp(dir, ".burnrate-write-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func readonlyDirectory(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "readonly database") || strings.Contains(msg, "1544") || strings.Contains(msg, "READONLY_DIRECTORY")
}

func openDB(path string, opts Options, readOnly, immutable bool) (*Store, error) {
	busy := busyMillis(opts.BusyTimeout)
	db, err := sql.Open("sqlite", sqliteDSN(path, busy, readOnly, immutable))
	if err != nil {
		return nil, explainDB(path, err)
	}
	// More than one connection so a read can run while another statement is open.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := prepare(db, busy, readOnly); err != nil {
		db.Close()
		return nil, explainDB(path, err)
	}
	retention := opts.Retention
	if retention == 0 {
		retention = DefaultRetention
	}
	s := &Store{db: db, path: path, retention: retention}
	if readOnly {
		return s, nil
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, explainDB(path, err)
	}
	if err := s.restrict(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func writableDB(path string) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return fmt.Errorf("database path %s is a directory", path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("database path %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("database directory %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot create database directory %s: %w", dir, err)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("database directory %s is not a directory", dir)
	}
	probe := filepath.Join(dir, ".burnrate-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("database directory %s is not writable: %w", dir, err)
	}
	f.Close()
	os.Remove(probe)
	return nil
}

func explainDB(path string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "out of memory") || strings.Contains(msg, "(14)") {
		return fmt.Errorf("cannot open database %s: path is not writable", path)
	}
	return err
}

func busyMillis(d time.Duration) int {
	if d <= 0 {
		d = defaultBusyTimeout
	}
	ms := int(d / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	return ms
}

func sqliteDSN(path string, busyMS int, readOnly, immutable bool) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	// RawQuery is copied as-is. Parentheses must stay literal for the driver.
	if readOnly {
		q := fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)", busyMS)
		if immutable {
			q = "immutable=1&" + q
		}
		u.RawQuery = q
		return u.String()
	}
	u.RawQuery = fmt.Sprintf("_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", busyMS)
	return u.String()
}

func prepare(db *sql.DB, busyMS int, readOnly bool) error {
	if !readOnly {
		if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
			return fmt.Errorf("journal_mode: %w", err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyMS)); err != nil {
		return fmt.Errorf("busy_timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("foreign_keys: %w", err)
	}
	return nil
}

// restrict keeps the database and its WAL sidecars private to the user.
func (s *Store) restrict() error {
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	for i, script := range migrations {
		version := i + 1
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if err := execScript(tx, script); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, version, time.Now().Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func execScript(tx *sql.Tx, script string) error {
	stmt := ""
	flush := func() error {
		s := strings.TrimSpace(stmt)
		stmt = ""
		if s == "" {
			return nil
		}
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
		return nil
	}
	for _, b := range script {
		if b == ';' {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		stmt += string(b)
	}
	return flush()
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// CanWrite reports whether a write transaction can start. status and forecast
// use it so a locked database falls back to a read instead of failing.
func (s *Store) CanWrite(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("nil store")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `ROLLBACK`)
	return err
}

// SaveSnapshot inserts a snapshot. A repeated origin is ignored.
// resets_at values within ResetSlack of a stored window reuse that window.
// A snapshot whose windows match the latest one updates that row's observed_at
// instead of inserting another row, so the forecast sees the last observation.
func (s *Store) SaveSnapshot(ctx context.Context, snap provider.Snapshot) error {
	if snap.Provider == "" || snap.Origin == "" || snap.ObservedAt.IsZero() || len(snap.Windows) == 0 {
		return fmt.Errorf("incomplete snapshot")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	windows, err := canonicalWindows(ctx, tx, snap.Provider, snap.LimitID, snap.Windows)
	if err != nil {
		return err
	}
	id, observedMS, same, err := sameAsLatest(ctx, tx, snap.Provider, snap.LimitID, snap.PlanType, windows)
	if err != nil {
		return err
	}
	if same {
		if snap.ObservedAt.UnixMilli() > observedMS {
			if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET observed_at = ? WHERE id = ?`, snap.ObservedAt.UnixMilli(), id); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return s.restrict()
		}
		return nil
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots (provider, observed_at, limit_id, plan_type, source_key, origin)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, origin) DO NOTHING`,
		snap.Provider,
		snap.ObservedAt.UnixMilli(),
		snap.LimitID,
		snap.PlanType,
		snap.SourceKey,
		snap.Origin,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	id, err = res.LastInsertId()
	if err != nil {
		return err
	}
	for _, w := range windows {
		if w.Key == "" || w.ResetsAt.IsZero() {
			return fmt.Errorf("window %q missing key or reset time", w.Slot)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_windows (snapshot_id, window_key, slot, used_percent, window_minutes, resets_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, w.Key, w.Slot, w.UsedPercent, w.WindowMinutes, w.ResetsAt.Unix(),
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.restrict()
}

func canonicalWindows(ctx context.Context, tx *sql.Tx, providerName, limitID string, windows []provider.Window) ([]provider.Window, error) {
	out := make([]provider.Window, len(windows))
	copy(out, windows)
	slack := int64(ResetSlack / time.Second)
	for i, w := range out {
		if w.ResetsAt.IsZero() {
			continue
		}
		var canon int64
		err := tx.QueryRowContext(ctx, `
			SELECT w.resets_at FROM snapshot_windows w
			JOIN snapshots s ON s.id = w.snapshot_id
			WHERE s.provider = ? AND s.limit_id = ? AND w.window_key = ?
			  AND abs(w.resets_at - ?) <= ?
			ORDER BY abs(w.resets_at - ?), w.resets_at
			LIMIT 1`,
			providerName, limitID, w.Key, w.ResetsAt.Unix(), slack, w.ResetsAt.Unix(),
		).Scan(&canon)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i].ResetsAt = time.Unix(canon, 0).UTC()
	}
	return out, nil
}

func sameAsLatest(ctx context.Context, tx *sql.Tx, providerName, limitID, plan string, windows []provider.Window) (int64, int64, bool, error) {
	var id int64
	var observed int64
	var gotPlan string
	err := tx.QueryRowContext(ctx, `
		SELECT id, plan_type, observed_at FROM snapshots
		WHERE provider = ? AND limit_id = ?
		ORDER BY observed_at DESC, id DESC
		LIMIT 1`, providerName, limitID).Scan(&id, &gotPlan, &observed)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	if gotPlan != plan {
		return 0, 0, false, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT window_key, used_percent, window_minutes, resets_at
		FROM snapshot_windows WHERE snapshot_id = ?`, id)
	if err != nil {
		return 0, 0, false, err
	}
	defer rows.Close()
	got := map[string]provider.Window{}
	for rows.Next() {
		var w provider.Window
		var resets int64
		if err := rows.Scan(&w.Key, &w.UsedPercent, &w.WindowMinutes, &resets); err != nil {
			return 0, 0, false, err
		}
		w.ResetsAt = time.Unix(resets, 0).UTC()
		got[w.Key] = w
	}
	if err := rows.Err(); err != nil {
		return 0, 0, false, err
	}
	if len(got) != len(windows) {
		return 0, 0, false, nil
	}
	for _, w := range windows {
		prev, ok := got[w.Key]
		if !ok || prev.WindowMinutes != w.WindowMinutes || prev.ResetsAt.Unix() != w.ResetsAt.Unix() {
			return 0, 0, false, nil
		}
		if math.Abs(prev.UsedPercent-w.UsedPercent) > 1e-6 {
			return 0, 0, false, nil
		}
	}
	return id, observed, true, nil
}

// Prune deletes snapshots and alerts older than the retention window.
func (s *Store) Prune(ctx context.Context) error {
	if s == nil || s.retention <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-s.retention).UnixMilli()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE observed_at < ?`, cutoff); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE sent_at < ?`, cutoff); err != nil {
		return err
	}
	return s.restrict()
}

// CountSnapshots returns how many snapshot rows are stored.
func (s *Store) CountSnapshots(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&n)
	return n, err
}

// SaveTokens upserts a usage row, keeping the larger output_tokens count.
func (s *Store) SaveTokens(ctx context.Context, usage provider.TokenUsage) error {
	if usage.Provider == "" || usage.MessageID == "" {
		return fmt.Errorf("incomplete token usage")
	}
	observed := usage.ObservedAt
	if observed.IsZero() {
		observed = time.Unix(0, 0)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO token_usage (
			provider, message_id, observed_at, model,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, message_id) DO UPDATE SET
			model = CASE
				WHEN excluded.output_tokens >= token_usage.output_tokens THEN excluded.model
				ELSE token_usage.model
			END,
			input_tokens = CASE
				WHEN excluded.output_tokens >= token_usage.output_tokens THEN excluded.input_tokens
				ELSE token_usage.input_tokens
			END,
			cache_creation_tokens = CASE
				WHEN excluded.output_tokens >= token_usage.output_tokens THEN excluded.cache_creation_tokens
				ELSE token_usage.cache_creation_tokens
			END,
			cache_read_tokens = CASE
				WHEN excluded.output_tokens >= token_usage.output_tokens THEN excluded.cache_read_tokens
				ELSE token_usage.cache_read_tokens
			END,
			output_tokens = CASE
				WHEN excluded.output_tokens > token_usage.output_tokens THEN excluded.output_tokens
				ELSE token_usage.output_tokens
			END,
			observed_at = CASE
				WHEN excluded.output_tokens >= token_usage.output_tokens THEN excluded.observed_at
				ELSE token_usage.observed_at
			END`,
		usage.Provider,
		usage.MessageID,
		observed.UnixMilli(),
		usage.Model,
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheCreationInputTokens,
		usage.CacheReadInputTokens,
	)
	if err != nil {
		return err
	}
	return s.restrict()
}

// Usage returns one stored token row.
func (s *Store) Usage(ctx context.Context, providerName, messageID string) (provider.TokenUsage, error) {
	var usage provider.TokenUsage
	var observed int64
	err := s.db.QueryRowContext(ctx, `
		SELECT provider, message_id, observed_at, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
		FROM token_usage WHERE provider = ? AND message_id = ?`,
		providerName, messageID,
	).Scan(
		&usage.Provider, &usage.MessageID, &observed, &usage.Model,
		&usage.InputTokens, &usage.OutputTokens, &usage.CacheCreationInputTokens, &usage.CacheReadInputTokens,
	)
	if err != nil {
		return provider.TokenUsage{}, err
	}
	usage.ObservedAt = time.UnixMilli(observed).UTC()
	return usage, nil
}

// CountTokens returns how many usage rows are stored.
func (s *Store) CountTokens(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_usage`).Scan(&n)
	return n, err
}

// Cursor returns the byte offset and file identity for a source.
// A missing row is offset 0 and an empty file id.
func (s *Store) Cursor(ctx context.Context, providerName, sourceKey string) (int64, string, error) {
	var off int64
	var fileID string
	err := s.db.QueryRowContext(ctx, `
		SELECT byte_offset, file_id FROM cursors WHERE provider = ? AND source_key = ?`,
		providerName, sourceKey,
	).Scan(&off, &fileID)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return off, fileID, err
}

// SetCursor records a byte offset and the file identity it belongs to.
func (s *Store) SetCursor(ctx context.Context, providerName, sourceKey string, offset int64, fileID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cursors (provider, source_key, byte_offset, file_id) VALUES (?, ?, ?, ?)
		ON CONFLICT(provider, source_key) DO UPDATE SET
			byte_offset = excluded.byte_offset,
			file_id = excluded.file_id`,
		providerName, sourceKey, offset, fileID,
	)
	if err != nil {
		return err
	}
	return s.restrict()
}

// LatestSeries returns one series per provider, limit, and window key.
func (s *Store) LatestSeries(ctx context.Context) ([]provider.Series, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.provider, s.limit_id, s.plan_type, w.window_key, w.slot,
		       w.window_minutes, w.resets_at
		FROM snapshot_windows w
		JOIN snapshots s ON s.id = w.snapshot_id
		WHERE s.id = (
			SELECT s2.id
			FROM snapshots s2
			JOIN snapshot_windows w2 ON w2.snapshot_id = s2.id
			WHERE s2.provider = s.provider
			  AND s2.limit_id = s.limit_id
			  AND w2.window_key = w.window_key
			ORDER BY s2.observed_at DESC, s2.id DESC
			LIMIT 1
		)`)
	if err != nil {
		return nil, err
	}
	type head struct {
		series provider.Series
		resets int64
	}
	var heads []head
	err = func() error {
		defer rows.Close()
		for rows.Next() {
			var h head
			if err := rows.Scan(
				&h.series.Provider, &h.series.LimitID, &h.series.PlanType, &h.series.WindowKey, &h.series.Slot,
				&h.series.WindowMinutes, &h.resets,
			); err != nil {
				return err
			}
			heads = append(heads, h)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, err
	}
	out := make([]provider.Series, 0, len(heads))
	for _, h := range heads {
		samples, err := s.samples(ctx, h.series.Provider, h.series.LimitID, h.series.WindowKey, h.resets)
		if err != nil {
			return nil, err
		}
		h.series.ResetsAt = time.Unix(h.resets, 0).UTC()
		h.series.Samples = samples
		out = append(out, h.series)
	}
	return out, nil
}

func (s *Store) samples(ctx context.Context, providerName, limitID, windowKey string, resetsAt int64) ([]provider.Sample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.observed_at, w.used_percent
		FROM snapshots s
		JOIN snapshot_windows w ON w.snapshot_id = s.id
		WHERE s.provider = ? AND s.limit_id = ? AND w.window_key = ? AND w.resets_at = ?
		ORDER BY s.observed_at, s.id`,
		providerName, limitID, windowKey, resetsAt,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []provider.Sample
	for rows.Next() {
		var ms int64
		var used float64
		if err := rows.Scan(&ms, &used); err != nil {
			return nil, err
		}
		out = append(out, provider.Sample{At: time.UnixMilli(ms).UTC(), Used: used})
	}
	return out, rows.Err()
}

// SentThresholds lists alert thresholds already recorded for one window.
func (s *Store) SentThresholds(ctx context.Context, providerName, limitID, windowKey string, resetsAt time.Time) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT threshold FROM alerts
		WHERE provider = ? AND limit_id = ? AND window_key = ? AND resets_at = ?`,
		providerName, limitID, windowKey, resetsAt.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var th int
		if err := rows.Scan(&th); err != nil {
			return nil, err
		}
		out[th] = true
	}
	return out, rows.Err()
}

// MarkAlert records that a threshold was delivered for a window.
func (s *Store) MarkAlert(ctx context.Context, providerName, limitID, windowKey string, resetsAt time.Time, threshold int, used float64, sentAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO alerts (provider, limit_id, window_key, resets_at, threshold, used_percent, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, limit_id, window_key, resets_at, threshold) DO NOTHING`,
		providerName, limitID, windowKey, resetsAt.Unix(), threshold, used, sentAt.UnixMilli(),
	)
	return err
}
