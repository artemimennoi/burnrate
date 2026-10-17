package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxUnit(t *testing.T) {
	home := t.TempDir()
	res, err := Install(Options{GOOS: "linux", Home: home, Exe: "/usr/local/bin/burnrate", Enable: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Path, filepath.Join(".config", "systemd", "user", "burnrate.service")) {
		t.Fatalf("path = %s", res.Path)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "ExecStart=/usr/local/bin/burnrate agent") {
		t.Fatalf("unit = %s", text)
	}
	if !strings.Contains(text, "WantedBy=default.target") {
		t.Fatalf("unit = %s", text)
	}
	if !strings.Contains(res.Note, "systemctl --user") {
		t.Fatalf("note = %s", res.Note)
	}
}

func TestDarwinPlistEscapesPath(t *testing.T) {
	home := t.TempDir()
	exe := "/Users/me/bin/burnrate & co"
	res, err := Install(Options{GOOS: "darwin", Home: home, Exe: exe, Enable: false})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "<string>com.artemimennoi.burnrate</string>") {
		t.Fatalf("plist = %s", text)
	}
	if !strings.Contains(text, "burnrate &amp; co") || strings.Contains(text, "burnrate & co") {
		t.Fatalf("plist did not escape the path: %s", text)
	}
	if !strings.Contains(text, "<string>agent</string>") || !strings.Contains(text, "<key>RunAtLoad</key>") {
		t.Fatalf("plist = %s", text)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Logs")); err != nil {
		t.Fatal(err)
	}
}

func TestRejectRelativeExe(t *testing.T) {
	if _, err := Install(Options{GOOS: "linux", Home: t.TempDir(), Exe: "burnrate"}); err == nil {
		t.Fatal("expected error")
	}
}
