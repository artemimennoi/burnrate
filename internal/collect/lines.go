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
// A trailing partial line is left unconsumed so a writer can finish it.
// A line longer than maxLine is skipped. If yield returns an error, the
// returned offset does not include that line.
func ReadNewLines(path string, offset int64, yield func([]byte) error) (int64, error) {
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
	skipping := false

	flushLine := func(line []byte) error {
		if skipping || len(line) > maxLine {
			skipping = false
			pos += int64(len(line))
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
		pos += int64(len(line))
		return nil
	}

	for {
		chunk := make([]byte, 64*1024)
		n, rerr := br.Read(chunk)
		if n > 0 {
			pending = append(pending, chunk[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := make([]byte, i+1)
				copy(line, pending[:i+1])
				pending = append([]byte(nil), pending[i+1:]...)
				if err := flushLine(line); err != nil {
					return pos, err
				}
			}
			if len(pending) > maxLine {
				pos += int64(len(pending))
				pending = nil
				skipping = true
			}
		}
		if rerr == io.EOF {
			return pos, nil
		}
		if rerr != nil {
			return pos, rerr
		}
	}
}
