package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	envFile := filepath.Join(home, ".config", "burnrate", "telegram.env")
	if !strings.Contains(text, "EnvironmentFile=-"+envFile) {
		t.Fatalf("unit = %s", text)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("env file should be absent without credentials: %v", err)
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

func TestLinuxXDGAndEnvFile(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "xdg")
	token := "123456:SECRETTOKEN"
	res, err := Install(Options{
		GOOS: "linux", Home: home, Exe: "/usr/local/bin/burnrate", Enable: false,
		XDGConfigHome: xdg, TelegramToken: token, TelegramChat: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantUnit := filepath.Join(xdg, "systemd", "user", "burnrate.service")
	if res.Path != wantUnit {
		t.Fatalf("path = %s", res.Path)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(xdg, "burnrate", "telegram.env")
	text := string(body)
	if strings.Contains(text, token) {
		t.Fatalf("unit contains the token: %s", text)
	}
	if !strings.Contains(text, "EnvironmentFile=-"+envFile) {
		t.Fatalf("unit = %s", text)
	}
	envBody, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(envBody) != "BURNRATE_TELEGRAM_TOKEN="+token+"\nBURNRATE_TELEGRAM_CHAT_ID=42\n" {
		t.Fatalf("env = %q", envBody)
	}
	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env mode = %o", info.Mode().Perm())
	}
}

func TestDarwinEnvFileNotInPlist(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "xdg")
	token := "123456:SECRETTOKEN"
	exe := "/Users/me/bin/burnrate & co"
	res, err := Install(Options{
		GOOS: "darwin", Home: home, Exe: exe, Enable: false,
		XDGConfigHome: xdg, TelegramToken: token, TelegramChat: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, token) {
		t.Fatalf("plist contains the token: %s", text)
	}
	if !strings.Contains(text, "burnrate &amp; co") || strings.Contains(text, "burnrate & co") {
		t.Fatalf("plist did not escape the path: %s", text)
	}
	if !strings.Contains(text, "<string>/bin/sh</string>") || !strings.Contains(text, "exec ") {
		t.Fatalf("plist = %s", text)
	}
	envFile := filepath.Join(xdg, "burnrate", "telegram.env")
	envBody, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envBody), "BURNRATE_TELEGRAM_TOKEN="+token+"\n") {
		t.Fatalf("env = %q", envBody)
	}
	if !strings.Contains(string(envBody), "BURNRATE_TELEGRAM_CHAT_ID=7\n") {
		t.Fatalf("env = %q", envBody)
	}
	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env mode = %o", info.Mode().Perm())
	}

	script, err := launchdScript(envFile, exe)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "fake-burnrate")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$BURNRATE_TELEGRAM_TOKEN\" \"$BURNRATE_TELEGRAM_CHAT_ID\" \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err = launchdScript(envFile, fake)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != token+" 7 agent" {
		t.Fatalf("script output = %q", out)
	}
}

func TestRejectTokenNewline(t *testing.T) {
	_, err := Install(Options{GOOS: "linux", Home: t.TempDir(), Exe: "/usr/bin/burnrate", TelegramToken: "a\nb"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectRelativeExe(t *testing.T) {
	if _, err := Install(Options{GOOS: "linux", Home: t.TempDir(), Exe: "burnrate"}); err == nil {
		t.Fatal("expected error")
	}
}
