package config

import (
	"testing"
	"time"
)

func TestDurationUnmarshalText(t *testing.T) {
	const day = 24 * time.Hour
	const week = 7 * day

	ok := []struct {
		in   string
		want time.Duration
	}{
		// new week/day units
		{"4w", 4 * week},
		{"2w", 2 * week},
		{"1d", day},
		{"3d", 3 * day},
		{"1w", week},
		// compounds (order-independent, mixes with standard units)
		{"2w3d12h", 2*week + 3*day + 12*time.Hour},
		{"1w1d", week + day},
		{"1h2w", 2*week + time.Hour},
		// decimals
		{"1.5w", week + week/2},
		{"0.5d", 12 * time.Hour},
		// sign
		{"-2w", -2 * week},
		// standard-only strings still parse exactly as before
		{"15s", 15 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"0s", 0},
	}
	for _, tc := range ok {
		var d Duration
		if err := d.UnmarshalText([]byte(tc.in)); err != nil {
			t.Errorf("UnmarshalText(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if time.Duration(d) != tc.want {
			t.Errorf("UnmarshalText(%q) = %v, want %v", tc.in, time.Duration(d), tc.want)
		}
	}

	bad := []string{
		"",                    // empty
		"4x",                  // unknown unit, no w/d
		"w",                   // unit with no magnitude
		"1w2x",                // trailing unknown unit
		"abc",                 // not a duration
		"1.2.3w",              // malformed number
		"100000000000000000w", // overflows int64 when scaled to hours
	}
	for _, in := range bad {
		var d Duration
		if err := d.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("UnmarshalText(%q) expected error, got %v", in, time.Duration(d))
		}
	}
}

func TestDurationRoundTripViaHours(t *testing.T) {
	// MarshalText re-serializes as hours (lossy but load-bearing direction is
	// Unmarshal); confirm a value survives a marshal->unmarshal cycle.
	var d Duration
	if err := d.UnmarshalText([]byte("4w")); err != nil {
		t.Fatal(err)
	}
	b, err := d.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var d2 Duration
	if err := d2.UnmarshalText(b); err != nil {
		t.Fatalf("re-unmarshal %q: %v", b, err)
	}
	if d2 != d {
		t.Errorf("round trip: %v -> %q -> %v", time.Duration(d), b, time.Duration(d2))
	}
}
