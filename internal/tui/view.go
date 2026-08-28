package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/craig/composectl/internal/client"
)

// Colours are ANSI palette indices rather than hex, so they resolve through the
// user's own terminal theme. A hard-coded hex pair looks deliberate on the
// machine it was chosen on and unreadable on a light background elsewhere, and
// this is an operations tool that has to be legible wherever it is opened.
var (
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleHead    = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
	styleSel     = lipgloss.NewStyle().Reverse(true)
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleTabOn   = lipgloss.NewStyle().Bold(true).Underline(true)
	styleTabOff  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleTitleHi = lipgloss.NewStyle().Bold(true)
)

// stateStyle maps a platform state to a colour. Deployment and node states
// share this on purpose — "failed" and "unreachable" should look the same
// because they mean the same thing to whoever is looking: this one needs you.
func stateStyle(s string) lipgloss.Style {
	switch s {
	case "live", "ready", "healthy":
		return styleOK
	case "failed", "unreachable":
		return styleBad
	case "pending", "scheduling", "starting", "draining":
		return styleWarn
	case "superseded", "stopped", "retired":
		return styleDim
	}
	return lipgloss.NewStyle()
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if m.fatal != nil {
		return fmt.Sprintf("\n  cannot start: %v\n\n  press q to quit\n", m.fatal)
	}
	// Bubble Tea sends the first WindowSizeMsg after the program starts, so
	// there is a real moment with no dimensions. Rendering a table into a
	// zero-width terminal is where a TUI panics on a slice bound.
	if m.width < 20 || m.height < 6 {
		return "terminal too small"
	}

	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	b.WriteString(m.tabs())
	b.WriteString("\n\n")

	// Body height is what is left after the chrome. Panes render at most this
	// many rows so the footer cannot be pushed off the screen.
	body := m.height - 6
	if body < 1 {
		body = 1
	}
	switch m.active {
	case paneFleet:
		b.WriteString(m.fleetPane(body))
	case paneEnvs:
		b.WriteString(m.envsPane(body))
	case paneEvents:
		b.WriteString(m.eventsPane(body))
	case paneLogs:
		b.WriteString(m.logsPane())
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	left := styleTitleHi.Render("navarch") + styleDim.Render("  org "+m.orgSlug)

	var right string
	switch {
	case m.health.err != nil:
		right = styleBad.Render("● control plane unreachable")
	case m.healthOK:
		right = styleOK.Render("● healthy")
	default:
		right = styleWarn.Render("● connecting")
	}
	// The age of the freshest thing on screen, so the header answers "is this
	// moving?" without the reader having to check each pane.
	if !m.fleet.updatedAt.IsZero() {
		right += styleDim.Render("  updated " + agoPhrase(m.now.Sub(m.fleet.updatedAt)))
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) tabs() string {
	parts := make([]string, 0, paneCount)
	for p := pane(0); p < paneCount; p++ {
		label := fmt.Sprintf(" %d %s ", int(p)+1, p.title())
		if p == m.active {
			parts = append(parts, styleTabOn.Render(label))
		} else {
			parts = append(parts, styleTabOff.Render(label))
		}
	}
	return strings.Join(parts, styleDim.Render("│"))
}

func (m Model) footer() string {
	keys := styleDim.Render("1-4/tab pane · j/k move · r refresh · q quit")
	if s := m.paneStatus(); s != "" {
		return keys + "\n" + s
	}
	return keys
}

// paneStatus is where a failing refresh becomes visible. It names the pane's
// own freshness rather than a global one, because a stale fleet and a stale
// catalog mean different things and are refreshed on different cadences.
func (m Model) paneStatus() string {
	var s section
	switch m.active {
	case paneFleet:
		s = m.fleet
	case paneEnvs:
		s = m.catalog
	case paneEvents:
		s = m.events
	default:
		return ""
	}
	if s.err != nil {
		last := "never"
		if !s.updatedAt.IsZero() {
			last = agoPhrase(m.now.Sub(s.updatedAt))
		}
		// A transport error carries the whole URL and easily exceeds the
		// terminal width. Left to wrap it pushes the layout down a line and the
		// footer off the screen, so the failure display breaks the frame at
		// exactly the moment the frame is all the reader has.
		suffix := fmt.Sprintf("  (showing data from %s, retrying)", last)
		budget := maxInt(20, m.width-len(suffix)-len("refresh failed: "))
		return styleBad.Render("refresh failed: "+truncate(s.err.Error(), budget)) +
			styleDim.Render(suffix)
	}
	if s.stale(m.now) && !s.updatedAt.IsZero() {
		return styleWarn.Render("stale: last updated " + agoPhrase(m.now.Sub(s.updatedAt)))
	}
	if s.updatedAt.IsZero() {
		return styleDim.Render("loading…")
	}
	return ""
}

func (m Model) fleetPane(maxRows int) string {
	if len(m.nodes) == 0 {
		return styleDim.Render("  no nodes registered")
	}
	// Column widths are fixed rather than measured: the fields are known and
	// bounded, and a layout that reflows on every poll is harder to read than
	// one that is occasionally truncated.
	head := "  " + styleHead.Render(
		pad("HOSTNAME", 16)+pad("STATE", 12)+pad("ADDRESS", 15)+
			pad("CPU", 6)+pad("MEM", 6)+pad("HEARTBEAT", 10)+"LABELS")
	rows := make([]string, 0, len(m.nodes))
	for i, n := range m.nodes {
		hb := "never"
		if n.LastHeartbeat != nil {
			hb = agoPhrase(m.now.Sub(*n.LastHeartbeat))
		}
		line := pad(n.Hostname, 16) +
			stateStyle(n.State).Render(pad(n.State, 12)) +
			pad(n.AdvertiseAddr, 15) +
			pad(ratio(int64(n.AllocCPUMillis), int64(n.CPUMillis)), 6) +
			pad(ratio(n.AllocMemoryBytes, n.MemoryBytes), 6) +
			pad(hb, 10) +
			labels(n.Labels)
		// A node advertising an unapproved age key cannot open secrets sealed
		// to the old one, so its next rollout there fails. It is a waiting
		// operator decision, and an operator watching this dashboard is exactly
		// who has to make it — silence here would mean noticing at the failure
		// instead of before it.
		if n.PendingAgeRecipient != "" {
			line += styleWarn.Render("  ⚠ key rotation pending")
		}
		rows = append(rows, m.cursorLine(paneFleet, i, line))
	}
	return head + "\n" + window(rows, m.cursor[paneFleet], maxRows-1)
}

func (m Model) envsPane(maxRows int) string {
	if len(m.envs) == 0 {
		if m.catalog.updatedAt.IsZero() {
			return styleDim.Render("  walking the catalog…")
		}
		return styleDim.Render("  no environments")
	}
	// The pane is split: the catalog above, the selected environment's
	// revisions below. Rollout progress only means something next to the
	// environment it belongs to.
	listRows := maxRows - 7
	if listRows < 2 {
		listRows = 2
	}
	head := "  " + styleHead.Render(pad("APP/STACK/ENV", 36)+pad("HOSTNAME", 28)+pad("NODE", 13)+pad("LIVE", 6)+"TTL")
	rows := make([]string, 0, len(m.envs))
	for i, e := range m.envs {
		live := styleDim.Render(pad("no", 6))
		if e.HasLive {
			live = styleOK.Render(pad("yes", 6))
		}
		ttl := ""
		if e.Ephemeral {
			ttl = styleDim.Render("preview")
			if e.ExpiresAt != nil {
				ttl = styleWarn.Render("expires in " + age(e.ExpiresAt.Sub(m.now)))
			}
		}
		// An unplaced environment is a real state, not missing data: nothing is
		// bound until the first deployment. Dimming it says "not yet" rather
		// than letting a blank column read as a failed lookup.
		node := styleDim.Render(pad("unplaced", 13))
		if e.HomeNode != "" {
			node = pad(e.HomeNode, 13)
		}
		line := pad(e.App+"/"+e.Stack+"/"+e.Env, 36) + pad(orDash(e.Hostname), 28) + node + live + ttl
		rows = append(rows, m.cursorLine(paneEnvs, i, line))
	}
	return head + "\n" + window(rows, m.cursor[paneEnvs], listRows) + "\n\n" + m.revisions()
}

// revisions renders the selected environment's deployment history — the
// closest thing the read model has to rollout progress.
func (m Model) revisions() string {
	sel := m.selectedEnvID()
	if sel == "" {
		return ""
	}
	title := styleHead.Render("  REVISIONS")
	if m.deployments.err != nil {
		return title + "\n" + styleBad.Render("  "+m.deployments.err.Error())
	}
	if m.deploymentsFor != sel {
		return title + "\n" + styleDim.Render("  loading…")
	}
	if len(m.deploymentList) == 0 {
		return title + "\n" + styleDim.Render("  never deployed")
	}
	var b strings.Builder
	b.WriteString(title + "\n")
	for i, d := range m.deploymentList {
		if i >= 4 { // newest few; the CLI is where full history belongs
			b.WriteString(styleDim.Render(fmt.Sprintf("  … %d older", len(m.deploymentList)-i)))
			break
		}
		line := fmt.Sprintf("  r%-4d %s %s %s",
			d.Revision, pad(d.Slot, 7),
			stateStyle(d.State).Render(pad(d.State, 12)),
			styleDim.Render(agoPhrase(m.now.Sub(d.UpdatedAt))))
		if d.FailureReason != "" {
			line += styleBad.Render("  " + truncate(d.FailureReason, maxInt(10, m.width-50)))
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) eventsPane(maxRows int) string {
	if len(m.eventList) == 0 {
		return styleDim.Render("  no events")
	}
	// Without a target column every row reads "deployment entered starting"
	// with no way to tell which deployment, which makes a busy timeline
	// unusable exactly when several rollouts overlap.
	head := "  " + styleHead.Render(pad("WHEN", 12)+pad("TARGET", 10)+pad("KIND", 26)+"MESSAGE")
	rows := make([]string, 0, len(m.eventList))
	for i, e := range m.eventList {
		msgWidth := maxInt(10, m.width-54)
		line := pad(agoPhrase(m.now.Sub(e.CreatedAt)), 12) +
			pad(eventTarget(e), 10) +
			pad(e.Kind, 26) +
			truncate(e.Message, msgWidth)
		rows = append(rows, m.cursorLine(paneEvents, i, line))
	}
	return head + "\n" + window(rows, m.cursor[paneEvents], maxRows-1)
}

// logsPane is a placeholder. The log API it will consume is Slice A of this
// sprint and does not exist yet; inventing a client method here would fork the
// protocol, which is the one thing the client boundary exists to prevent.
func (m Model) logsPane() string {
	return styleDim.Render(`  Logs are not wired yet.

  This pane will show container logs once Sprint 5 Slice A lands the
  on-demand log path: the control plane records a request, the agent
  picks it up on its next poll and posts the chunk back.

  Note when it arrives: logs are fetched, not stored. A container's
  logs die with the container.`)
}

// cursorLine marks the selected row, and only in the pane that has focus —
// showing a cursor in a pane you are not driving invites acting on it.
func (m Model) cursorLine(p pane, i int, line string) string {
	if m.active == p && m.cursor[p] == i {
		return styleSel.Render("▸ " + line)
	}
	return "  " + line
}

// window scrolls a row set to keep the cursor visible. Without it a fleet
// larger than the terminal simply hides its tail, and the rows that scroll off
// are the ones an operator most needs when something is wrong.
func window(rows []string, cursor, height int) string {
	if height < 1 {
		height = 1
	}
	if len(rows) <= height {
		return strings.Join(rows, "\n")
	}
	start := cursor - height/2
	if start < 0 {
		start = 0
	}
	if start+height > len(rows) {
		start = len(rows) - height
	}
	out := rows[start : start+height]
	tail := ""
	if start+height < len(rows) {
		tail = "\n" + styleDim.Render(fmt.Sprintf("  … %d more", len(rows)-start-height))
	}
	return strings.Join(out, "\n") + tail
}

func labels(l map[string]string) string {
	if len(l) == 0 {
		return styleDim.Render("-")
	}
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	// Sorted so two consecutive polls of an unchanged node render identically;
	// map order would otherwise make the row appear to change on every refresh.
	return strings.Join(sorted(parts), ",")
}

// eventTarget names what an event is about, in the same 8-character form the
// platform uses for env8 in container names, so an id on screen matches an id
// on a node. Org-scoped events have neither and get a dash rather than a blank,
// which would read as a rendering fault.
func eventTarget(e client.Event) string {
	switch {
	case e.DeploymentID != nil && *e.DeploymentID != "":
		return shortID(*e.DeploymentID)
	case e.NodeID != nil && *e.NodeID != "":
		return shortID(*e.NodeID)
	}
	return "-"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

var _ = time.Time{}
