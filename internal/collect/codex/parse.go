// Package codex reads subscription windows from Codex rollout JSONL.
//
// A token_count event carries payload.rate_limits with primary and secondary
// windows (used_percent, window_minutes, resets_at). Snapshots whose windows
// are null — the limit_id "premium" case — are skipped and are never stored
// as 0%.
package codex

import (
	"encoding/json"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"
)

// Kind classifies one rollout line.
type Kind int

const (
	// KindIgnore is a line that is not a rate-limit snapshot.
	KindIgnore Kind = iota
	// KindSkipNull is a token_count whose windows are missing or null.
	KindSkipNull
	// KindSnapshot is a token_count with at least one real window.
	KindSnapshot
)

// Result is the parse of one JSONL line.
type Result struct {
	Kind     Kind
	Snapshot provider.Snapshot
}

type rawWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *float64 `json:"window_minutes"`
	ResetsAt      *float64 `json:"resets_at"`
}

type rawLimits struct {
	LimitID   string     `json:"limit_id"`
	PlanType  string     `json:"plan_type"`
	Primary   *rawWindow `json:"primary"`
	Secondary *rawWindow `json:"secondary"`
}

type envelope struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   *struct {
		Type       string          `json:"type"`
		RateLimits json.RawMessage `json:"rate_limits"`
	} `json:"payload"`
}

// ParseLine parses one rollout line. Malformed JSON is KindIgnore so a bad
// line does not stop the scan.
func ParseLine(line []byte) Result {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Result{Kind: KindIgnore}
	}
	if env.Type != "event_msg" || env.Payload == nil || env.Payload.Type != "token_count" {
		return Result{Kind: KindIgnore}
	}
	if len(env.Payload.RateLimits) == 0 || string(env.Payload.RateLimits) == "null" {
		return Result{Kind: KindSkipNull}
	}
	var limits rawLimits
	if err := json.Unmarshal(env.Payload.RateLimits, &limits); err != nil {
		return Result{Kind: KindIgnore}
	}
	observed, ok := parseTimestamp(env.Timestamp)
	if !ok {
		return Result{Kind: KindSkipNull}
	}
	var windows []provider.Window
	if w, ok := convertWindow(limits.Primary, "primary"); ok {
		windows = append(windows, w)
	}
	if w, ok := convertWindow(limits.Secondary, "secondary"); ok {
		windows = appendWindow(windows, w)
	}
	if len(windows) == 0 {
		return Result{Kind: KindSkipNull}
	}
	limitID := limits.LimitID
	if limitID == "" {
		limitID = "codex"
	}
	return Result{
		Kind: KindSnapshot,
		Snapshot: provider.Snapshot{
			Provider:   "codex",
			ObservedAt: observed,
			LimitID:    limitID,
			PlanType:   limits.PlanType,
			Windows:    windows,
		},
	}
}

func convertWindow(raw *rawWindow, slot string) (provider.Window, bool) {
	if raw == nil || raw.UsedPercent == nil || raw.WindowMinutes == nil || raw.ResetsAt == nil {
		return provider.Window{}, false
	}
	minutes := int(*raw.WindowMinutes)
	if minutes <= 0 || *raw.ResetsAt <= 0 {
		return provider.Window{}, false
	}
	key := provider.WindowKey(minutes)
	if key == "" {
		return provider.Window{}, false
	}
	used, ok := provider.CleanPercent(*raw.UsedPercent)
	if !ok {
		return provider.Window{}, false
	}
	return provider.Window{
		Key:           key,
		Slot:          slot,
		UsedPercent:   used,
		WindowMinutes: minutes,
		ResetsAt:      time.Unix(int64(*raw.ResetsAt), 0).UTC(),
	}, true
}

func appendWindow(windows []provider.Window, w provider.Window) []provider.Window {
	for _, existing := range windows {
		if existing.Key == w.Key {
			w.Key = w.Key + ":" + w.Slot
			break
		}
	}
	return append(windows, w)
}

func parseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
