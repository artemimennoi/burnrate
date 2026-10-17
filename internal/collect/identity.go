package collect

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// FileID identifies a file at a path so a replacement can be told from an append.
// The form is inode:mtimeUnixNano:size.
func FileID(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d", inode(info), info.ModTime().UnixNano(), info.Size())
}

func inode(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0
	}
	return uint64(st.Ino)
}

// CursorStale reports that offset belongs to a different file than info.
// An append (size grew) is not stale. A new inode, a shrink, or a same-size
// rewrite (mtime changed, size did not grow) is stale.
func CursorStale(stored string, info os.FileInfo, offset int64) bool {
	if info == nil {
		return false
	}
	if offset > info.Size() {
		return true
	}
	ino, mtime, size, ok := parseFileID(stored)
	if !ok {
		return false
	}
	cur := inode(info)
	if ino != 0 && cur != 0 && ino != cur {
		return true
	}
	if info.Size() < size {
		return true
	}
	if info.ModTime().UnixNano() != mtime && info.Size() <= size {
		return true
	}
	return false
}

func parseFileID(stored string) (ino uint64, mtime int64, size int64, ok bool) {
	parts := strings.Split(stored, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	ino, err = strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	mtime, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	size, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	return ino, mtime, size, true
}
