package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/artemimennoi/burnrate/internal/collect"
	"github.com/artemimennoi/burnrate/internal/provider"
)

// Collector scans ~/.claude/projects/**/*.jsonl for token usage.
type Collector struct {
	Dir string
	Log *log.Logger
}

// New returns a transcript collector.
func New(dir string) *Collector {
	return &Collector{Dir: dir}
}

func (c *Collector) Name() string { return "claude" }

func (c *Collector) logf(format string, args ...any) {
	if c != nil && c.Log != nil {
		c.Log.Printf(format, args...)
	}
}

// Collect reads new transcript bytes. A missing directory is not an error.
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
		return stats, fmt.Errorf("claude projects path is not a directory")
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			c.logf("claude: %s: %v", path, walkErr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		stats.Files++
		n, malformed, ferr := consumeTranscript(ctx, sink, path)
		stats.Tokens += n
		stats.Malformed += malformed
		if ferr != nil {
			c.logf("claude: %s: %v", path, ferr)
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	return stats, err
}

func consumeTranscript(ctx context.Context, sink provider.Sink, path string) (tokens, malformed int, err error) {
	sourceKey := collect.Sum([]byte(path))
	offset, fileID, err := sink.Cursor(ctx, "claude", sourceKey)
	if err != nil {
		return 0, 0, err
	}
	newOffset, newID, err := collect.Tail(path, offset, fileID, func(line []byte) error {
		usage, ok := ParseUsage(line)
		if !ok {
			if len(line) > 0 && line[0] != '{' {
				malformed++
			} else if len(line) > 0 && !validJSON(line) {
				malformed++
			}
			return nil
		}
		if err := sink.SaveTokens(ctx, usage); err != nil {
			return err
		}
		tokens++
		return nil
	})
	if newOffset != offset || newID != fileID {
		if cerr := sink.SetCursor(ctx, "claude", sourceKey, newOffset, newID); cerr != nil && err == nil {
			err = cerr
		}
	}
	return tokens, malformed, err
}

func validJSON(line []byte) bool {
	var v any
	return json.Unmarshal(line, &v) == nil
}

// Options runs the statusline wrapper.
type Options struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Command string
	Now     time.Time
	Save    func(provider.Snapshot) error
}

// Run reads the statusline JSON, starts the user's previous statusline command
// immediately, then records subscription windows. The command's stdout is not
// held behind the database write. Recording errors are written to stderr and
// do not fail the command.
func Run(opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	raw, err := io.ReadAll(opts.Stdin)
	if err != nil {
		return err
	}
	cmd, startErr := startCommand(opts, raw)
	recordStatusline(opts, raw)
	if cmd == nil {
		return startErr
	}
	waitErr := cmd.Wait()
	if startErr != nil {
		return startErr
	}
	return waitErr
}

func startCommand(opts Options, raw []byte) (*exec.Cmd, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return nil, nil
	}
	if strings.Contains(command, "burnrate statusline") {
		fmt.Fprintln(opts.Stderr, "burnrate: statusline_command must not invoke burnrate statusline")
		return nil, nil
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func recordStatusline(opts Options, raw []byte) {
	if opts.Save == nil || len(bytes.TrimSpace(raw)) == 0 {
		return
	}
	observed := opts.Now
	if observed.IsZero() {
		observed = time.Now()
	}
	snap, perr := ParseStatusline(raw, observed)
	if perr != nil {
		fmt.Fprintf(opts.Stderr, "burnrate: statusline json: %v\n", perr)
		return
	}
	if len(snap.Windows) == 0 {
		return
	}
	snap.Origin = fmt.Sprintf("statusline:%d:%s", observed.UTC().UnixNano(), collect.Sum(raw))
	if serr := opts.Save(snap); serr != nil {
		fmt.Fprintf(opts.Stderr, "burnrate: record statusline: %v\n", serr)
	}
}
