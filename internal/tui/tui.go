// Package tui is the read-only terminal dashboard for the Navarch control
// plane. It observes and never acts: every destructive operation stays in the
// CLI, where it is explicit, scriptable and reviewable.
//
// It is a second consumer of internal/client, never a second protocol. Nothing
// here knows a URL or a JSON shape.
package tui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/craigderington/navarch/internal/client"
)

// Options configures a session. Zero values are usable: the org defaults to the
// bootstrapped dev org and the refresh interval to the fast tier.
type Options struct {
	// Org is the organization slug to observe.
	Org string
	// Refresh overrides the fast-tier poll interval. The catalog walk keeps its
	// own slower cadence regardless — it is the expensive one, and letting a
	// flag drive it to a second would put double-digit request rates on the
	// control plane from a screen nobody is reading that fast.
	Refresh time.Duration
	// LogFile, when set, is where diagnostics go. See the note in Run: a TUI
	// owns the screen, so the default is to discard them.
	LogFile string
}

// Run takes over the terminal until the user quits or ctx is cancelled.
//
// The client is passed in rather than constructed here so the caller keeps
// ownership of URL, token and config precedence — the CLI already resolves all
// three, and a second resolution path would be a second set of rules to keep
// in step.
func Run(ctx context.Context, c *client.Client, opts Options) error {
	if c == nil {
		return fmt.Errorf("tui: client is required")
	}
	org := opts.Org
	if org == "" {
		org = "dev"
	}
	interval := opts.Refresh
	if interval <= 0 {
		interval = fastInterval
	}

	// Logging while owning the screen: slog's default handler writes to stderr,
	// which is the terminal this program is drawing on — one log line would
	// corrupt the frame and there is no way to redraw over it reliably. So
	// diagnostics are discarded unless a file is named. Errors are not lost by
	// this: every request failure is surfaced in the status line, which is
	// where an operator is already looking, rather than in a stream they cannot
	// see.
	logOut := io.Discard
	if opts.LogFile != "" {
		f, err := os.OpenFile(opts.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("tui: open log file: %w", err)
		}
		defer f.Close()
		logOut = f
	}
	log := slog.New(slog.NewTextHandler(logOut, nil))

	a := app{
		Model:    newModel(org, time.Now()),
		c:        c,
		interval: interval,
		log:      log,
	}
	p := tea.NewProgram(a, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// app is the bubbletea adapter. The state machine lives on Model, which is free
// of the client and the terminal; this type is the only place the two meet, and
// it exists so Model.Update can stay a pure function that tests can drive.
type app struct {
	Model
	c        *client.Client
	interval time.Duration
	log      *slog.Logger
}

func (a app) Init() tea.Cmd {
	return tea.Batch(tickCmd(a.interval), resolveOrgCmd(a.c, a.orgSlug), healthCmd(a.c))
}

func (a app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := a.Model.Update(msg)
	a.Model = m

	var cmds []tea.Cmd
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if _, isTick := msg.(tickMsg); isTick {
		cmds = append(cmds, tickCmd(a.interval))
	}

	// due() is consulted after every message, not only on the tick, so a manual
	// refresh or a pane switch acts immediately instead of waiting out the
	// interval. The loading flags are what stop that turning into a request per
	// keystroke.
	if !a.quitting && a.fatal == nil {
		f := a.Model.due()
		a.Model = a.Model.markLoading(f)
		cmds = append(cmds, a.fetch(f)...)
	}
	return a, tea.Batch(cmds...)
}

func (a app) View() string { return a.Model.View() }

func (a app) fetch(f fetches) []tea.Cmd {
	var cmds []tea.Cmd
	if f.health {
		cmds = append(cmds, healthCmd(a.c))
	}
	if f.fleet {
		cmds = append(cmds, fleetCmd(a.c, a.orgID))
	}
	if f.events {
		cmds = append(cmds, eventsCmd(a.c, a.orgID, eventLimit))
	}
	if f.catalog {
		a.log.Debug("walking catalog", "org", a.orgID)
		cmds = append(cmds, catalogCmd(a.c, a.orgID))
	}
	if f.deploymentsFor != "" {
		cmds = append(cmds, deploymentsCmd(a.c, f.deploymentsFor))
	}
	return cmds
}

// eventLimit is what the events pane asks for. Bounded because the endpoint is
// cursor-paginated and a dashboard has no business pulling the whole audit
// history on every poll.
const eventLimit = 100
