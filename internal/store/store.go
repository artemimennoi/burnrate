// Package store is the local SQLite database.
// File paths and prompts are not columns. Collectors key cursors by a hash.
package store

import (
	"context"
	"database/sql"
	"fmt"
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
)

var migrations = []string{migration1, migration2, migration3}

// StatuslineBusyTimeout is the SQLite busy wait for the statusline process.
// A locked database drops the sample instead of holding up the user's command.
const StatuslineBusyTimeout = 50 * time.Millisecond

const defaultBusyTimeout = 5 * time.Second

// Options controls a database connection.
type Options struct {
	// BusyTimeout is how long SQLite waits on a lock. Zero uses five seconds.
	BusyTimeout time.Duration
}

// Store is one process-local connection to the burnrate database.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates parent directories, opens the database, and applies migrations.
func Open(path string) (*Store, error) {
	return OpenOptions(path, Options{})
}

// OpenOptions is Open with a caller-chosen busy timeout.
func OpenOptions(path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	busy := busyMillis(opts.BusyTimeout)
	db, err := sql.Open("sqlite", sqliteDSN(path, busy))
	if err != nil {
		return nil, err
	}
	// More than one connection so a read can run while another statement is open.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := prepare(db, busy); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.restrict(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
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

func sqliteDSN(path string, busyMS int) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	// RawQuery is copied as-is. Parentheses must stay literal for the driver.
	u.RawQuery = fmt.Sprintf("_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", busyMS)
	return u.String()
}

func prepare(db *sql.DB, busyMS int) error {
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("journal_mode: %w", err)
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

// SaveSnapshot inserts a snapshot. A repeated origin is ignored.
func (s *Store) SaveSnapshot(ctx context.Context, snap provider.Snapshot) error {
	if snap.Provider == "" || snap.Origin == "" || snap.ObservedAt.IsZero() || len(snap.Windows) == 0 {
		return fmt.Errorf("incomplete snapshot")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, w := range snap.Windows {
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
