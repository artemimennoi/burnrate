package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	t.Setenv("BURNRATE_CONFIG", "")
	t.Setenv("BURNRATE_TELEGRAM_TOKEN", "")
	t.Setenv("BURNRATE_TELEGRAM_CHAT_ID", "")
	t.Setenv("BURNRATE_TIMEZONE", "")
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Claude.Enabled || !cfg.Codex.Enabled || cfg.Cursor.Enabled || cfg.Grok.Enabled {
		t.Fatalf("enabled flags = claude %v codex %v cursor %v grok %v", cfg.Claude.Enabled, cfg.Codex.Enabled, cfg.Cursor.Enabled, cfg.Grok.Enabled)
	}
	if cfg.PollInterval != "30s" {
		t.Fatalf("poll = %q", cfg.PollInterval)
	}
	if len(cfg.Alerts.Thresholds) != 3 || cfg.Alerts.Thresholds[0] != 50 || cfg.Alerts.Thresholds[2] != 90 {
		t.Fatalf("thresholds = %#v", cfg.Alerts.Thresholds)
	}
	if cfg.TelegramConfigured() {
		t.Fatal("telegram should be unconfigured")
	}
}

func TestLoadFileAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
timezone = "UTC"
poll_interval = "15s"
[claude]
enabled = false
statusline_command = "  jq .  "
[codex]
enabled = true
sessions_dir = "~/codex-sessions"
[cursor]
enabled = true
[telegram]
token = "from-file"
chat_id = 5
[alerts]
thresholds = [90, 50, 50, 0, 75]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BURNRATE_CONFIG", "")
	t.Setenv("BURNRATE_TELEGRAM_TOKEN", "from-env")
	t.Setenv("BURNRATE_TELEGRAM_CHAT_ID", "42")
	t.Setenv("BURNRATE_TIMEZONE", "Europe/Minsk")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.Enabled || !cfg.Cursor.Enabled {
		t.Fatalf("claude=%v cursor=%v", cfg.Claude.Enabled, cfg.Cursor.Enabled)
	}
	if cfg.Claude.StatuslineCommand != "jq ." {
		t.Fatalf("command = %q", cfg.Claude.StatuslineCommand)
	}
	if cfg.Telegram.Token != "from-env" || cfg.Telegram.ChatID != 42 {
		t.Fatalf("telegram = %#v", cfg.Telegram)
	}
	if cfg.Timezone != "Europe/Minsk" {
		t.Fatalf("tz = %q", cfg.Timezone)
	}
	want := []int{50, 75, 90}
	if len(cfg.Alerts.Thresholds) != len(want) {
		t.Fatalf("thresholds = %#v", cfg.Alerts.Thresholds)
	}
	for i := range want {
		if cfg.Alerts.Thresholds[i] != want[i] {
			t.Fatalf("thresholds = %#v", cfg.Alerts.Thresholds)
		}
	}
	loc, err := cfg.Location()
	if err != nil {
		t.Fatal(err)
	}
	if loc.String() != "Europe/Minsk" {
		t.Fatalf("loc = %s", loc)
	}
	d, err := cfg.Interval()
	if err != nil || d.String() != "15s" {
		t.Fatalf("interval = %s err=%v", d, err)
	}
	sessions, err := cfg.ResolvedSessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if sessions != filepath.Join(home, "codex-sessions") {
		t.Fatalf("sessions = %s", sessions)
	}
}

func TestExplicitMissingConfigErrors(t *testing.T) {
	t.Setenv("BURNRATE_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))
	if _, err := Load(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestExampleConfig(t *testing.T) {
	t.Setenv("BURNRATE_CONFIG", "")
	t.Setenv("BURNRATE_TELEGRAM_TOKEN", "")
	t.Setenv("BURNRATE_TELEGRAM_CHAT_ID", "")
	t.Setenv("BURNRATE_TIMEZONE", "")
	cfg, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timezone != "Europe/Minsk" || cfg.Cursor.Enabled || cfg.Grok.Enabled {
		t.Fatalf("example = tz %q cursor %v grok %v", cfg.Timezone, cfg.Cursor.Enabled, cfg.Grok.Enabled)
	}
	if cfg.Telegram.Token != "" || cfg.Telegram.ChatID != 0 {
		t.Fatal("example config must not contain a telegram secret")
	}
	if !cfg.Claude.Enabled || !cfg.Codex.Enabled {
		t.Fatal("example should enable claude and codex")
	}
}

func TestBadChatID(t *testing.T) {
	t.Setenv("BURNRATE_CONFIG", "")
	t.Setenv("BURNRATE_TELEGRAM_CHAT_ID", "nope")
	t.Setenv("BURNRATE_TELEGRAM_TOKEN", "")
	t.Setenv("BURNRATE_TIMEZONE", "")
	_, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err == nil {
		t.Fatal("expected chat id error")
	}
	if strings.Contains(err.Error(), "nope") {
		t.Fatalf("error echoed the env value: %v", err)
	}
}

func TestIntervalZeroIsNotAnAgentInterval(t *testing.T) {
	cfg := Config{PollInterval: "0s"}
	d, err := cfg.Interval()
	if err != nil || d != 0 {
		t.Fatalf("interval = %s err %v", d, err)
	}
	if _, err := cfg.AgentInterval(); err == nil {
		t.Fatal("agent accepted 0s")
	}
	cfg.PollInterval = "30s"
	if _, err := cfg.AgentInterval(); err != nil {
		t.Fatal(err)
	}
	cfg.PollInterval = "-1s"
	if _, err := cfg.Interval(); err == nil {
		t.Fatal("negative interval accepted")
	}
}

func TestRetentionDefault(t *testing.T) {
	if got := (Config{}).Retention(); got != 90*24*time.Hour {
		t.Fatalf("retention = %s", got)
	}
	if got := (Config{RetentionDays: 7}).Retention(); got != 7*24*time.Hour {
		t.Fatalf("retention = %s", got)
	}
}

func TestRecoverStatuslineCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	body := "poll_interval = [\nstatusline_command = \"printf no\"\n[codex]\nstatusline_command = \"printf also-no\"\n[claude]\nstatusline_command = \"printf hi\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RecoverStatuslineCommand(path); got != "printf hi" {
		t.Fatalf("recovered = %q", got)
	}
	outside := filepath.Join(t.TempDir(), "outside.toml")
	if err := os.WriteFile(outside, []byte("statusline_command = \"printf no\"\n[codex]\nstatusline_command = 'printf no'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RecoverStatuslineCommand(outside); got != "" {
		t.Fatalf("outside [claude] recovered %q", got)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("broken config parsed")
	}
}

func TestWarnConfigPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("timezone = \"UTC\"\n[telegram]\ntoken = \"sekrit\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	WarnConfigPerms(path, &b)
	if b.Len() == 0 {
		t.Fatal("expected a permission warning")
	}
	if strings.Contains(b.String(), "sekrit") {
		t.Fatalf("warning included the token: %s", b.String())
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	WarnConfigPerms(path, &b)
	if b.Len() != 0 {
		t.Fatalf("private file warned: %s", b.String())
	}
	if err := os.WriteFile(path, []byte("[telegram]\ntoken = \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	WarnConfigPerms(path, &b)
	if b.Len() != 0 {
		t.Fatalf("empty token warned: %s", b.String())
	}
	if err := os.WriteFile(path, []byte("timezone = \"UTC\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	WarnConfigPerms(path, &b)
	if b.Len() != 0 {
		t.Fatalf("tokenless file warned: %s", b.String())
	}
}
