package collect

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadNewLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := "one\ntwo\npartial"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []string
	off, err := ReadNewLines(path, 0, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len("one\ntwo\n")) {
		t.Fatalf("offset = %d", off)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("lines = %#v", got)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" done\nthree\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got = nil
	off2, err := ReadNewLines(path, off, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if off2 <= off {
		t.Fatalf("offset did not advance: %d -> %d", off, off2)
	}
	if len(got) != 2 || got[0] != "partial done" || got[1] != "three" {
		t.Fatalf("resumed lines = %#v", got)
	}
}

func TestReadNewLinesYieldErrorKeepsLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	off, err := ReadNewLines(path, 0, func(line []byte) error {
		if string(line) == "b" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if off != int64(len("a\n")) {
		t.Fatalf("offset = %d, want %d", off, len("a\n"))
	}
}

func TestReadNewLinesSkipsOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	var buf bytes.Buffer
	buf.WriteString("keep-me\n")
	buf.Write(bytes.Repeat([]byte("x"), maxLine+10))
	buf.WriteString("\n")
	buf.WriteString("after\n")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	_, err := ReadNewLines(path, 0, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "keep-me" || got[1] != "after" {
		t.Fatalf("lines = %#v", got)
	}
}

func TestReadNewLinesFileShrinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	off, err := ReadNewLines(path, 1000, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len("short\n")) || len(got) != 1 || got[0] != "short" {
		t.Fatalf("off=%d lines=%#v", off, got)
	}
}

func TestReadNewLinesLeavesUnfinishedOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	partial := bytes.Repeat([]byte("x"), maxLine+100)
	if err := os.WriteFile(path, partial, 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	off, err := ReadNewLines(path, 0, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 || len(got) != 0 {
		t.Fatalf("off=%d lines=%#v", off, got)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(bytes.Repeat([]byte("y"), 10), '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("keep\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	off, err = ReadNewLines(path, off, func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "keep" {
		t.Fatalf("lines = %#v", got)
	}
	if off != info.Size() {
		t.Fatalf("offset = %d, size = %d", off, info.Size())
	}
}

func TestCursorStaleAppendAndRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	if err := os.WriteFile(path, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	id := FileID(info)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("b\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	grown, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if CursorStale(id, grown, int64(len("a\n"))) {
		t.Fatal("append looked like a replacement")
	}
	if err := os.WriteFile(path, []byte("zz"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !CursorStale(id, rewritten, int64(len("a\n"))) {
		t.Fatal("same-size rewrite was not detected")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".burn", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("longer line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldIno := info.Sys().(*syscall.Stat_t).Ino
	newIno := replaced.Sys().(*syscall.Stat_t).Ino
	if oldIno == newIno {
		t.Fatal("could not allocate a new inode for the replacement")
	}
	if !CursorStale(id, replaced, int64(len("a\n"))) {
		t.Fatal("new inode was not detected")
	}
}

func TestSumStable(t *testing.T) {
	if Sum([]byte("a")) == Sum([]byte("b")) {
		t.Fatal("hash collision")
	}
	if Sum([]byte("a")) != Sum([]byte("a")) {
		t.Fatal("hash unstable")
	}
}
