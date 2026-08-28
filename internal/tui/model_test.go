package tui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/craigderington/navarch/internal/client"
)

// The model is a pure state machine, so these drive it with messages and assert
// on the state that results. No terminal, no server, no sleeping.

var t0 = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func ready(now time.Time) Model {
	m := newModel("dev", now)
	m.orgID = "org-1"
	m.width, m.height = 120, 40
	return m
}

func send(m Model, msg tea.Msg) Model {
	out, _ := m.Update(msg)
	return out
}

// A failed refresh must not advance the timestamp. The data on screen really is
// as old as it was, and claiming otherwise turns "the control plane stopped
// answering" into "nothing is happening" — the two look identical on a
// dashboard, which is exactly when the distinction matters most.
func TestFailedRefreshKeepsTheOldTimestamp(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "n1"}}, at: t0})
	if m.fleet.updatedAt != t0 {
		t.Fatalf("first success should set updatedAt, got %v", m.fleet.updatedAt)
	}

	later := t0.Add(10 * time.Second)
	m.now = later
	m = send(m, fleetMsg{at: later, err: errors.New("connection refused")})

	if m.fleet.updatedAt != t0 {
		t.Fatalf("a failed refresh must not advance updatedAt: got %v, want %v", m.fleet.updatedAt, t0)
	}
	if m.fleet.err == nil {
		t.Fatal("the error must be retained for the status line")
	}
	if len(m.nodes) != 1 {
		t.Fatalf("previous data must survive a failed refresh, got %d nodes", len(m.nodes))
	}
}

func TestStaleAfterThreeIntervals(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "n1"}}, at: t0})

	if m.fleet.stale(t0.Add(fastInterval)) {
		t.Error("one interval old is not stale")
	}
	if !m.fleet.stale(t0.Add(staleAfter + time.Second)) {
		t.Error("past the stale threshold it must report stale")
	}
	// Never-loaded is stale by definition: there is nothing to trust yet.
	if !(section{}).stale(t0) {
		t.Error("a section that never loaded must be stale")
	}
}

// The polling policy is the thing that keeps this dashboard from becoming load
// on a control plane that is also scheduling. It is tested here rather than
// left implicit in the event loop.
func TestDuePollsCheapThingsOftenAndTheWalkRarely(t *testing.T) {
	m := ready(t0)
	// Nothing fetched yet: everything is due, including the walk, because the
	// first paint has to show something.
	f := m.due()
	if !f.health || !f.fleet || !f.events || !f.catalog {
		t.Fatalf("first pass should fetch everything, got %+v", f)
	}

	// Everything fresh, viewing the fleet: nothing due yet.
	m = send(m, healthMsg{ok: true, at: t0})
	m = send(m, fleetMsg{at: t0})
	m = send(m, eventsMsg{at: t0})
	m = send(m, catalogMsg{at: t0})
	m.now = t0.Add(time.Second)
	if f := m.due(); f.health || f.fleet || f.events || f.catalog {
		t.Fatalf("nothing should be due one second in, got %+v", f)
	}

	// One fast interval later the cheap ones are due; the walk is not.
	m.now = t0.Add(fastInterval)
	f = m.due()
	if !f.fleet || !f.events || !f.health {
		t.Errorf("cheap fetches should be due after the fast interval, got %+v", f)
	}
	if f.catalog {
		t.Error("the catalog walk must not run on the fast tier — it is 15+ requests")
	}

	// Even past the slow interval, the walk stays put while another pane is in
	// front. A background walk costs a request per stack for a pane nobody is
	// looking at.
	m.now = t0.Add(slowInterval + time.Second)
	if f := m.due(); f.catalog {
		t.Error("the walk must not run while its pane is not active")
	}
	m.active = paneEnvs
	if f := m.due(); !f.catalog {
		t.Error("the walk must run once its pane is active and the slow interval has passed")
	}
}

func TestDueNeverStacksRequests(t *testing.T) {
	m := ready(t0)
	f := m.due()
	m = m.markLoading(f)
	if f2 := m.due(); f2.fleet || f2.events || f2.health || f2.catalog {
		t.Fatalf("a fetch already in flight must not be reissued, got %+v", f2)
	}
	// The response clears it.
	m = send(m, fleetMsg{at: t0})
	m.now = t0.Add(fastInterval)
	if !m.due().fleet {
		t.Error("after the response lands the next interval should fetch again")
	}
}

// A response for a row the cursor has left must be dropped. Pairing one
// environment's name with another's revisions is a wrong that gets believed,
// because nothing on screen says the two came from different requests.
func TestLateDeploymentsForAnotherEnvironmentAreDropped(t *testing.T) {
	m := ready(t0)
	m = send(m, catalogMsg{rows: []envRow{
		{App: "a", Stack: "s", Env: "prod", EnvID: "env-1"},
		{App: "b", Stack: "s", Env: "prod", EnvID: "env-2"},
	}, at: t0})
	m.active = paneEnvs
	// Cursor on env-1; a response for env-2 arrives late.
	m = send(m, deploymentsMsg{envID: "env-2", list: []client.Deployment{{Revision: 9}}, at: t0})

	if m.deploymentsFor == "env-2" || len(m.deploymentList) != 0 {
		t.Fatalf("a response for an unselected environment must be dropped, got for=%q n=%d",
			m.deploymentsFor, len(m.deploymentList))
	}
	// The matching one is kept.
	m = send(m, deploymentsMsg{envID: "env-1", list: []client.Deployment{{Revision: 3}}, at: t0})
	if m.deploymentsFor != "env-1" || len(m.deploymentList) != 1 {
		t.Fatalf("the selected environment's response must be kept, got for=%q n=%d",
			m.deploymentsFor, len(m.deploymentList))
	}
}

// An environment reaped between two polls, or a node removed, shrinks the list
// under the cursor. That is routine on this platform — previews expire on a
// timer — so it must not index out of range or leave the cursor pointing past
// the end.
func TestCursorSurvivesAListShrinking(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "a"}, {Hostname: "b"}, {Hostname: "c"}}, at: t0})
	m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}) // last row
	if m.cursor[paneFleet] != 2 {
		t.Fatalf("cursor should be on the last row, got %d", m.cursor[paneFleet])
	}

	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "a"}}, at: t0})
	if m.cursor[paneFleet] != 0 {
		t.Fatalf("cursor must clamp into the shrunken list, got %d", m.cursor[paneFleet])
	}
	// And an empty list must not leave a cursor pointing at a row.
	m = send(m, fleetMsg{nodes: nil, at: t0})
	if m.cursor[paneFleet] != 0 {
		t.Fatalf("cursor must be 0 for an empty list, got %d", m.cursor[paneFleet])
	}
	if got := m.selectedEnvID(); got != "" {
		t.Fatalf("no selection in an empty catalog, got %q", got)
	}
}

func TestPaneNavigation(t *testing.T) {
	m := ready(t0)
	if m.active != paneFleet {
		t.Fatal("should open on the fleet")
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.active != paneEnvs {
		t.Fatalf("tab should advance, got %v", m.active)
	}
	// Wraps rather than sticking at the end.
	for i := 0; i < int(paneCount); i++ {
		m = send(m, tea.KeyMsg{Type: tea.KeyTab})
	}
	if m.active != paneEnvs {
		t.Fatalf("tab should wrap around, got %v", m.active)
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.active != paneFleet {
		t.Fatalf("shift-tab should go back, got %v", m.active)
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if m.active != paneEvents {
		t.Fatalf("number keys should jump, got %v", m.active)
	}
}

func TestCursorDoesNotRunOffEitherEnd(t *testing.T) {
	m := ready(t0)
	m = send(m, fleetMsg{nodes: []client.Node{{Hostname: "a"}, {Hostname: "b"}}, at: t0})
	for i := 0; i < 5; i++ {
		m = send(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.cursor[paneFleet] != 1 {
		t.Fatalf("cursor must stop at the last row, got %d", m.cursor[paneFleet])
	}
	for i := 0; i < 5; i++ {
		m = send(m, tea.KeyMsg{Type: tea.KeyUp})
	}
	if m.cursor[paneFleet] != 0 {
		t.Fatalf("cursor must stop at the first row, got %d", m.cursor[paneFleet])
	}
}

func TestRefreshKeyMakesEverythingDueAtOnce(t *testing.T) {
	m := ready(t0)
	m = send(m, healthMsg{ok: true, at: t0})
	m = send(m, fleetMsg{at: t0})
	m = send(m, eventsMsg{at: t0})
	m = send(m, catalogMsg{at: t0})
	m.now = t0.Add(time.Second)
	if f := m.due(); f.fleet || f.events {
		t.Fatal("precondition: nothing should be due")
	}

	m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	f := m.due()
	if !f.health || !f.fleet || !f.events {
		t.Fatalf("r must make the cheap fetches due immediately, got %+v", f)
	}
}

// The TUI is read-only by design, and this pins it. Every key that would name a
// destructive action in the CLI must do nothing here — no state change, no
// command. Someone adding "just one" action has to delete this test to do it,
// which is the point.
func TestNoKeyPerformsAnAction(t *testing.T) {
	base := ready(t0)
	base = send(base, catalogMsg{rows: []envRow{{App: "a", Stack: "s", Env: "prod", EnvID: "env-1"}}, at: t0})
	base.active = paneEnvs

	for _, key := range []rune{'d', 'D', 'p', 'P', 'R', 'x', 'X', 'u', 'U', '\n'} {
		got, cmd := base.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		if cmd != nil {
			t.Errorf("key %q produced a command; the TUI must not act", string(key))
		}
		if got.active != base.active || got.cursor != base.cursor || got.quitting {
			t.Errorf("key %q changed state; expected it to be inert", string(key))
		}
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyEsc},
	} {
		m, cmd := ready(t0).Update(k)
		if !m.quitting {
			t.Errorf("%v should quit", k)
		}
		if cmd == nil {
			t.Errorf("%v should return tea.Quit", k)
		}
	}
}

// An org slug that does not resolve is fatal rather than retried: it will not
// start existing, and every other request needs the id.
func TestUnresolvableOrgIsFatal(t *testing.T) {
	m := newModel("nope", t0)
	m = send(m, orgMsg{err: errors.New("no organization with slug \"nope\"")})
	if m.fatal == nil {
		t.Fatal("an unresolvable org must be fatal")
	}
	if f := m.due(); f.fleet || f.events || f.catalog {
		t.Fatalf("nothing is addressable without an org id, got %+v", f)
	}
}
