package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// StatuslineDepthEnv is set on the wrapped command. It is not a lock: a
// statusline with no burnrate statusline ancestor ignores it. Nesting is
// detected by walking parent processes.
const StatuslineDepthEnv = "BURNRATE_STATUSLINE_DEPTH"

// statuslineTimeout bounds the main wrapped process. On expiry only that
// process group is killed. A normal exit does not kill background children.
var statuslineTimeout = 5 * time.Second

// statuslineWaitDelay is how long Wait keeps reading pipes after the main
// process exits. Background children may still hold those pipes.
var statuslineWaitDelay = 100 * time.Millisecond

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
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if StatuslineAncestor() || DepthAtLeast(2) {
		return nil
	}
	WarnIgnoredDepth(opts.Stderr)
	raw, err := io.ReadAll(opts.Stdin)
	if err != nil {
		return err
	}
	acquired, release := acquireSessionLock(raw)
	defer release()
	if !acquired {
		// Same stdin is already running a statusline. Record this sample and
		// do not start another wrapped command.
		recordStatusline(opts, raw)
		return nil
	}
	cmd, stderr, cleanup, startErr := startCommand(opts, raw)
	defer cleanup()
	opts.Stderr = stderr
	recordStatusline(opts, raw)
	if cmd == nil {
		return startErr
	}
	waitErr := waitCommand(cmd)
	if startErr != nil {
		return startErr
	}
	return waitErr
}

// DepthAtLeast reports whether BURNRATE_STATUSLINE_DEPTH is an integer >= n.
func DepthAtLeast(n int) bool {
	v, ok := os.LookupEnv(StatuslineDepthEnv)
	if !ok {
		return false
	}
	got, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return false
	}
	return got >= n
}

// WarnIgnoredDepth prints one line when BURNRATE_STATUSLINE_DEPTH is set on a
// process that is not nested under statusline. The variable is then ignored.
func WarnIgnoredDepth(w io.Writer) {
	if w == nil {
		return
	}
	if _, ok := os.LookupEnv(StatuslineDepthEnv); !ok {
		return
	}
	fmt.Fprintf(w, "burnrate: %s is set but this process is not nested under statusline; ignoring it\n", StatuslineDepthEnv)
}

func waitCommand(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(statuslineTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return ignoreWaitDelay(cmd, err)
	case <-timer.C:
		killGroup(cmd)
		<-done
		return fmt.Errorf("statusline command timed out")
	}
}

func ignoreWaitDelay(cmd *exec.Cmd, err error) error {
	if !errors.Is(err, exec.ErrWaitDelay) {
		return err
	}
	if cmd != nil && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func startCommand(opts Options, raw []byte) (*exec.Cmd, io.Writer, func(), error) {
	noop := func() {}
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return nil, opts.Stderr, noop, nil
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stdout = opts.Stdout
	cmd.Env = withStatuslineDepth(os.Environ())
	cmd.WaitDelay = statuslineWaitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child, record, cleanup := setupStderr(opts.Stderr)
	cmd.Stderr = child
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, opts.Stderr, noop, err
	}
	return cmd, record, cleanup, nil
}

// setupStderr gives the child an *os.File. A real stderr file is passed
// through, so Wait does not sit on a pipe that a background child inherited.
// Any other writer is fed from a pipe that is closed once the caller finishes
// waiting for the main process.
func setupStderr(dst io.Writer) (child io.Writer, record io.Writer, cleanup func()) {
	noop := func() {}
	if f, ok := dst.(*os.File); ok {
		return f, f, noop
	}
	if dst == nil {
		dst = io.Discard
	}
	r, w, err := os.Pipe()
	if err != nil {
		locked := &syncWriter{w: dst}
		return locked, locked, noop
	}
	locked := &syncWriter{w: dst}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(locked, r)
	}()
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			_ = w.Close()
			_ = r.Close()
			<-done
		})
	}
	return w, locked, cleanup
}

func withStatuslineDepth(env []string) []string {
	prefix := StatuslineDepthEnv + "="
	n := 0
	if v, ok := os.LookupEnv(StatuslineDepthEnv); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			n = parsed
		}
	}
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+strconv.Itoa(n+1))
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
