// Package install writes a launchd plist or a systemd user unit.
package install

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const label = "com.artemimennoi.burnrate"

// Options selects the unit to write.
type Options struct {
	GOOS   string
	Home   string
	Exe    string
	Enable bool
}

// Result is the unit that was written.
type Result struct {
	Path string
	Note string
}

// Install writes the service file for GOOS. When Enable is set it loads the
// unit. A failure to load still leaves the file in place; Note explains how
// to load it by hand.
func Install(opts Options) (Result, error) {
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.Home == "" {
		return Result{}, fmt.Errorf("home directory is required")
	}
	if !filepath.IsAbs(opts.Exe) {
		return Result{}, fmt.Errorf("executable path must be absolute")
	}
	rel, body, err := render(opts.GOOS, opts.Home, opts.Exe)
	if err != nil {
		return Result{}, err
	}
	path := filepath.Join(opts.Home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, err
	}
	if opts.GOOS == "darwin" {
		if err := os.MkdirAll(filepath.Join(opts.Home, "Library", "Logs"), 0o755); err != nil {
			return Result{}, err
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return Result{}, err
	}
	res := Result{Path: path, Note: manual(opts.GOOS)}
	if !opts.Enable {
		return res, nil
	}
	if err := enable(opts.GOOS, path); err != nil {
		return res, err
	}
	res.Note = "service enabled"
	return res, nil
}

func render(goos, home, exe string) (rel, body string, err error) {
	switch goos {
	case "linux":
		execLine, err := quoteExec(exe)
		if err != nil {
			return "", "", err
		}
		body = "[Unit]\nDescription=burnrate AI subscription limit tracker\n\n[Service]\nType=simple\nExecStart=" + execLine + " agent\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n"
		return filepath.Join(".config", "systemd", "user", "burnrate.service"), body, nil
	case "darwin":
		out := filepath.Join(home, "Library", "Logs", "burnrate.out.log")
		errPath := filepath.Join(home, "Library", "Logs", "burnrate.err.log")
		body = plist(exe, out, errPath)
		return filepath.Join("Library", "LaunchAgents", label+".plist"), body, nil
	default:
		return "", "", fmt.Errorf("unsupported OS %s", goos)
	}
}

func quoteExec(path string) (string, error) {
	if strings.ContainsAny(path, "\n\r") {
		return "", fmt.Errorf("executable path cannot contain newlines")
	}
	if strings.ContainsAny(path, " \t\"'\\") {
		return strconv.Quote(path), nil
	}
	return path, nil
}

func plist(exe, stdout, stderr string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writeKey(&b, "Label", label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	b.WriteString("\t\t<string>" + xmlEscape(exe) + "</string>\n")
	b.WriteString("\t\t<string>agent</string>\n")
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	writeKey(&b, "StandardOutPath", stdout)
	writeKey(&b, "StandardErrorPath", stderr)
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func writeKey(b *strings.Builder, key, value string) {
	b.WriteString("\t<key>" + xmlEscape(key) + "</key>\n")
	b.WriteString("\t<string>" + xmlEscape(value) + "</string>\n")
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func manual(goos string) string {
	switch goos {
	case "linux":
		return "systemctl --user daemon-reload && systemctl --user enable --now burnrate.service"
	case "darwin":
		return "launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/" + label + ".plist"
	default:
		return ""
	}
}

func enable(goos, unitPath string) error {
	switch goos {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return fmt.Errorf("systemctl not found; %s", manual(goos))
		}
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", "burnrate.service").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl enable: %s", strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return fmt.Errorf("launchctl not found; %s", manual(goos))
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
		if out, err := exec.Command("launchctl", "bootstrap", domain, unitPath).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unsupported OS %s", goos)
	}
}
