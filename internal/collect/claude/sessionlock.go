//go:build linux || darwin

package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/artemimennoi/burnrate/internal/collect"
)

// acquireSessionLock takes a non-blocking exclusive lock named by the
// statusline session. The same stdin (or the same session_id) cannot run two
// wrapped commands at once. A different session uses a different file.
// acquired is false when another holder already has the lock.
func acquireSessionLock(raw []byte) (acquired bool, release func()) {
	release = func() {}
	dir, err := sessionRuntimeDir()
	if err != nil {
		return true, release
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionKey(raw)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true, release
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return false, release
		}
		return true, release
	}
	return true, func() {
		_ = flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

func flock(fd, how int) error {
	for {
		err := syscall.Flock(fd, how)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}

func sessionRuntimeDir() (string, error) {
	var dir string
	if base := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); base != "" {
		dir = filepath.Join(base, "burnrate")
	} else {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("burnrate-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func sessionKey(raw []byte) string {
	if id := sessionID(raw); id != "" {
		return collect.Sum([]byte(id))
	}
	return collect.Sum(raw)
}

func sessionID(raw []byte) string {
	var meta struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.SessionID)
}
