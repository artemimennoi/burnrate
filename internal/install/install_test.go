package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if strings.Contains(res.Note, "enable --now") || !strings.Contains(res.Note, "restart") {
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
	if !strings.Contains(res.Note, "bootout") || !strings.Contains(res.Note, "kickstart -k") {
		t.Fatalf("note = %s", res.Note)
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

func TestReinstallDropsToken(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "xdg")
	opts := Options{
		GOOS: "linux", Home: home, Exe: "/usr/local/bin/burnrate", Enable: false,
		XDGConfigHome: xdg, TelegramToken: "123:SECRET", TelegramChat: 9,
	}
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(xdg, "burnrate", "telegram.env")
	body, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "BURNRATE_TELEGRAM_TOKEN=123:SECRET\n") {
		t.Fatalf("env = %q", body)
	}
	opts.TelegramToken = ""
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRET") || !strings.Contains(string(body), "BURNRATE_TELEGRAM_CHAT_ID=9\n") {
		t.Fatalf("env after refresh = %q", body)
	}
	opts.TelegramChat = 0
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("telegram.env still present: %v", err)
	}
}

func TestSystemdEnvFileAllowsSpaces(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "x dg")
	res, err := Install(Options{
		GOOS: "linux", Home: home, Exe: "/usr/local/bin/burnrate", Enable: false,
		XDGConfigHome: xdg, TelegramToken: "123:SECRET", TelegramChat: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "123:SECRET") {
		t.Fatalf("unit contains the token: %s", text)
	}
	if strings.Contains(text, `EnvironmentFile="`) || strings.Contains(text, `EnvironmentFile=-"`) || strings.Contains(text, `\x20`) {
		t.Fatalf("EnvironmentFile is quoted or hex-escaped: %s", text)
	}
	var envLine string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "EnvironmentFile=") {
			envLine = line
		}
	}
	parsed, optional, err := parseEnvironmentFileValue(envLine)
	if err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(xdg, "burnrate", "telegram.env")
	if !optional || parsed != envFile {
		t.Fatalf("parsed %q optional %v, want %s", parsed, optional, envFile)
	}
	percent := "/tmp/a%b c"
	escaped, err := systemdPath(percent)
	if err != nil {
		t.Fatal(err)
	}
	if escaped != "/tmp/a%%b c" {
		t.Fatalf("escaped = %q", escaped)
	}
	back, optional, err := parseEnvironmentFileValue("EnvironmentFile=-" + escaped)
	if err != nil || !optional || back != percent {
		t.Fatalf("round trip %q %v %v", back, optional, err)
	}
	bin, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	for _, args := range [][]string{
		{bin, "verify", res.Path},
		{bin, "--user", "verify", res.Path},
	} {
		out, _ := exec.Command(args[0], args[1:]...).CombinedOutput()
		if strings.Contains(string(out), "not absolute") {
			t.Fatalf("%v\n%s\nunit:\n%s", args, out, text)
		}
	}
}

func TestEnableRestartsLinuxService(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(bin, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(logPath) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := enable("linux", filepath.Join(t.TempDir(), "burnrate.service")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "enable --now") || !strings.Contains(text, "--user restart burnrate.service") {
		t.Fatalf("commands = %q", text)
	}
	if !strings.Contains(text, "--user enable burnrate.service") || !strings.Contains(text, "--user daemon-reload") {
		t.Fatalf("commands = %q", text)
	}
}

func TestEnableKickstartsDarwin(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(bin, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(logPath) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := enable("darwin", filepath.Join(t.TempDir(), "agent.plist")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "bootout ") || !strings.Contains(text, "bootstrap ") || !strings.Contains(text, "kickstart -k ") {
		t.Fatalf("commands = %q", text)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(logPath) + "\ncase \"$*\" in *bootstrap*) exit 1 ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := enable("darwin", filepath.Join(t.TempDir(), "agent.plist")); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text = string(got)
	if !strings.Contains(text, "bootout ") || !strings.Contains(text, "bootstrap ") || !strings.Contains(text, "kickstart -k ") {
		t.Fatalf("reload commands = %q", text)
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
