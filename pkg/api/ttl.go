package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseTTL parses lifetimes such as "90s", "30m", "12h", "7d", "2w", "1d12h"
// and "never". It returns (0, true, nil) for "never".
func ParseTTL(s string) (d time.Duration, never bool, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "never", "forever", "permanent", "none", "inf", "infinite":
		return 0, true, nil
	case "":
		return 0, false, fmt.Errorf("empty ttl")
	}
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 || i == len(rest) {
			return 0, false, fmt.Errorf("invalid ttl %q (examples: 30m, 12h, 7d, 2w, never)", s)
		}
		n, perr := strconv.ParseFloat(rest[:i], 64)
		if perr != nil {
			return 0, false, fmt.Errorf("invalid ttl %q", s)
		}
		j := i
		for j < len(rest) && rest[j] >= 'a' && rest[j] <= 'z' {
			j++
		}
		var unit time.Duration
		switch rest[i:j] {
		case "s", "sec", "secs", "second", "seconds":
			unit = time.Second
		case "m", "min", "mins", "minute", "minutes":
			unit = time.Minute
		case "h", "hr", "hrs", "hour", "hours":
			unit = time.Hour
		case "d", "day", "days":
			unit = 24 * time.Hour
		case "w", "wk", "week", "weeks":
			unit = 7 * 24 * time.Hour
		case "mo", "month", "months":
			unit = 30 * 24 * time.Hour
		case "y", "year", "years":
			unit = 365 * 24 * time.Hour
		default:
			return 0, false, fmt.Errorf("invalid ttl unit in %q (use s, m, h, d, w, mo, y)", s)
		}
		d += time.Duration(n * float64(unit))
		rest = rest[j:]
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("ttl must be positive")
	}
	return d, false, nil
}

// FormatTTL renders a duration compactly (e.g. 7d, 36h, 45m).
func FormatTTL(d time.Duration) string {
	switch {
	case d%(7*24*time.Hour) == 0 && d >= 7*24*time.Hour:
		return fmt.Sprintf("%dw", d/(7*24*time.Hour))
	case d%(24*time.Hour) == 0 && d >= 24*time.Hour:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0 && d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.Round(time.Second).String()
}
