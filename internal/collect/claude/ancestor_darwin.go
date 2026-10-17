//go:build darwin

package claude

import (
	"bytes"
	"encoding/binary"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func statuslineAncestor() bool {
	pid := os.Getpid()
	seen := map[int]bool{}
	for i := 0; i < 16; i++ {
		parent, err := parentPID(pid)
		if err != nil || parent <= 1 || seen[parent] {
			return false
		}
		seen[parent] = true
		if sameExecutable(parent) && argsHaveStatusline(parent) {
			return true
		}
		pid = parent
	}
	return false
}

func parentPID(pid int) (int, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(info.Eproc.Ppid), nil
}

func sameExecutable(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	a, err := os.Stat(self)
	if err != nil {
		return false
	}
	path, err := procPath(pid)
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// procPath reads the executable path via the proc_info syscall that
// proc_pidpath wraps. PROC_INFO_CALL_PIDINFO is 2 and PROC_PIDPATHINFO is 11.
func procPath(pid int) (string, error) {
	buf := make([]byte, 4096)
	r1, _, errno := unix.Syscall6(
		unix.SYS_PROC_INFO,
		2,
		uintptr(pid),
		11,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if errno != 0 {
		return "", errno
	}
	n := int(r1)
	if n <= 0 || n > len(buf) {
		return "", unix.ENOENT
	}
	if i := bytes.IndexByte(buf[:n], 0); i >= 0 {
		n = i
	}
	if n == 0 {
		return "", unix.ENOENT
	}
	return string(buf[:n]), nil
}

func argsHaveStatusline(pid int) bool {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) < 4 {
		return false
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for n := 0; n < argc && len(rest) > 0; n++ {
		i := bytes.IndexByte(rest, 0)
		arg := rest
		if i >= 0 {
			arg = rest[:i]
			rest = rest[i+1:]
		} else {
			rest = nil
		}
		if string(arg) == "statusline" {
			return true
		}
	}
	return false
}
