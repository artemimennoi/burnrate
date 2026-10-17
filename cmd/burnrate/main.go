package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
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
		return cmdAgent()
	case "status":
		return cmdReport(false)
	case "forecast":
		return cmdReport(true)
	case "statusline":
		return cmdStatusline()
	case "install":
		return cmdInstall()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
}

func logger() *log.Logger {
	return log.New(os.Stderr, "burnrate: ", log.LstdFlags)
}

func openApp() (config.Config, *store.Store, error) {
	cfg, err := config.Load("")
	if err != nil {
		return config.Config{}, nil, err
	}
	path, err := cfg.ResolvedDBPath()
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.Open(path)
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
	collectors, err := agent.Collectors(cfg)
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
		Log:        logger(),
	}
	if withTelegram && cfg.TelegramConfigured() {
		ag.Telegram = &telegram.Client{Token: cfg.Telegram.Token}
	}
	return ag, nil
}

func cmdAgent() int {
	cfg, st, err := openApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		return 1
	}
	defer st.Close()
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

func cmdReport(forecast bool) int {
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

func cmdStatusline() int {
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
		// Still try to run the wrapped command if we can see one. Without a
		// config there is no command, so stop.
		return 1
	}
	var save func(provider.Snapshot) error
	if path, err := cfg.ResolvedDBPath(); err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
	} else if st, err := store.Open(path); err != nil {
		fmt.Fprintf(os.Stderr, "burnrate: %v\n", err)
	} else {
		defer st.Close()
		save = func(snap provider.Snapshot) error {
			return st.SaveSnapshot(context.Background(), snap)
		}
	}
	err = claude.Run(claude.Options{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Command: cfg.Claude.StatuslineCommand,
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

func cmdInstall() int {
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
	res, err := install.Install(install.Options{
		Home:   home,
		Exe:    exe,
		Enable: true,
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
