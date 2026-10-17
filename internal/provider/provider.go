// Package provider is the extension point for a subscription-limit source.
//
// Codex and Claude collectors live in internal/collect. Cursor and Grok are
// phase 2 (opt-in, unofficial APIs) and ship as Disabled so a later
// implementation can be registered without changing the agent.
package provider

import (
	"context"
	"fmt"
	"time"
)

// Snapshot is one observation of a subscription quota.
// SourceKey and Origin stay on the machine. Alert and bot text must not include them.
type Snapshot struct {
	Provider   string
	ObservedAt time.Time
	LimitID    string
	PlanType   string
	SourceKey  string
	Origin     string
	Windows    []Window
}

// Window is one limit window inside a snapshot.
// Key is stable across primary/secondary slot swaps ("5h", "7d", "spend").
type Window struct {
	Key           string
	Slot          string
	UsedPercent   float64
	WindowMinutes int
	ResetsAt      time.Time
}

// TokenUsage is one Claude API response after streaming snapshots are collapsed.
type TokenUsage struct {
	Provider                 string
	MessageID                string
	ObservedAt               time.Time
	Model                    string
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// Sample is one used-percent observation inside a single window (same resets_at).
type Sample struct {
	At   time.Time
	Used float64
}

// Series is the latest window for one provider quota, with every sample that
// shares that window's resets_at.
type Series struct {
	Provider      string
	LimitID       string
	PlanType      string
	WindowKey     string
	Slot          string
	WindowMinutes int
	ResetsAt      time.Time
	Samples       []Sample
}

// Stats counts one collector pass.
type Stats struct {
	Files     int
	Snapshots int
	Tokens    int
	Skipped   int
	Malformed int
}

// Sink is how a collector persists data. *store.Store implements it.
type Sink interface {
	SaveSnapshot(ctx context.Context, snap Snapshot) error
	SaveTokens(ctx context.Context, usage TokenUsage) error
	Cursor(ctx context.Context, providerName, sourceKey string) (offset int64, fileID string, err error)
	SetCursor(ctx context.Context, providerName, sourceKey string, offset int64, fileID string) error
}

// Collector scans a local source. Implementations must not read credentials.
type Collector interface {
	Name() string
	Collect(ctx context.Context, sink Sink) (Stats, error)
}

// Disabled is a phase-2 placeholder. Collect performs no I/O.
type Disabled struct {
	ID string
}

func (d Disabled) Name() string {
	if d.ID == "" {
		return "disabled"
	}
	return d.ID
}

func (d Disabled) Collect(context.Context, Sink) (Stats, error) {
	return Stats{}, &NotImplementedError{Provider: d.Name()}
}

// NotImplementedError means the provider interface is wired and the
// implementation is intentionally absent.
type NotImplementedError struct {
	Provider string
}

func (e *NotImplementedError) Error() string {
	return e.Provider + " collector is not implemented (phase 2, opt-in unofficial API)"
}

// WindowKey labels a window by its length so a weekly window sitting in the
// primary slot is still "7d".
func WindowKey(minutes int) string {
	switch minutes {
	case 300:
		return "5h"
	case 10080:
		return "7d"
	default:
		if minutes <= 0 {
			return ""
		}
		return fmt.Sprintf("%dm", minutes)
	}
}

// DisplayName is the human label for a provider id.
func DisplayName(name string) string {
	switch name {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "cursor":
		return "Cursor"
	case "grok":
		return "Grok"
	default:
		if name == "" {
			return "unknown"
		}
		return name
	}
}
