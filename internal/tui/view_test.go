package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/craig/composectl/internal/client"
)

// Rendering is not asserted line by line — that tests the layout, which is
// meant to change. These pin the properties that would make the display lie or
// crash.

// A terminal can be any size, and Bubble Tea delivers the first size *after*
// start, so there is a real moment with zero dimensions. Rendering a table into
// that is where a TUI panics on a slice bound.
func TestViewSurvivesAnySize(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "dev-node-1", State: "ready"}}, at: t0})
	m = send(m, catalogMsg{rows: []envRow{{App: "a", Stack: "s", Env: "prod", EnvID: "e1"}}, at: t0})
	m = send(m, eventsMsg{events: []client.Event{{Kind: "deployment.promoted", Message: "x"}}, at: t0})

	for _, size := range [][2]int{{0, 0}, {1, 1}, {20, 6}, {40, 10}, {200, 60}, {80, 7}} {
		m.width, m.height = size[0], size[1]
		for p := pane(0); p < paneCount; p++ {
			m.active = p
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("View panicked at %dx%d on pane %v: %v", size[0], size[1], p, r)
					}
				}()
				_ = m.View()
			}()
		}
	}
}

// Below a usable size the panes are not merely cramped, they are misleading:
// fixed-width columns overflow into each other and the result reads like data
// rather than like a layout that ran out of room. Say so instead.
//
// Worth noting for anyone strengthening this: the render path is panic-safe on
// its own — window() clamps, pad() and truncate() handle zero width — so the
// test above passes with or without the guard. This is what actually pins it.
func TestTinyTerminalSaysSoRatherThanGarbling(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "dev-node-1", State: "ready"}}, at: t0})

	m.width, m.height = 10, 4
	if got := m.View(); !strings.Contains(got, "too small") {
		t.Errorf("a terminal too small to lay out must say so, got:\n%s", got)
	}
	// And it must not start rendering rows into a space that cannot hold them.
	if strings.Contains(m.View(), "dev-node-1") {
		t.Error("no table should be drawn below the usable size")
	}

	m.width, m.height = 120, 40
	if !strings.Contains(m.View(), "dev-node-1") {
		t.Error("at a usable size the table must render")
	}
}

// The staleness contract, at the level a reader sees it: when a refresh is
// failing, the screen must say so and say how old what it is showing is. A
// dashboard that renders stale data silently is worse than one that is down,
// because it is believed.
func TestViewReportsAFailingRefresh(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "dev-node-1", State: "ready"}}, at: t0})
	m.now = t0.Add(30 * time.Second)
	m = send(m, fleetMsg{at: m.now, err: errTest})

	out := m.View()
	if !strings.Contains(out, "refresh failed") {
		t.Errorf("a failing refresh must be visible:\n%s", out)
	}
	if !strings.Contains(out, "30s ago") {
		t.Errorf("the age of what is displayed must be visible:\n%s", out)
	}
	if !strings.Contains(out, "dev-node-1") {
		t.Errorf("the last good data should still be shown:\n%s", out)
	}
}

func TestViewMarksStaleData(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "n", State: "ready"}}, at: t0})
	m.now = t0.Add(staleAfter + time.Second)
	if !strings.Contains(m.View(), "stale") {
		t.Errorf("data past the stale threshold must be marked:\n%s", m.View())
	}
}

// The logs pane must say that logs are not stored. Implying durability the
// platform does not provide is the kind of thing an operator finds out about
// during an incident.
func TestLogsPaneIsHonestAboutNotBeingWired(t *testing.T) {
	m := ready(t0)
	m.active = paneLogs
	out := m.View()
	for _, want := range []string{"not wired yet", "die with the container"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs pane should say %q:\n%s", want, out)
		}
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "connection refused" }

// A transport error carries the whole URL and easily exceeds the terminal
// width. Left to wrap, it pushes the footer off the screen — so the failure
// display breaks the frame at exactly the moment the frame is all the reader
// has.
func TestFailureLineFitsTheTerminal(t *testing.T) {
	m := ready(t0)
	m.width, m.height = 80, 20
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "n", State: "ready"}}, at: t0})
	m.now = t0.Add(47 * time.Second)
	m = send(m, fleetMsg{at: m.now, err: errLong})

	for _, line := range strings.Split(m.View(), "\n") {
		if w := len([]rune(stripANSI(line))); w > m.width {
			t.Fatalf("line exceeds terminal width (%d > %d): %q", w, m.width, line)
		}
	}
}

var errLong = &longErr{}

type longErr struct{}

func (*longErr) Error() string {
	return `Get "http://localhost:8417/v1/nodes?org=0d9658b1-cc7e-4b68-b50d-67c986a9b456": dial tcp [::1]:8417: connect: connection refused`
}

// stripANSI removes styling so widths are measured in printed columns. lipgloss
// emits escapes even when the profile is plain, and counting those as width
// would make this test pass on a line that visibly overflows.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// The home node is the point of the catalog endpoint for a multi-node fleet:
// "which machine holds this environment's data" is the first thing an operator
// asks, and before Sprint 5's client work no front end could answer it.
//
// Unplaced is a real state, not missing data — an environment is bound by its
// FIRST deployment — so the pane must say so rather than leave a blank that
// reads as a lookup that failed.
func TestEnvsPaneShowsHomeNodeAndNamesTheUnplaced(t *testing.T) {
	m := ready(t0)
	m.active = paneEnvs
	m = send(m, catalogMsg{at: t0, rows: []envRow{
		{App: "shop", Stack: "main", Env: "prod", EnvID: "e1", Hostname: "shop.example.com", HomeNode: "dev-node-3"},
		{App: "shop", Stack: "main", Env: "staging", EnvID: "e2"},
	}})

	out := m.View()
	if !strings.Contains(out, "dev-node-3") {
		t.Fatalf("placed environment must show its node:\n%s", out)
	}
	if !strings.Contains(out, "unplaced") {
		t.Fatalf("an environment with no home node must be named unplaced:\n%s", out)
	}
	if !strings.Contains(out, "NODE") {
		t.Fatalf("the pane needs a NODE column header:\n%s", out)
	}
}
