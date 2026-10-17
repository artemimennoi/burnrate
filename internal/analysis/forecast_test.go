package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/artemimennoi/burnrate/internal/provider"
)

func TestForecastFromSamples(t *testing.T) {
	resets := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	series := provider.Series{
		Provider: "claude", LimitID: "subscription", WindowKey: "5h",
		WindowMinutes: 300, ResetsAt: resets,
		Samples: []provider.Sample{
			{At: time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC), Used: 20},
			{At: time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC), Used: 50},
		},
	}
	view := Evaluate(series, now)
	if view.Forecast.Kind != KindHit {
		t.Fatalf("kind = %v", view.Forecast.Kind)
	}
	if view.Forecast.PacePerHour != 30 {
		t.Fatalf("pace = %v", view.Forecast.PacePerHour)
	}
	wantHit := time.Date(2026, 10, 17, 12, 40, 0, 0, time.UTC)
	if !view.Forecast.HitAt.Equal(wantHit) {
		t.Fatalf("hit = %s", view.Forecast.HitAt)
	}
	if view.Used != 50 {
		t.Fatalf("used = %v", view.Used)
	}
}

func TestForecastAfterResetAndFlat(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	slow := provider.Series{
		Provider: "codex", LimitID: "codex", WindowKey: "7d", WindowMinutes: 10080,
		ResetsAt: time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC),
		Samples: []provider.Sample{
			{At: time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC), Used: 10},
			{At: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Used: 12},
		},
	}
	if got := Evaluate(slow, now).Forecast.Kind; got != KindAfterReset {
		t.Fatalf("slow kind = %v", got)
	}
	flat := slow
	flat.Samples = []provider.Sample{
		{At: slow.Samples[0].At, Used: 12},
		{At: slow.Samples[1].At, Used: 12},
	}
	if got := Evaluate(flat, now).Forecast.Kind; got != KindFlat {
		t.Fatalf("flat kind = %v", got)
	}
	falling := flat
	falling.Samples[1].Used = 5
	view := Evaluate(falling, now)
	if view.Forecast.Kind != KindFlat || view.Forecast.PacePerHour >= 0 {
		t.Fatalf("falling = %#v", view.Forecast)
	}
}

func TestForecastSingleSampleUsesWindowStart(t *testing.T) {
	resets := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 17, 13, 30, 0, 0, time.UTC)
	series := provider.Series{
		Provider: "codex", LimitID: "codex", WindowKey: "5h", WindowMinutes: 300, ResetsAt: resets,
		Samples: []provider.Sample{{At: time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC), Used: 40}},
	}
	view := Evaluate(series, now)
	if view.Forecast.Kind != KindHit {
		t.Fatalf("kind = %v pace %v", view.Forecast.Kind, view.Forecast.PacePerHour)
	}
	if view.Forecast.PacePerHour != 40 {
		t.Fatalf("pace = %v", view.Forecast.PacePerHour)
	}
	want := time.Date(2026, 10, 17, 14, 30, 0, 0, time.UTC)
	if !view.Forecast.HitAt.Equal(want) {
		t.Fatalf("hit = %s", view.Forecast.HitAt)
	}
}

func TestForecastIgnoresTinySpan(t *testing.T) {
	resets := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	now := resets.Add(-5 * time.Hour).Add(2 * time.Second)
	view := Evaluate(provider.Series{
		Provider: "claude", LimitID: "subscription", WindowKey: "5h",
		WindowMinutes: 300, ResetsAt: resets,
		Samples: []provider.Sample{{At: now, Used: 23.5}},
	}, now)
	if view.Forecast.Kind != KindInsufficient {
		t.Fatalf("kind = %v pace %v", view.Forecast.Kind, view.Forecast.PacePerHour)
	}
}

func TestForecastTinyPaceDoesNotOverflow(t *testing.T) {
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	resets := end.Add(24 * time.Hour)
	view := Evaluate(provider.Series{
		Provider: "claude", LimitID: "subscription", WindowKey: "7d",
		WindowMinutes: 10080, ResetsAt: resets,
		Samples: []provider.Sample{
			{At: start, Used: 10},
			{At: end, Used: 10.001},
		},
	}, end)
	if view.Forecast.Kind != KindAfterReset {
		t.Fatalf("kind = %v hit %s pace %v", view.Forecast.Kind, view.Forecast.HitAt, view.Forecast.PacePerHour)
	}
	if !view.Forecast.HitAt.IsZero() && (view.Forecast.HitAt.Year() < 2026 || !view.Forecast.HitAt.Before(resets)) {
		t.Fatalf("hit = %s", view.Forecast.HitAt)
	}
}

func TestForecastReachedAndStale(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	reached := Evaluate(provider.Series{
		Provider: "claude", WindowKey: "5h", ResetsAt: now.Add(time.Hour),
		Samples: []provider.Sample{{At: now.Add(-time.Minute), Used: 100}},
	}, now)
	if reached.Forecast.Kind != KindReached {
		t.Fatalf("reached = %v", reached.Forecast.Kind)
	}
	stale := Evaluate(provider.Series{
		Provider: "claude", WindowKey: "5h", ResetsAt: now.Add(-time.Minute),
		Samples: []provider.Sample{{At: now.Add(-time.Hour), Used: 90}},
	}, now)
	if stale.Forecast.Kind != KindStale {
		t.Fatalf("stale = %v", stale.Forecast.Kind)
	}
}

func TestFormatUsesTimezoneAndOmitsSecrets(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Minsk")
	if err != nil {
		t.Fatal(err)
	}
	resets := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	view := Evaluate(provider.Series{
		Provider: "claude", LimitID: "subscription", PlanType: "max", WindowKey: "5h",
		WindowMinutes: 300, ResetsAt: resets,
		Samples: []provider.Sample{
			{At: time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC), Used: 20},
			{At: time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC), Used: 50},
		},
	}, time.Date(2026, 10, 17, 11, 30, 0, 0, time.UTC))
	text := FormatAlert(view, 50, loc)
	if strings.Contains(text, "SECRET_PATH") || strings.Contains(text, ".jsonl") {
		t.Fatalf("alert leaked path-like text: %s", text)
	}
	if !strings.Contains(text, "Claude (max) 5h is at 50.0% (alert 50%).") {
		t.Fatalf("alert = %s", text)
	}
	if !strings.Contains(text, "Sat 17 Oct 18:00 +03") {
		t.Fatalf("reset not in Minsk: %s", text)
	}
	status := FormatStatus([]View{view}, loc)
	if !strings.Contains(status, "Claude (max)") || !strings.Contains(status, "5h  50.0%") {
		t.Fatalf("status = %s", status)
	}
	if FormatStatus(nil, loc) == "" || FormatForecast(nil, loc) == "" {
		t.Fatal("empty report")
	}
}

func TestNegativePaceDoesNotPrintSignedZero(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	view := Evaluate(provider.Series{
		Provider: "codex", WindowKey: "5h", WindowMinutes: 300,
		ResetsAt: now.Add(3 * time.Hour),
		Samples: []provider.Sample{
			{At: now.Add(-2 * time.Hour), Used: 40},
			{At: now, Used: 39.99},
		},
	}, now)
	if view.Forecast.PacePerHour >= 0 {
		t.Fatalf("pace = %v", view.Forecast.PacePerHour)
	}
	text := FormatForecast([]View{view}, time.UTC)
	if strings.Contains(text, "-0.0") {
		t.Fatalf("forecast = %s", text)
	}
}

func TestResetDateIncludesOtherYear(t *testing.T) {
	resets := time.Date(2027, 1, 2, 15, 4, 0, 0, time.UTC)
	view := View{
		Series:   provider.Series{Provider: "codex", WindowKey: "5h", ResetsAt: resets},
		Used:     10,
		Observed: resets.Add(-time.Hour),
		Forecast: Forecast{Kind: KindFlat},
	}
	text := FormatStatus([]View{view}, time.UTC)
	if !strings.Contains(text, "2027") {
		t.Fatalf("status = %s", text)
	}
	if strings.Contains(text, "2026") {
		t.Fatalf("current year leaked into a 2027 reset: %s", text)
	}
}
