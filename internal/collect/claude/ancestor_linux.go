//go:build linux

package claude

import (
	"bytes"
	"os"
	"strconv"
	"strings"
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
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// comm is wrapped in parentheses and may itself contain parentheses.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0, os.ErrInvalid
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 2 {
		return 0, os.ErrInvalid
	}
	return strconv.Atoi(fields[1])
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
	b, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

func argsHaveStatusline(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	for _, arg := range bytes.Split(b, []byte{0}) {
		if string(arg) == "statusline" {
			return true
		}
	}
	return false
}
