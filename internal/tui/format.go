package tui

import (
	"fmt"
	"strings"
	"time"
)

// Formatting helpers are pure and separate from rendering so they can be tested
// without a terminal. Everything here answers a question an operator asks out
// loud — "how stale is this?", "how full is that node?" — so the answers are
// shaped for glancing at, not for precision.

// age renders a duration the way a status line should: coarse, short, and
// never wider than a few columns, because it sits next to data that matters
// more than it does.
func age(d time.Duration) string {
	switch {
	case d < 0:
		// Clock skew between this machine and the control plane. Reporting a
		// negative age would look like a bug in the TUI rather than what it is,
		// so it clamps and says "now".
		return "now"
	case d < time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// bytesShort renders a byte count in binary units. Node capacity is quoted in
// GiB everywhere else in this project (declared limits, advertised memory), so
// matching that avoids an operator comparing two numbers that use different
// units without saying so.
func bytesShort(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGT"[exp])
}

// ratio renders used-of-total as a percentage, guarding a zero total because a
// node that has not reported capacity yet is a real state, not an impossibility.
func ratio(used, total int64) string {
	if total <= 0 {
		return "  -"
	}
	pct := float64(used) / float64(total) * 100
	if pct < 0 {
		pct = 0
	}
	return fmt.Sprintf("%3.0f%%", pct)
}

// truncate cuts to a column budget, marking the cut so a reader can tell the
// difference between a short value and a clipped one. A silently clipped id is
// the kind of thing someone copies and then cannot find.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// shortID trims a UUID to the same 8 characters the platform uses for env8 in
// container and network names, so what is on screen matches what is on the
// node.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// pad right-fills to a fixed width so columns line up without a table library.
// Truncation happens first: a value that overflows its column would otherwise
// shift every column after it and make the whole row unreadable.
func pad(s string, w int) string {
	s = truncate(s, w)
	if n := w - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// agoPhrase renders an elapsed time as something a person would say. age()
// alone produces "now ago", which reads as a bug in the clock rather than as
// freshness.
func agoPhrase(d time.Duration) string {
	a := age(d)
	if a == "now" {
		return "just now"
	}
	return a + " ago"
}
