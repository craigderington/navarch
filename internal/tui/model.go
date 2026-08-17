package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/craig/composectl/internal/client"
)

// The model is deliberately free of both the terminal and the client: every
// value it holds arrives as a message, and every request it wants leaves as a
// tea.Cmd. That is what lets the whole state machine be tested by constructing
// a Model and sending it messages, with no server and no TTY — which matters
// here because a TUI that is only testable by looking at it is a TUI nobody
// tests.

type pane int

const (
	paneFleet pane = iota
	paneEnvs
	paneEvents
	paneLogs
	paneCount
)

func (p pane) title() string {
	switch p {
	case paneFleet:
		return "Fleet"
	case paneEnvs:
		return "Environments"
	case paneEvents:
		return "Events"
	case paneLogs:
		return "Logs"
	}
	return "?"
}

// Refresh cadence is tiered because the cost of the panes is not equal. Fleet,
// events and health are one request each; the environment catalog is a walk of
// apps → stacks → envs and grows with the catalog (15 requests against the dev
// fleet at the time of writing). Polling the walk on the fast tier would put
// steady double-digit request rates on a control plane that is also running the
// scheduler, controller and reaper loops.
const (
	fastInterval = 3 * time.Second
	slowInterval = 20 * time.Second
	// staleAfter is when a pane stops being presented as current. It is a
	// multiple of the interval rather than equal to it, so an ordinary late
	// response does not make the display flicker between fresh and stale.
	staleAfter = 3 * fastInterval
)

// section is the freshness envelope around every polled thing. A TUI that
// renders data without saying when it arrived is at its most dangerous exactly
// when it is most wrong: a control plane that stopped answering looks identical
// to one where nothing is happening.
type section struct {
	updatedAt time.Time
	err       error
	loading   bool
}

func (s section) stale(now time.Time) bool {
	return s.updatedAt.IsZero() || now.Sub(s.updatedAt) > staleAfter
}

// envRow is one row of the environment catalog, flattened from the walk so the
// pane can render without holding the tree.
type envRow struct {
	App      string
	Stack    string
	Env      string
	EnvID    string
	Hostname string
	// HasLive distinguishes "deployed and serving" from "created but never
	// rolled out", which is the difference between an environment that is
	// broken and one that is merely empty.
	HasLive   bool
	Ephemeral bool
	ExpiresAt *time.Time
}

type Model struct {
	orgSlug string
	orgID   string

	width, height int
	// now is carried on the model rather than read from the clock during
	// render, so tests can place the model at a moment and assert on what it
	// says about staleness.
	now time.Time

	active pane
	cursor [paneCount]int

	health    section
	healthOK  bool
	fleet     section
	nodes     []client.Node
	events    section
	eventList []client.Event
	catalog   section
	envs      []envRow

	// deployments belong to whichever environment the cursor is on; they are
	// fetched per selection rather than for the whole catalog, which would
	// multiply the walk by another request per environment.
	deployments    section
	deploymentList []client.Deployment
	deploymentsFor string

	quitting bool
	// fatal is set only for a condition the TUI cannot poll its way out of.
	// Everything else — a timeout, a restarting control plane — belongs in a
	// section's err, where it is visible without ending the session.
	fatal error
}

func newModel(orgSlug string, now time.Time) Model {
	return Model{orgSlug: orgSlug, now: now, active: paneFleet}
}

// ---- messages ----

type tickMsg time.Time

type healthMsg struct {
	ok  bool
	at  time.Time
	err error
}

type orgMsg struct {
	id  string
	at  time.Time
	err error
}

type fleetMsg struct {
	nodes []client.Node
	at    time.Time
	err   error
}

type eventsMsg struct {
	events []client.Event
	at     time.Time
	err    error
}

type catalogMsg struct {
	rows []envRow
	at   time.Time
	err  error
}

type deploymentsMsg struct {
	envID string
	list  []client.Deployment
	at    time.Time
	err   error
}

// ---- update ----

func (m Model) Init() tea.Cmd { return nil } // Run wires the initial commands.

func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)

	case tickMsg:
		m.now = time.Time(msg)
		return m, nil // Run decides what to fetch; see due().

	case orgMsg:
		if msg.err != nil {
			m.fatal = msg.err // without an org id nothing else can be asked for
			return m, nil
		}
		m.orgID = msg.id
		return m, nil

	case healthMsg:
		m.health = settle(m.health, msg.at, msg.err)
		m.healthOK = msg.err == nil && msg.ok
		return m, nil

	case fleetMsg:
		m.fleet = settle(m.fleet, msg.at, msg.err)
		if msg.err == nil {
			m.nodes = msg.nodes
			m.cursor[paneFleet] = clamp(m.cursor[paneFleet], len(m.nodes))
		}
		return m, nil

	case eventsMsg:
		m.events = settle(m.events, msg.at, msg.err)
		if msg.err == nil {
			m.eventList = msg.events
			m.cursor[paneEvents] = clamp(m.cursor[paneEvents], len(m.eventList))
		}
		return m, nil

	case catalogMsg:
		m.catalog = settle(m.catalog, msg.at, msg.err)
		if msg.err == nil {
			m.envs = msg.rows
			m.cursor[paneEnvs] = clamp(m.cursor[paneEnvs], len(m.envs))
		}
		return m, nil

	case deploymentsMsg:
		// A response for an environment the cursor has since left is dropped
		// rather than shown: arriving late is not the same as being current,
		// and pairing one environment's name with another's revisions is the
		// kind of wrong that gets believed.
		if msg.envID != m.selectedEnvID() {
			return m, nil
		}
		m.deployments = settle(m.deployments, msg.at, msg.err)
		if msg.err == nil {
			m.deploymentList = msg.list
			m.deploymentsFor = msg.envID
		}
		return m, nil
	}
	return m, nil
}

// settle folds a response into a section. A failed refresh keeps the previous
// updatedAt: the data on screen really is that old, and advancing the timestamp
// on failure would claim a freshness the request did not deliver.
func settle(s section, at time.Time, err error) section {
	s.loading = false
	s.err = err
	if err == nil {
		s.updatedAt = at
	}
	return s
}

func (m Model) key(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit
	case "tab", "right", "l":
		m.active = (m.active + 1) % paneCount
		return m, nil
	case "shift+tab", "left", "h":
		m.active = (m.active + paneCount - 1) % paneCount
		return m, nil
	case "1":
		m.active = paneFleet
		return m, nil
	case "2":
		m.active = paneEnvs
		return m, nil
	case "3":
		m.active = paneEvents
		return m, nil
	case "4":
		m.active = paneLogs
		return m, nil
	case "down", "j":
		m.cursor[m.active] = move(m.cursor[m.active], 1, m.rows(m.active))
		return m, nil
	case "up", "k":
		m.cursor[m.active] = move(m.cursor[m.active], -1, m.rows(m.active))
		return m, nil
	case "g", "home":
		m.cursor[m.active] = 0
		return m, nil
	case "G", "end":
		m.cursor[m.active] = clamp(m.rows(m.active)-1, m.rows(m.active))
		return m, nil
	case "r":
		// Manual refresh marks everything due; Run's next tick picks it up.
		// Zeroing updatedAt rather than issuing commands here keeps Update free
		// of the client, which is what makes it testable.
		m.health.updatedAt = time.Time{}
		m.fleet.updatedAt = time.Time{}
		m.events.updatedAt = time.Time{}
		m.catalog.updatedAt = time.Time{}
		m.deployments.updatedAt = time.Time{}
		return m, nil
	}
	return m, nil
}

func (m Model) rows(p pane) int {
	switch p {
	case paneFleet:
		return len(m.nodes)
	case paneEnvs:
		return len(m.envs)
	case paneEvents:
		return len(m.eventList)
	}
	return 0
}

func (m Model) selectedEnvID() string {
	if len(m.envs) == 0 {
		return ""
	}
	i := clamp(m.cursor[paneEnvs], len(m.envs))
	return m.envs[i].EnvID
}

// clamp keeps a cursor inside a list that may have shrunk under it — an
// environment reaped between two polls is routine, not exceptional.
func clamp(i, n int) int {
	if n <= 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	if i < 0 {
		return 0
	}
	return i
}

func move(i, delta, n int) int {
	if n <= 0 {
		return 0
	}
	return clamp(i+delta, n)
}

// due reports which fetches should run now. Keeping this a pure function of the
// model means the polling policy — what is cheap, what is expensive, what only
// matters while you are looking at it — is testable rather than tangled into
// the event loop.
type fetches struct {
	health, fleet, events, catalog bool
	deploymentsFor                 string
}

func (m Model) due() fetches {
	var f fetches
	if m.orgID == "" {
		return f // nothing is addressable until the org resolves
	}
	f.health = elapsed(m.health, m.now, fastInterval)
	f.fleet = elapsed(m.fleet, m.now, fastInterval)
	f.events = elapsed(m.events, m.now, fastInterval)
	// The catalog walk runs on the slow tier, and only while its pane is in
	// front. An operator watching the fleet has no use for a background walk
	// that costs a request per stack.
	if m.active == paneEnvs || m.catalog.updatedAt.IsZero() {
		f.catalog = elapsed(m.catalog, m.now, slowInterval)
	}
	// Deployments follow the cursor: fetch when the selection moved to an
	// environment we do not have, or when what we have has gone stale.
	if m.active == paneEnvs {
		if id := m.selectedEnvID(); id != "" {
			if id != m.deploymentsFor || elapsed(m.deployments, m.now, fastInterval) {
				f.deploymentsFor = id
			}
		}
	}
	return f
}

func elapsed(s section, now time.Time, every time.Duration) bool {
	if s.loading {
		return false // never stack a second request on an outstanding one
	}
	return s.updatedAt.IsZero() || now.Sub(s.updatedAt) >= every
}

// markLoading records that the fetches in f are in flight, so due() will not
// ask again while they are outstanding.
func (m Model) markLoading(f fetches) Model {
	if f.health {
		m.health.loading = true
	}
	if f.fleet {
		m.fleet.loading = true
	}
	if f.events {
		m.events.loading = true
	}
	if f.catalog {
		m.catalog.loading = true
	}
	if f.deploymentsFor != "" {
		m.deployments.loading = true
	}
	return m
}
