// Package analysis turns stored window samples into a pace and a limit forecast.
package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"
)

// Kind classifies a forecast.
type Kind int

const (
	// KindInsufficient means there is not enough history to estimate a pace.
	KindInsufficient Kind = iota
	// KindFlat means usage is not rising.
	KindFlat
	// KindHit means the current pace reaches 100% before the window resets.
	KindHit
	// KindAfterReset means the pace is positive but the window resets first.
	KindAfterReset
	// KindReached means the latest sample is already at the limit.
	KindReached
	// KindStale means resets_at is in the past, so the percent is not current.
	KindStale
	// KindUnknown means a positive pace projects a hit time that is already past.
	KindUnknown
)

// Forecast is the pace of one window.
type Forecast struct {
	Kind        Kind
	PacePerHour float64
	HitAt       time.Time
}

// View is a window plus the forecast at a given moment.
type View struct {
	Series   provider.Series
	Forecast Forecast
	Used     float64
	Observed time.Time
}

// Evaluate forecasts one series at now.
// Pace is the change from the earliest sample in this window to the latest.
// A single sample assumes 0% at the start of the window (resets_at minus the
// window length).
func Evaluate(series provider.Series, now time.Time) View {
	samples := append([]provider.Sample(nil), series.Samples...)
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].At.Before(samples[j].At)
	})
	view := View{Series: series, Forecast: Forecast{Kind: KindInsufficient}}
	if len(samples) == 0 {
		return view
	}
	last := samples[len(samples)-1]
	view.Used = last.Used
	view.Observed = last.At
	if !series.ResetsAt.IsZero() && !series.ResetsAt.After(now) {
		view.Forecast = Forecast{Kind: KindStale}
		return view
	}
	if last.Used >= 100 {
		view.Forecast = Forecast{Kind: KindReached, HitAt: last.At}
		return view
	}
	pace, ok := paceOf(samples, series.ResetsAt, series.WindowMinutes)
	if !ok {
		return view
	}
	if pace <= 0 {
		view.Forecast = Forecast{Kind: KindFlat, PacePerHour: pace}
		return view
	}
	hoursLeft := (100 - last.Used) / pace
	hit, okHit := addHours(last.At, hoursLeft)
	if okHit && !hit.After(now) {
		// The pace measured up to the latest sample already points at a time
		// that has passed, and usage is still under 100%. Projecting that
		// same pace forward from now is not evidence it continued, so the
		// ETA is unknown rather than a clock time in the past.
		view.Forecast = Forecast{Kind: KindUnknown, PacePerHour: pace}
		return view
	}
	if !okHit || (!series.ResetsAt.IsZero() && !hit.Before(series.ResetsAt)) {
		view.Forecast = Forecast{Kind: KindAfterReset, PacePerHour: pace, HitAt: hit}
		return view
	}
	view.Forecast = Forecast{Kind: KindHit, PacePerHour: pace, HitAt: hit}
	return view
}

// addHours returns from+hours. A pace near zero can ask for a duration that
// does not fit in time.Duration; that is not a hit time.
func addHours(from time.Time, hours float64) (time.Time, bool) {
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours < 0 {
		return time.Time{}, false
	}
	maxHours := float64(math.MaxInt64) / float64(time.Hour) / 2
	if hours > maxHours {
		return time.Time{}, false
	}
	d := time.Duration(hours * float64(time.Hour))
	if d < 0 {
		return time.Time{}, false
	}
	hit := from.Add(d).Round(time.Second)
	if hit.Before(from) {
		return time.Time{}, false
	}
	return hit, true
}

// minPaceHours ignores spans shorter than a minute. A statusline refresh a
// second after the window opens would otherwise look like thousands of %/h.
const minPaceHours = 1.0 / 60.0

func paceOf(samples []provider.Sample, resetsAt time.Time, windowMinutes int) (float64, bool) {
	first := samples[0]
	last := samples[len(samples)-1]
	if last.At.After(first.At) {
		hours := last.At.Sub(first.At).Hours()
		if hours >= minPaceHours {
			return (last.Used - first.Used) / hours, true
		}
	}
	if windowMinutes > 0 && !resetsAt.IsZero() {
		start := resetsAt.Add(-time.Duration(windowMinutes) * time.Minute)
		if last.At.After(start) {
			hours := last.At.Sub(start).Hours()
			if hours >= minPaceHours {
				return last.Used / hours, true
			}
		}
	}
	return 0, false
}

func quotaName(providerName, limitID, plan string) string {
	name := provider.DisplayName(providerName)
	switch limitID {
	case "", "codex", "subscription":
	default:
		name += " " + limitID
	}
	if plan != "" {
		name += " (" + plan + ")"
	}
	return name
}

func formatTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "unknown"
	}
	if loc == nil {
		loc = time.UTC
	}
	tt := t.In(loc)
	if tt.Year() != time.Now().In(loc).Year() {
		return tt.Format("Mon 2 Jan 2006 15:04 MST")
	}
	return tt.Format("Mon 2 Jan 15:04 MST")
}

// formatPace hides a pace that prints as -0.0 or 0.0 at one decimal place.
func formatPace(p float64) float64 {
	if math.Abs(p) < 0.05 {
		return 0
	}
	return p
}

func forecastSentence(f Forecast, loc *time.Location) string {
	switch f.Kind {
	case KindStale:
		return "Window has reset; waiting for a new sample."
	case KindReached:
		return "Limit already reached."
	case KindFlat:
		if f.PacePerHour < 0 && math.Abs(f.PacePerHour) >= 0.05 {
			return fmt.Sprintf("Pace %.1f%%/h, usage is falling.", formatPace(f.PacePerHour))
		}
		return "Pace flat, no hit at the current pace."
	case KindAfterReset:
		return fmt.Sprintf("Pace %.1f%%/h, window resets before the limit.", formatPace(f.PacePerHour))
	case KindHit:
		return fmt.Sprintf("Pace %.1f%%/h, limit around %s.", formatPace(f.PacePerHour), formatTime(f.HitAt, loc))
	case KindUnknown:
		return "Pace unknown."
	default:
		return "Not enough samples to forecast."
	}
}

// FormatAlert is the Telegram text for one threshold. It has no paths or prompts.
func FormatAlert(v View, threshold int, loc *time.Location) string {
	return fmt.Sprintf("%s %s is at %.1f%% (alert %d%%).\n%s\nResets %s.",
		quotaName(v.Series.Provider, v.Series.LimitID, v.Series.PlanType),
		v.Series.WindowKey,
		v.Used,
		threshold,
		forecastSentence(v.Forecast, loc),
		formatTime(v.Series.ResetsAt, loc),
	)
}

// FormatStatus renders current windows.
func FormatStatus(views []View, loc *time.Location) string {
	return formatViews(views, loc, false)
}

// FormatForecast renders pace and predicted hit times.
func FormatForecast(views []View, loc *time.Location) string {
	return formatViews(views, loc, true)
}

func formatViews(views []View, loc *time.Location, forecastOnly bool) string {
	if len(views) == 0 {
		return "No subscription windows recorded yet.\nCodex: burnrate reads rollout logs after a turn.\nClaude: point the status line at `burnrate statusline` (Claude Code v2.1.80+)."
	}
	sorted := append([]View(nil), views...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Series.Provider != sorted[j].Series.Provider {
			return sorted[i].Series.Provider < sorted[j].Series.Provider
		}
		if sorted[i].Series.LimitID != sorted[j].Series.LimitID {
			return sorted[i].Series.LimitID < sorted[j].Series.LimitID
		}
		return windowRank(sorted[i].Series.WindowKey) < windowRank(sorted[j].Series.WindowKey)
	})
	var b strings.Builder
	var prev string
	for i, v := range sorted {
		head := quotaName(v.Series.Provider, v.Series.LimitID, v.Series.PlanType)
		if head != prev {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(head)
			b.WriteByte('\n')
			prev = head
		}
		if v.Forecast.Kind == KindStale {
			fmt.Fprintf(&b, "  %s  last sample %.1f%% as of %s; window reset %s\n",
				v.Series.WindowKey, v.Used, formatTime(v.Observed, loc), formatTime(v.Series.ResetsAt, loc))
			continue
		}
		if forecastOnly {
			fmt.Fprintf(&b, "  %s  %.1f%%  %s\n", v.Series.WindowKey, v.Used, forecastSentence(v.Forecast, loc))
			fmt.Fprintf(&b, "      resets %s\n", formatTime(v.Series.ResetsAt, loc))
			continue
		}
		fmt.Fprintf(&b, "  %s  %.1f%%  as of %s  resets %s\n",
			v.Series.WindowKey, v.Used, formatTime(v.Observed, loc), formatTime(v.Series.ResetsAt, loc))
		fmt.Fprintf(&b, "      %s\n", forecastSentence(v.Forecast, loc))
	}
	return strings.TrimRight(b.String(), "\n")
}

func windowRank(key string) int {
	switch key {
	case "5h":
		return 0
	case "7d":
		return 1
	case "spend":
		return 2
	default:
		return 9
	}
}
