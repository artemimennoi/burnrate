// Package config loads ~/.config/burnrate/config.toml and environment overrides.
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk configuration plus environment overrides.
type Config struct {
	Timezone      string   `toml:"timezone"`
	PollInterval  string   `toml:"poll_interval"`
	RetentionDays int      `toml:"retention_days"`
	DBPath        string   `toml:"db_path"`
	Claude        Claude   `toml:"claude"`
	Codex         Codex    `toml:"codex"`
	Cursor        Toggle   `toml:"cursor"`
	Grok          Toggle   `toml:"grok"`
	Telegram      Telegram `toml:"telegram"`
	Alerts        Alerts   `toml:"alerts"`
}

// Claude configures the statusline wrapper and transcript token scan.
type Claude struct {
	Enabled           bool   `toml:"enabled"`
	ProjectsDir       string `toml:"projects_dir"`
	StatuslineCommand string `toml:"statusline_command"`
}

// Codex configures the rollout scan.
type Codex struct {
	Enabled     bool   `toml:"enabled"`
	SessionsDir string `toml:"sessions_dir"`
}

// Toggle is a phase-2 provider that is off unless the user opts in.
type Toggle struct {
	Enabled bool `toml:"enabled"`
}

// Telegram holds the bot token and the only chat allowed to receive messages.
type Telegram struct {
	Token  string `toml:"token"`
	ChatID int64  `toml:"chat_id"`
}

// Alerts lists integer percent thresholds.
type Alerts struct {
	Thresholds []int `toml:"thresholds"`
}

func defaultConfig() Config {
	return Config{
		PollInterval: "30s",
		Claude:       Claude{Enabled: true},
		Codex:        Codex{Enabled: true},
		Alerts:       Alerts{Thresholds: []int{50, 75, 90}},
	}
}

// DefaultConfigPath is $BURNRATE_CONFIG, otherwise $XDG_CONFIG_HOME/burnrate/config.toml
// or ~/.config/burnrate/config.toml.
func DefaultConfigPath() (string, error) {
	if v := os.Getenv("BURNRATE_CONFIG"); v != "" {
		return v, nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "burnrate", "config.toml"), nil
}

// DefaultDBPath is $XDG_DATA_HOME/burnrate/burnrate.db or ~/.local/share/burnrate/burnrate.db.
func DefaultDBPath() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "burnrate", "burnrate.db"), nil
}

// DefaultClaudeProjects is ~/.claude/projects.
func DefaultClaudeProjects() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// DefaultCodexSessions is ~/.codex/sessions.
func DefaultCodexSessions() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// WarnConfigPerms writes a warning when path is group- or world-readable and
// the file itself contains a non-empty telegram token. A token that exists
// only in the environment does not warn. The config file is not modified.
func WarnConfigPerms(path string, w io.Writer) {
	if w == nil || path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 == 0 {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var probe struct {
		Telegram struct {
			Token string `toml:"token"`
		} `toml:"telegram"`
	}
	if err := toml.Unmarshal(b, &probe); err != nil {
		return
	}
	if strings.TrimSpace(probe.Telegram.Token) == "" {
		return
	}
	fmt.Fprintf(w, "burnrate: config %s is readable by group or others and contains a Telegram token; chmod 600 is safer\n", path)
}

// Load reads path. A missing default path yields defaults. A missing
// $BURNRATE_CONFIG path is an error. Environment variables override the file.
func Load(path string) (Config, error) {
	cfg := defaultConfig()
	explicit := os.Getenv("BURNRATE_CONFIG") != ""
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return Config{}, err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			cfg.normalize()
			if err := applyEnv(&cfg); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.normalize()
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) normalize() {
	if strings.TrimSpace(c.PollInterval) == "" {
		c.PollInterval = "30s"
	}
	c.Telegram.Token = strings.TrimSpace(c.Telegram.Token)
	c.Timezone = strings.TrimSpace(c.Timezone)
	c.Claude.StatuslineCommand = strings.TrimSpace(c.Claude.StatuslineCommand)
	seen := map[int]bool{}
	var thresholds []int
	for _, th := range c.Alerts.Thresholds {
		if th <= 0 || th > 100 || seen[th] {
			continue
		}
		seen[th] = true
		thresholds = append(thresholds, th)
	}
	if len(thresholds) == 0 {
		thresholds = []int{50, 75, 90}
	}
	sort.Ints(thresholds)
	c.Alerts.Thresholds = thresholds
}

func applyEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("BURNRATE_TELEGRAM_TOKEN"); ok {
		cfg.Telegram.Token = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("BURNRATE_TELEGRAM_CHAT_ID"); ok && strings.TrimSpace(v) != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return fmt.Errorf("BURNRATE_TELEGRAM_CHAT_ID must be an integer")
		}
		cfg.Telegram.ChatID = id
	}
	if v, ok := os.LookupEnv("BURNRATE_TIMEZONE"); ok && strings.TrimSpace(v) != "" {
		cfg.Timezone = strings.TrimSpace(v)
	}
	return nil
}

// Location returns the configured zone, or the system zone when unset.
func (c Config) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	return loc, nil
}

// Interval parses poll_interval. Zero and other short values are allowed here;
// the agent is the command that requires at least one second.
func (c Config) Interval() (time.Duration, error) {
	if strings.TrimSpace(c.PollInterval) == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(c.PollInterval)
	if err != nil {
		return 0, fmt.Errorf("poll_interval is not a valid duration")
	}
	if d < 0 {
		return 0, fmt.Errorf("poll_interval must not be negative")
	}
	return d, nil
}

// AgentInterval is Interval plus the one-second minimum used by the agent.
func (c Config) AgentInterval() (time.Duration, error) {
	d, err := c.Interval()
	if err != nil {
		return 0, err
	}
	if d < time.Second {
		return 0, fmt.Errorf("poll_interval must be at least 1s")
	}
	return d, nil
}

// Retention is how long snapshots are kept. Unset or non-positive uses 90 days.
func (c Config) Retention() time.Duration {
	days := c.RetentionDays
	if days <= 0 {
		days = 90
	}
	if days > 3650 {
		days = 3650
	}
	return time.Duration(days) * 24 * time.Hour
}

// RecoverStatuslineCommand reads a statusline_command assignment from a config
// file even when the rest of the file does not parse.
func RecoverStatuslineCommand(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "statusline_command") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "statusline_command"))
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		return unquoteTOML(strings.TrimSpace(rest[1:]))
	}
	return ""
}

func unquoteTOML(v string) string {
	if len(v) >= 2 {
		if v[0] == '"' && v[len(v)-1] == '"' {
			s, err := strconv.Unquote(v)
			if err == nil {
				return strings.TrimSpace(s)
			}
		}
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return strings.TrimSpace(v)
}

// TelegramConfigured reports whether alerts and the bot can run.
func (c Config) TelegramConfigured() bool {
	return c.Telegram.Token != "" && c.Telegram.ChatID != 0
}

// ResolvedDBPath applies the default and a leading ~/ .
func (c Config) ResolvedDBPath() (string, error) {
	if strings.TrimSpace(c.DBPath) == "" {
		return DefaultDBPath()
	}
	return expandHome(c.DBPath)
}

// ResolvedProjectsDir is the Claude transcript root.
func (c Config) ResolvedProjectsDir() (string, error) {
	if strings.TrimSpace(c.Claude.ProjectsDir) == "" {
		return DefaultClaudeProjects()
	}
	return expandHome(c.Claude.ProjectsDir)
}

// ResolvedSessionsDir is the Codex session root.
func (c Config) ResolvedSessionsDir() (string, error) {
	if strings.TrimSpace(c.Codex.SessionsDir) == "" {
		return DefaultCodexSessions()
	}
	return expandHome(c.Codex.SessionsDir)
}

func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
