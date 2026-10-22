package codex

import (
	"bufio"
	"os"
	"testing"
	"time"
)

func TestParseFixture(t *testing.T) {
	f, err := os.Open("testdata/token_count.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var lines [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 7 {
		t.Fatalf("lines = %d", len(lines))
	}

	if got := ParseLine(lines[0]).Kind; got != KindIgnore {
		t.Fatalf("meta kind = %v", got)
	}

	full := ParseLine(lines[1])
	if full.Kind != KindSnapshot {
		t.Fatalf("full kind = %v", full.Kind)
	}
	if full.Snapshot.PlanType != "plus" || full.Snapshot.LimitID != "codex" {
		t.Fatalf("snapshot = %#v", full.Snapshot)
	}
	if len(full.Snapshot.Windows) != 2 {
		t.Fatalf("windows = %#v", full.Snapshot.Windows)
	}
	if full.Snapshot.Windows[0].Key != "5h" || full.Snapshot.Windows[0].Slot != "primary" || full.Snapshot.Windows[0].UsedPercent != 62 {
		t.Fatalf("primary = %#v", full.Snapshot.Windows[0])
	}
	if full.Snapshot.Windows[1].Key != "7d" || full.Snapshot.Windows[1].UsedPercent != 81 || full.Snapshot.Windows[1].WindowMinutes != 10080 {
		t.Fatalf("secondary = %#v", full.Snapshot.Windows[1])
	}
	if !full.Snapshot.Windows[0].ResetsAt.Equal(time.Unix(1790000000, 0).UTC()) {
		t.Fatalf("resets = %s", full.Snapshot.Windows[0].ResetsAt)
	}

	if got := ParseLine(lines[2]); got.Kind != KindSkipNull || len(got.Snapshot.Windows) != 0 {
		t.Fatalf("premium = %#v", got)
	}
	if got := ParseLine(lines[3]).Kind; got != KindSkipNull {
		t.Fatalf("null rate_limits kind = %v", got)
	}

	zero := ParseLine(lines[4])
	if zero.Kind != KindSnapshot || len(zero.Snapshot.Windows) != 1 || zero.Snapshot.Windows[0].UsedPercent != 0 {
		t.Fatalf("zero = %#v", zero)
	}
	if zero.Snapshot.Windows[0].Key != "5h" {
		t.Fatalf("zero key = %s", zero.Snapshot.Windows[0].Key)
	}

	weekly := ParseLine(lines[5])
	if weekly.Kind != KindSnapshot || len(weekly.Snapshot.Windows) != 1 {
		t.Fatalf("weekly = %#v", weekly)
	}
	w := weekly.Snapshot.Windows[0]
	if w.Key != "7d" || w.Slot != "primary" || w.UsedPercent != 88 || w.WindowMinutes != 10080 {
		t.Fatalf("weekly window = %#v", w)
	}

	if got := ParseLine(lines[6]).Kind; got != KindIgnore {
		t.Fatalf("broken kind = %v", got)
	}
}

func TestParseRejectsWildPercents(t *testing.T) {
	line := func(pct string) string {
		return `{"timestamp":"2026-10-17T12:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"plus","primary":{"used_percent":` + pct + `,"window_minutes":300,"resets_at":1792300000},"secondary":null}}}`
	}
	if got := ParseLine([]byte(line("1e10"))).Kind; got != KindSkipNull {
		t.Fatalf("huge kind = %v", got)
	}
	if got := ParseLine([]byte(line("-50"))).Kind; got != KindSkipNull {
		t.Fatalf("negative kind = %v", got)
	}
	high := ParseLine([]byte(line("100.4")))
	if high.Kind != KindSnapshot || len(high.Snapshot.Windows) != 1 || high.Snapshot.Windows[0].UsedPercent != 100 {
		t.Fatalf("clamp = %#v", high.Snapshot.Windows)
	}
	low := ParseLine([]byte(line("-0.2")))
	if low.Kind != KindSnapshot || len(low.Snapshot.Windows) != 1 || low.Snapshot.Windows[0].UsedPercent != 0 {
		t.Fatalf("clamp low = %#v", low.Snapshot.Windows)
	}
	over := ParseLine([]byte(line("120")))
	if over.Kind != KindSnapshot || len(over.Snapshot.Windows) != 1 || over.Snapshot.Windows[0].UsedPercent != 100 {
		t.Fatalf("120 = %#v", over.Snapshot.Windows)
	}
}
