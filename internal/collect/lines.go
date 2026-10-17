// Package collect holds file-tail helpers shared by provider collectors.
package collect

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

const maxLine = 4 << 20

// Sum is a hex SHA-256 of b. Collectors use it for idempotency keys and for
// path cursors so file paths are not written to the database.
func Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReadNewLines calls yield for each complete line after offset.
// A trailing partial line is left unconsumed so a writer can finish it,
// including a partial line that is already longer than maxLine.
// A complete line longer than maxLine is skipped and consumed.
// If yield returns an error, the returned offset does not include that line.
func ReadNewLines(path string, offset int64, yield func([]byte) error) (int64, error) {
	return readLines(path, offset, false, yield)
}

func readLines(path string, offset int64, includePartial bool, yield func([]byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}

	br := bufio.NewReaderSize(f, 64*1024)
	pos := offset
	var pending []byte
	var skipped int64
	skipping := false

	consume := func(line []byte) error {
		n := int64(len(line))
		if skipping || skipped > 0 || len(line) > maxLine {
			skipping = false
			pos += skipped + n
			skipped = 0
			return nil
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) > 0 {
			owned := make([]byte, len(trimmed))
			copy(owned, trimmed)
			if err := yield(owned); err != nil {
				return err
			}
		}
		pos += n
		return nil
	}

	for {
		chunk := make([]byte, 64*1024)
		n, rerr := br.Read(chunk)
		data := chunk[:n]
		for len(data) > 0 {
			if skipping {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					skipped += int64(len(data))
					break
				}
				skipped += int64(i + 1)
				pos += skipped
				skipped = 0
				skipping = false
				data = data[i+1:]
				continue
			}
			pending = append(pending, data...)
			data = nil
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := make([]byte, i+1)
				copy(line, pending[:i+1])
				pending = append([]byte(nil), pending[i+1:]...)
				if err := consume(line); err != nil {
					return pos, err
				}
			}
			if len(pending) > maxLine {
				skipped += int64(len(pending))
				pending = nil
				skipping = true
			}
		}
		if rerr == io.EOF {
			if includePartial && len(pending) > 0 && !skipping && len(pending) <= maxLine {
				owned := make([]byte, len(pending))
				copy(owned, pending)
				trimmed := bytes.TrimRight(owned, "\r\n")
				if len(trimmed) > 0 {
					if err := yield(trimmed); err != nil {
						return pos, err
					}
				}
				pos += int64(len(pending))
				return pos, nil
			}
			// A growing file's unfinished line stays unconsumed.
			return pos, nil
		}
		if rerr != nil {
			return pos, rerr
		}
	}
}

// Tail reads new lines and reports the file identity to store with the cursor.
// A replaced file (new inode, shrink, or same-size rewrite) is read from the start.
func Tail(path string, offset int64, fileID string, yield func([]byte) error) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return offset, fileID, err
	}
	if CursorStale(fileID, info, offset) {
		offset = 0
		fileID = ""
	}
	// A file that has not changed since the last scan is done growing, so a
	// final line without a newline is complete. A changed file keeps the
	// partial unconsumed until a later scan sees it stay the same size.
	stable := fileID != "" && fileID == FileID(info) && offset < info.Size()
	var newOffset int64
	if stable {
		newOffset, err = readLines(path, offset, true, yield)
	} else {
		newOffset, err = readLines(path, offset, false, yield)
	}
	info2, serr := os.Stat(path)
	if serr != nil {
		return newOffset, fileID, err
	}
	return newOffset, FileID(info2), err
}
