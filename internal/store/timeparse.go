package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseTime accepts RFC3339, "2006-01-02", "2006-01-02 15:04[:05]", unix
// seconds, or a relative duration back from now such as "30m", "24h", "7d".
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 1e9 {
		return time.Unix(n, 0), nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q (use 2006-01-02, 2006-01-02 15:04, RFC3339, unix seconds, or 24h/7d)", s)
}

// ParseUntil is ParseTime, but a bare date means the end of that day.
func ParseUntil(s string) (time.Time, error) {
	t, err := ParseTime(s)
	if err == nil && len(strings.TrimSpace(s)) == len("2006-01-02") {
		t = t.Add(24*time.Hour - time.Second)
	}
	return t, err
}
