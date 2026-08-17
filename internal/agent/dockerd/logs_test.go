package dockerd

import (
	"strings"
	"testing"
)

// framed builds Docker's multiplexed stream framing: an 8-byte header carrying
// the stream type and a big-endian length, then the payload.
func framed(stream byte, payload string) []byte {
	n := len(payload)
	h := []byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	return append(h, payload...)
}

// Without demultiplexing, every line arrives with eight bytes of binary header
// glued to it — output that looks corrupted rather than absent, which is worse
// because it reads as a bug in the tenant's application.
func TestDemuxStripsDockerFraming(t *testing.T) {
	raw := append(framed(1, "out line\n"), framed(2, "err line\n")...)
	got := demuxLogs(raw, 1<<20)
	if got != "out line\nerr line\n" {
		t.Fatalf("framing not stripped: %q", got)
	}
}

// A container started with a TTY produces no framing at all. Treating that as
// corruption would blank the output of every such container, so an unrecognised
// header is taken as plain text.
func TestDemuxPassesUnframedTTYOutput(t *testing.T) {
	raw := []byte("plain tty output with no header\n")
	if got := demuxLogs(raw, 1<<20); got != string(raw) {
		t.Fatalf("TTY output altered: %q", got)
	}
}

// Tail counts lines, and a line has no length limit: one enormous JSON blob per
// line satisfies Tail:10 and still returns megabytes. The byte cap is what makes
// the bound real, and it has to announce itself — a silently short answer is
// indistinguishable from a container that stopped logging.
func TestDemuxCapsBytesAndSaysSo(t *testing.T) {
	raw := framed(1, strings.Repeat("x", 500))
	got := demuxLogs(raw, 64)
	if len(got) > 200 {
		t.Fatalf("byte cap not applied, got %d bytes", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncation must be visible in the output, got %q", got)
	}
}

// The cap can land mid-message, leaving a header that claims more bytes than
// remain. Trusting that length would slice out of range.
func TestDemuxSurvivesTruncationMidMessage(t *testing.T) {
	raw := framed(1, "abcdefghij")
	got := demuxLogs(raw[:12], 1<<20) // header plus 4 of 10 payload bytes
	if got != "abcd" {
		t.Fatalf("expected the partial payload, got %q", got)
	}
}
