package claude

import (
	"os"
	"testing"
	"time"
)

func TestParseStatuslineFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/statusline.json")
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	snap, err := ParseStatusline(raw, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %#v", snap.Windows)
	}
	if snap.Windows[0].Key != "5h" || snap.Windows[0].UsedPercent != 23.5 || snap.Windows[0].Slot != "five_hour" {
		t.Fatalf("five_hour = %#v", snap.Windows[0])
	}
	if !snap.Windows[0].ResetsAt.Equal(time.Unix(1738425600, 0).UTC()) {
		t.Fatalf("resets = %s", snap.Windows[0].ResetsAt)
	}
	if snap.Windows[1].Key != "7d" || snap.Windows[1].UsedPercent != 41.2 || snap.Windows[1].WindowMinutes != 10080 {
		t.Fatalf("seven_day = %#v", snap.Windows[1])
	}
	if snap.Provider != "claude" || !snap.ObservedAt.Equal(observed) {
		t.Fatalf("snap = %#v", snap)
	}
}

func TestParseStatuslineISOAndAbsent(t *testing.T) {
	raw := []byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":"2026-10-17T18:00:00Z"},"seven_day":null}}`)
	snap, err := ParseStatusline(raw, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "5h" {
		t.Fatalf("windows = %#v", snap.Windows)
	}
	want := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	if !snap.Windows[0].ResetsAt.Equal(want) {
		t.Fatalf("resets = %s", snap.Windows[0].ResetsAt)
	}
	empty, err := ParseStatusline([]byte(`{"model":{"display_name":"Opus"}}`), time.Now())
	if err != nil || len(empty.Windows) != 0 {
		t.Fatalf("empty = %#v err=%v", empty, err)
	}
	if _, err := ParseStatusline([]byte(`{`), time.Now()); err == nil {
		t.Fatal("expected json error")
	}
}

func TestParseUsageFixtureDedupKey(t *testing.T) {
	raw, err := os.ReadFile("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var outputs []int64
	for _, line := range bytesSplit(raw) {
		usage, ok := ParseUsage(line)
		if !ok {
			if bytesContains(line, []byte("SECRET_PROMPT_DO_NOT_STORE")) && ok {
				t.Fatal("stored a prompt")
			}
			continue
		}
		if usage.Provider != "claude" {
			t.Fatalf("provider = %s", usage.Provider)
		}
		ids = append(ids, usage.MessageID)
		outputs = append(outputs, usage.OutputTokens)
	}
	if len(ids) != 4 {
		t.Fatalf("ids = %#v", ids)
	}
	if ids[0] != "msg_1" || outputs[0] != 3 || ids[1] != "msg_1" || outputs[1] != 120 {
		t.Fatalf("streaming = %v %v", ids, outputs)
	}
	if ids[2] != "msg_2" || outputs[2] != 40 || ids[3] != "solo" || outputs[3] != 2 {
		t.Fatalf("rest = %v %v", ids, outputs)
	}
}
