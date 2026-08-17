package tui

import (
	"testing"
	"time"
)

func TestAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		// Clock skew between this machine and the control plane is real, and a
		// negative age would read as a bug in the TUI rather than what it is.
		{-5 * time.Second, "now"},
		{0, "now"},
		{900 * time.Millisecond, "now"},
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m"},
		{2 * time.Hour, "2h"},
		{50 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		if got := age(tt.d); got != tt.want {
			t.Errorf("age(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestBytesShort(t *testing.T) {
	tests := []struct {
		b    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KiB"},
		{8 << 30, "8.0GiB"},
	}
	for _, tt := range tests {
		if got := bytesShort(tt.b); got != tt.want {
			t.Errorf("bytesShort(%d) = %q, want %q", tt.b, got, tt.want)
		}
	}
}

// A node that has not reported capacity is a real state, not an impossibility —
// dividing by it must not produce NaN or a panic.
func TestRatioHandlesZeroTotal(t *testing.T) {
	if got := ratio(5, 0); got != "  -" {
		t.Errorf("ratio(5,0) = %q, want a dash", got)
	}
	if got := ratio(2048, 8192); got != " 25%" {
		t.Errorf("ratio = %q, want ' 25%%'", got)
	}
}

// A clipped value must be visibly clipped. A silently truncated id is the kind
// of thing someone copies out of a dashboard and then cannot find.
func TestTruncateMarksTheCut(t *testing.T) {
	if got := truncate("abcdef", 10); got != "abcdef" {
		t.Errorf("short strings pass through, got %q", got)
	}
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q, want 'abc…'", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Errorf("zero width = %q, want empty", got)
	}
	// Multi-byte input must be cut by rune, not by byte, or the output is
	// mojibake in exactly the terminals that support the width best.
	if got := truncate("héllo wörld", 6); got != "héllo…" {
		t.Errorf("truncate multibyte = %q, want 'héllo…'", got)
	}
}

func TestPadAlignsAndTruncates(t *testing.T) {
	if got := pad("ab", 5); got != "ab   " {
		t.Errorf("pad = %q", got)
	}
	// Overflow must be cut, not allowed to shift every column after it.
	if got := pad("abcdefgh", 4); got != "abc…" {
		t.Errorf("pad overflow = %q, want 'abc…'", got)
	}
	if l := len([]rune(pad("héllo", 3))); l != 3 {
		t.Errorf("padded width must be measured in runes, got %d", l)
	}
}

func TestShortIDMatchesEnv8(t *testing.T) {
	// The platform names containers with the first 8 characters of the
	// environment UUID; showing anything else means the screen and the node
	// disagree about what a thing is called.
	if got := shortID("4e71b5fd-a2ef-4a60-a1e0-b37303fb758b"); got != "4e71b5fd" {
		t.Errorf("shortID = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("short input should pass through, got %q", got)
	}
}

// Two consecutive polls of an unchanged node must render identically. Map
// iteration order would otherwise make the row appear to change on every
// refresh, which trains the reader to ignore movement.
func TestLabelsAreStable(t *testing.T) {
	l := map[string]string{"zone": "eu", "ingress": "true", "arch": "arm64"}
	first := labels(l)
	for i := 0; i < 20; i++ {
		if got := labels(l); got != first {
			t.Fatalf("labels not deterministic: %q vs %q", got, first)
		}
	}
	if first != "arch=arm64,ingress=true,zone=eu" {
		t.Errorf("labels should be sorted, got %q", first)
	}
}
