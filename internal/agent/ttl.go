package agent

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// agentTTLPattern matches the API-spec TTL format \d+[hmd]: one or more
// digits followed by a single lowercase unit (h, m, or d). No bare numbers,
// no decimals, no negatives, no uppercase units, no whitespace.
var agentTTLPattern = regexp.MustCompile(`^(\d+)([hmd])$`)

// ParseAgentTTL parses a TTL string in the API-spec format \d+[hmd]
// (specs/api.md, specs/agent-lifecycle.md): "6h" -> 6h, "90m" -> 90m,
// "24h" -> 24h, "7d" -> 168h (day = 24h).
//
// It is a pure function with no package state: it does not consult
// configuration and does not default the empty string — callers decide
// defaulting. Invalid formats, zero/negative durations, and values that
// overflow time.Duration return an error.
func ParseAgentTTL(s string) (time.Duration, error) {
	m := agentTTLPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("must match %q (digits followed by h, m, or d)", `\d+[hmd]`)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value %q out of range", m[1])
	}
	var unit time.Duration
	switch m[2] {
	case "h":
		unit = time.Hour
	case "m":
		unit = time.Minute
	case "d":
		unit = 24 * time.Hour
	}
	if n > int64(math.MaxInt64)/int64(unit) {
		return 0, fmt.Errorf("value %q overflows duration", s)
	}
	d := time.Duration(n) * unit
	if d <= 0 {
		return 0, fmt.Errorf("must be a positive duration")
	}
	return d, nil
}
