package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a named type so durations can be written like "15s" in YAML.
// Convention: config fields use this instead of a bare time.Duration.
//
// In addition to the units time.ParseDuration understands (ns, us, ms, s, m, h),
// Duration accepts "w" (weeks = 168h) and "d" (days = 24h) so long spans like a
// 4-week TTL are writable as "4w" instead of "672h". Weeks/days may be combined
// with each other and with standard units ("2w3d12h") and take a fraction
// ("1.5w"); a single leading sign is honored ("-2w").
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(b []byte) error {
	expanded, err := expandWeeksDays(string(b))
	if err != nil {
		return err
	}
	v, err := time.ParseDuration(expanded)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// expandWeeksDays rewrites any "w"/"d" unit tokens into their hour equivalents,
// leaving every other token (standard units, sign) verbatim, so the result can
// be handed to time.ParseDuration (which sums repeated units). A string with no
// w/d unit is returned unchanged so time.ParseDuration reports its own error for
// malformed standard input. On a token this does not recognize, it returns the
// original string unchanged and lets time.ParseDuration produce the error.
func expandWeeksDays(s string) (string, error) {
	if !strings.ContainsAny(s, "wd") {
		return s, nil
	}
	var b strings.Builder
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		b.WriteByte(s[i])
		i++
	}
	for i < len(s) {
		// number: digits and an optional decimal point
		start := i
		for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
			i++
		}
		num := s[start:i]
		if num == "" {
			return s, nil // no number where one is expected; defer to ParseDuration
		}
		// unit: everything up to the next number, decimal point, or sign
		ustart := i
		for i < len(s) && !((s[i] >= '0' && s[i] <= '9') || s[i] == '.' || s[i] == '+' || s[i] == '-') {
			i++
		}
		unit := s[ustart:i]
		switch unit {
		case "w", "d":
			hours, err := scaleToHours(num, unit)
			if err != nil {
				return s, nil // malformed number; defer to ParseDuration
			}
			b.WriteString(hours)
			b.WriteByte('h')
		default:
			b.WriteString(num)
			b.WriteString(unit)
		}
	}
	return b.String(), nil
}

// scaleToHours converts a week ("w") or day ("d") magnitude to its value in
// hours as a decimal string. Whole numbers scale as integers to stay exact; a
// fractional magnitude falls back to float arithmetic (sub-nanosecond error on a
// week-scale value is immaterial for config durations).
func scaleToHours(num, unit string) (string, error) {
	var perUnit int64
	switch unit {
	case "w":
		perUnit = 168
	case "d":
		perUnit = 24
	}
	if !strings.ContainsRune(num, '.') {
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return "", err
		}
		hours := n * perUnit
		if n != 0 && hours/perUnit != n {
			// int64 overflow: without this the wrapped small value would parse as a
			// wrong (short) duration instead of erroring.
			return "", fmt.Errorf("duration magnitude %q%s overflows", num, unit)
		}
		return strconv.FormatInt(hours, 10), nil
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return "", err
	}
	return strconv.FormatFloat(f*float64(perUnit), 'f', -1, 64), nil
}
