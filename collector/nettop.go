package collector

import (
	"fmt"
	"strconv"
	"strings"
)

// parseProcessField splits nettop's "name.pid" (names may contain dots).
func parseProcessField(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	idx := strings.LastIndex(s, ".")
	if idx < 0 {
		return "", 0, fmt.Errorf("no dot in process field: %q", s)
	}
	pid, err := strconv.Atoi(s[idx+1:])
	if err != nil {
		return "", 0, fmt.Errorf("parse pid %q: %w", s[idx+1:], err)
	}
	return s[:idx], pid, nil
}
