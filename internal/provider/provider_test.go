package provider

import (
	"errors"
	"math"
	"testing"
)

func TestWindowKey(t *testing.T) {
	cases := []struct {
		minutes int
		want    string
	}{
		{300, "5h"},
		{10080, "7d"},
		{1440, "1440m"},
		{0, ""},
		{-5, ""},
	}
	for _, tc := range cases {
		if got := WindowKey(tc.minutes); got != tc.want {
			t.Fatalf("WindowKey(%d) = %q, want %q", tc.minutes, got, tc.want)
		}
	}
}

func TestCleanPercent(t *testing.T) {
	if _, ok := CleanPercent(math.NaN()); ok {
		t.Fatal("accepted NaN")
	}
	if _, ok := CleanPercent(math.Inf(1)); ok {
		t.Fatal("accepted +Inf")
	}
	if _, ok := CleanPercent(math.Inf(-1)); ok {
		t.Fatal("accepted -Inf")
	}
	if _, ok := CleanPercent(1e10); ok {
		t.Fatal("accepted a huge percent")
	}
	if _, ok := CleanPercent(-50); ok {
		t.Fatal("accepted -50")
	}
	got, ok := CleanPercent(100.4)
	if !ok || got != 100 {
		t.Fatalf("100.4 -> %v %v", got, ok)
	}
	got, ok = CleanPercent(-0.2)
	if !ok || got != 0 {
		t.Fatalf("-0.2 -> %v %v", got, ok)
	}
	got, ok = CleanPercent(40)
	if !ok || got != 40 {
		t.Fatalf("40 -> %v %v", got, ok)
	}
}

func TestDisabledDoesNoIO(t *testing.T) {
	_, err := Disabled{ID: "cursor"}.Collect(nil, nil)
	var ni *NotImplementedError
	if !errors.As(err, &ni) {
		t.Fatalf("got %v, want NotImplementedError", err)
	}
	if ni.Provider != "cursor" {
		t.Fatalf("provider = %q", ni.Provider)
	}
}
