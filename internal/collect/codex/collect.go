package codex

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/artemimennoi/burnrate/internal/collect"
	"github.com/artemimennoi/burnrate/internal/provider"
)

// Collector scans ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
type Collector struct {
	Dir string
	Log *log.Logger
}

// New returns a collector for a sessions directory.
func New(dir string) *Collector {
	return &Collector{Dir: dir}
}

func (c *Collector) Name() string { return "codex" }

func (c *Collector) logf(format string, args ...any) {
	if c != nil && c.Log != nil {
		c.Log.Printf(format, args...)
	}
}

func isRollout(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
}

// Collect reads new bytes from each rollout file. A missing directory is not
// an error. Null-window snapshots are counted as skipped and not stored.
func (c *Collector) Collect(ctx context.Context, sink provider.Sink) (provider.Stats, error) {
	var stats provider.Stats
	if strings.TrimSpace(c.Dir) == "" {
		return stats, nil
	}
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return stats, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return stats, nil
		}
		return stats, err
	}
	if !info.IsDir() {
		return stats, fmt.Errorf("codex sessions path is not a directory")
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			c.logf("codex: %s: %v", path, walkErr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && !isRollout(d.Name()) {
			return nil
		}
		if !isRollout(d.Name()) {
			return nil
		}
		stats.Files++
		snaps, skipped, malformed, ferr := consumeFile(ctx, sink, path)
		stats.Snapshots += snaps
		stats.Skipped += skipped
		stats.Malformed += malformed
		if ferr != nil {
			c.logf("codex: %s: %v", path, ferr)
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	return stats, err
}

func consumeFile(ctx context.Context, sink provider.Sink, path string) (snaps, skipped, malformed int, err error) {
	sourceKey := collect.Sum([]byte(path))
	offset, fileID, err := sink.Cursor(ctx, "codex", sourceKey)
	if err != nil {
		return 0, 0, 0, err
	}
	newOffset, newID, err := collect.Tail(path, offset, fileID, func(line []byte) error {
		switch res := ParseLine(line); res.Kind {
		case KindSnapshot:
			res.Snapshot.SourceKey = sourceKey
			res.Snapshot.Origin = collect.Sum(line)
			if err := sink.SaveSnapshot(ctx, res.Snapshot); err != nil {
				return err
			}
			snaps++
		case KindSkipNull:
			skipped++
		case KindIgnore:
			if len(line) > 0 && line[0] == '{' {
				// A JSON object that is not a rate-limit event is normal.
				// Count only lines that do not parse as JSON.
				if !jsonObject(line) {
					malformed++
				}
			} else if len(line) > 0 {
				malformed++
			}
		}
		return nil
	})
	if newOffset != offset || newID != fileID {
		if cerr := sink.SetCursor(ctx, "codex", sourceKey, newOffset, newID); cerr != nil && err == nil {
			err = cerr
		}
	}
	return snaps, skipped, malformed, err
}

func jsonObject(line []byte) bool {
	// ParseLine already tried json.Unmarshal. KindIgnore covers both
	// non-events and broken JSON. Re-check with a cheap prefix: broken JSON
	// still starts with '{'. Callers pass only lines ParseLine ignored.
	// Distinguishing requires a second parse, done here via encoding/json
	// indirectly: a line that Unmarshal accepts is a non-event.
	return validJSON(line)
}
