// Package claude reads Claude Code subscription windows and transcript token counts.
//
// Percentages come from the statusline JSON (rate_limits.five_hour and
// rate_limits.seven_day: used_percentage, resets_at). Token counts come from
// ~/.claude/projects/**/*.jsonl usage fields. This package does not read OAuth
// tokens and does not call api/oauth/usage.
package claude

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"
)

type rateWindow struct {
	UsedPercentage *float64        `json:"used_percentage"`
	ResetsAt       json.RawMessage `json:"resets_at"`
	Period         string          `json:"period"`
}

type statusDoc struct {
	RateLimits *struct {
		FiveHour   *rateWindow `json:"five_hour"`
		SevenDay   *rateWindow `json:"seven_day"`
		SpendLimit *rateWindow `json:"spend_limit"`
	} `json:"rate_limits"`
}

// ParseStatusline extracts subscription windows from a statusline payload.
// A payload with no rate-limit windows returns a zero snapshot and a nil error.
// Invalid JSON returns an error. Workspace paths and message text are ignored.
func ParseStatusline(raw []byte, observed time.Time) (provider.Snapshot, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return provider.Snapshot{}, nil
	}
	var doc statusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return provider.Snapshot{}, err
	}
	snap := provider.Snapshot{
		Provider:   "claude",
		ObservedAt: observed.UTC(),
		LimitID:    "subscription",
	}
	if doc.RateLimits == nil {
		return snap, nil
	}
	if w, ok := convertStatusWindow(doc.RateLimits.FiveHour, "five_hour", "5h", 300); ok {
		snap.Windows = append(snap.Windows, w)
	}
	if w, ok := convertStatusWindow(doc.RateLimits.SevenDay, "seven_day", "7d", 10080); ok {
		snap.Windows = append(snap.Windows, w)
	}
	if w, ok := convertSpend(doc.RateLimits.SpendLimit); ok {
		snap.Windows = append(snap.Windows, w)
	}
	if len(snap.Windows) == 0 {
		snap.LimitID = ""
	}
	return snap, nil
}

func convertStatusWindow(raw *rateWindow, slot, key string, minutes int) (provider.Window, bool) {
	if raw == nil || raw.UsedPercentage == nil {
		return provider.Window{}, false
	}
	resets, ok := parseResetsAt(raw.ResetsAt)
	if !ok {
		return provider.Window{}, false
	}
	used, ok := provider.CleanPercent(*raw.UsedPercentage)
	if !ok {
		return provider.Window{}, false
	}
	return provider.Window{
		Key:           key,
		Slot:          slot,
		UsedPercent:   used,
		WindowMinutes: minutes,
		ResetsAt:      resets,
	}, true
}

func convertSpend(raw *rateWindow) (provider.Window, bool) {
	if raw == nil || raw.UsedPercentage == nil {
		return provider.Window{}, false
	}
	resets, ok := parseResetsAt(raw.ResetsAt)
	if !ok {
		return provider.Window{}, false
	}
	used, ok := provider.CleanPercent(*raw.UsedPercentage)
	if !ok {
		return provider.Window{}, false
	}
	minutes := 0
	switch raw.Period {
	case "daily":
		minutes = 1440
	case "weekly":
		minutes = 10080
	}
	return provider.Window{
		Key:           "spend",
		Slot:          "spend_limit",
		UsedPercent:   used,
		WindowMinutes: minutes,
		ResetsAt:      resets,
	}, true
}

func parseResetsAt(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(n), 0).UTC(), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return time.Time{}, false
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 0 {
		return time.Unix(sec, 0).UTC(), true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

type usageDoc struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseUsage extracts one assistant usage record. Streaming snapshots that
// share a message id are collapsed later by keeping the larger output_tokens.
// Lines without usage, including user prompts, return ok=false.
func ParseUsage(line []byte) (provider.TokenUsage, bool) {
	var doc usageDoc
	if err := json.Unmarshal(line, &doc); err != nil {
		return provider.TokenUsage{}, false
	}
	if doc.Type != "assistant" || doc.Message == nil || doc.Message.Usage == nil {
		return provider.TokenUsage{}, false
	}
	id := doc.Message.ID
	if id == "" {
		id = doc.UUID
	}
	if id == "" {
		return provider.TokenUsage{}, false
	}
	observed, _ := parseTimestamp(doc.Timestamp)
	u := doc.Message.Usage
	return provider.TokenUsage{
		Provider:                 "claude",
		MessageID:                id,
		ObservedAt:               observed,
		Model:                    doc.Message.Model,
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}, true
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
