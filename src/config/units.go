package config

import (
	"fmt"
	"strings"
	"time"
)

// ParseDuration parses a human duration like "7d", "72h", "30m".
// Extends time.ParseDuration with "d" (days) support.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		s = strings.TrimSuffix(s, "d")
		var days int
		if _, err := fmt.Sscanf(s, "%d", &days); err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s+"d", err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// ParseSize parses a human size like "15GB", "500MB", "100KB".
// Uses binary units (1 GB = 1024³ bytes).
func ParseSize(s string) (int64, error) {
	raw := s
	s = strings.TrimSpace(strings.ToUpper(s))
	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(s, "TB"):
		multiplier = 1024 * 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "TB")
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	}
	var val int64
	if _, err := fmt.Sscanf(s, "%d", &val); err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", raw, err)
	}
	return val * multiplier, nil
}
