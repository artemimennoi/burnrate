package alert

import "testing"

func TestPendingThresholds(t *testing.T) {
	thresholds := []int{90, 50, 75}
	if got := Pending(40, thresholds, nil, false); len(got) != 0 {
		t.Fatalf("40%% = %#v", got)
	}
	got := Pending(50, thresholds, nil, false)
	if len(got) != 1 || got[0] != 50 {
		t.Fatalf("50%% = %#v", got)
	}
	got = Pending(76, thresholds, nil, false)
	if len(got) != 2 || got[0] != 50 || got[1] != 75 {
		t.Fatalf("76%% = %#v", got)
	}
	got = Pending(90, thresholds, map[int]bool{50: true}, false)
	if len(got) != 2 || got[0] != 75 || got[1] != 90 {
		t.Fatalf("deduped = %#v", got)
	}
	if got := Pending(95, thresholds, map[int]bool{50: true, 75: true, 90: true}, false); len(got) != 0 {
		t.Fatalf("all sent = %#v", got)
	}
	if got := Pending(99, thresholds, nil, true); len(got) != 0 {
		t.Fatalf("stale = %#v", got)
	}
	// A new resets_at is a new window: the caller passes an empty sent map.
	got = Pending(55, thresholds, map[int]bool{}, false)
	if len(got) != 1 || got[0] != 50 {
		t.Fatalf("new window = %#v", got)
	}
}

func TestHighestThreshold(t *testing.T) {
	got := Pending(92, []int{50, 75, 90}, nil, false)
	th, ok := Highest(got)
	if !ok || th != 90 {
		t.Fatalf("highest = %d %v from %#v", th, ok, got)
	}
	if _, ok := Highest(nil); ok {
		t.Fatal("empty pending")
	}
}
