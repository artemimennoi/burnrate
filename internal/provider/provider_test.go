package provider

import (
	"errors"
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
