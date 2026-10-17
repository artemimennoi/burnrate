package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/artemimennoi/burnrate/internal/agent"
	"github.com/artemimennoi/burnrate/internal/collect/claude"
	"github.com/artemimennoi/burnrate/internal/config"
	"github.com/artemimennoi/burnrate/internal/install"
	"github.com/artemimennoi/burnrate/internal/provider"
	"github.com/artemimennoi/burnrate/internal/store"
	"github.com/artemimennoi/burnrate/internal/telegram"
)

const version = "0.1.0"

const usageText = `burnrate tracks how fast AI subscription limits get used.

Usage:
  burnrate agent        run collectors, alerts, and the Telegram bot
  burnrate status       scan local logs and print current windows
  burnrate forecast     scan local logs and print pace and hit time
  burnrate statusline   record a Claude statusline snapshot and run the wrapped command
  burnrate install      install a systemd user unit or a launchd agent
  burnrate version      print the version

Config: ~/.config/burnrate/config.toml (or $BURNRATE_CONFIG)
Telegram: BURNRATE_TELEGRAM_TOKEN and BURNRATE_TELEGRAM_CHAT_ID
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usageText)
		return 0
	case "version", "-v", "--version":
		fmt.Println("burnrate " + version)
		return 0
	case "agent":
		return cmdAgent(args[1:])
	case "status":
		return cmdReport(args[1:], false)
	case "forecast":
		return cmdReport(args[1:], true)
	case "statusline":
		return cmdStatusline(args[1:])
	case "install":
		return cmdInstall(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
}

// parseArgs rejects unknown flags and extra arguments. --help prints usage and
// returns handled=true with code 0.
func parseArgs(name string, args []string) (int, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, usageText)
			return 0, true
		}
		fmt.Fprintf(os.Stderr, "burnrate: %s: %s\n", name, flagError(err))
		return 2, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "burnrate: %s: unexpected argument %q\n%s", name, fs.Arg(0), usageText)
		return 2, true
	}
	return 0, false
}

func flagError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "flag needs an argument: ")
	return msg
}

func logger() *log.Logger {
	return log.New(os.Stderr, "burnrate: ", log.LstdFlags)
}

func loadConfig() (config.Config, string, error) {
	path, err := config.DefaultConfigPath()
	if err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, path, err
	}
	return cfg, path, nil
}

func openApp() (config.Config, *store.Store, error) {
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		return config.Config{}, nil, err
	}
	config.WarnConfigPerms(cfgPath, os.Stderr)
	path, err := cfg.ResolvedDBPath()
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.OpenOptions(path, store.Options{Retention: cfg.Retention()})
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, st, nil
}

func newAgent(cfg config.Config, st *store.Store, withTelegram bool) (*agent.Agent, error) {
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	lg := logger()
	collectors, err := agent.Collectors(cfg, lg)
	if err != nil {
		return nil, err
	}
	interval, err := cfg.Interval()
	if err != nil {
		return nil, err
	}
	ag := &agent.Agent{
		Collectors: collectors,
		Store:      st,
		ChatID:     cfg.Telegram.ChatID,
		Thresholds: cfg.Alerts.Thresholds,
		Location:   loc,
		Interval:   interval,
		Log:        lg,
	}
	if withTelegram && cfg.TelegramConfigured() {
		ag.Telegram = &telegram.Client{Token: cfg.Telegram.Token}
	}
	return ag, nil
}

func cmdAgent(args []string) int {
	if code, handled := parseArgs("agent", args); handled {
		return code
	}
	cfg, st, err := openApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	defer st.Close()
	if _, err := cfg.AgentInterval(); err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	ag, err := newAgent(cfg, st, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := ag.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	return 0
}

func cmdReport(args []string, forecast bool) int {
	name := "status"
	if forecast {
		name = "forecast"
	}
	if code, handled := parseArgs(name, args); handled {
		return code
	}
	cfg, st, err := openApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	defer st.Close()
	ag, err := newAgent(cfg, st, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	ag.Scan(context.Background())
	var text string
	if forecast {
		text, err = ag.Forecast(context.Background())
	} else {
		text, err = ag.Status(context.Background())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	fmt.Println(text)
	return 0
}

func cmdStatusline(args []string) int {
	if os.Getenv(claude.StatuslineDepthEnv) != "" {
		return 0
	}
	if code, handled := parseArgs("statusline", args); handled {
		return code
	}
	cfg, cfgPath, err := loadConfig()
	command := ""
	var save func(provider.Snapshot) error
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		command = config.RecoverStatuslineCommand(cfgPath)
		if command == "" {
			return 0
		}
	} else {
		config.WarnConfigPerms(cfgPath, os.Stderr)
		command = cfg.Claude.StatuslineCommand
		if path, err := cfg.ResolvedDBPath(); err != nil {
			fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		} else if st, err := store.OpenOptions(path, store.Options{BusyTimeout: store.StatuslineBusyTimeout, Retention: cfg.Retention()}); err != nil {
			fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		} else {
			defer st.Close()
			save = func(snap provider.Snapshot) error {
				return st.SaveSnapshot(context.Background(), snap)
			}
		}
	}
	err = claude.Run(claude.Options{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Command: command,
		Save:    save,
	})
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
	return 1
}

func cmdInstall(args []string) int {
	if code, handled := parseArgs("install", args); handled {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	config.WarnConfigPerms(cfgPath, os.Stderr)
	res, err := install.Install(install.Options{
		Home:          home,
		Exe:           exe,
		Enable:        true,
		XDGConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		TelegramToken: cfg.Telegram.Token,
		TelegramChat:  cfg.Telegram.ChatID,
	})
	if res.Path != "" {
		fmt.Printf("wrote %s\n", res.Path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		if res.Note != "" {
			fmt.Fprintln(os.Stderr, res.Note)
		}
		return 1
	}
	if res.Note != "" {
		fmt.Println(res.Note)
	}
	return 0
}
