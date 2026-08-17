package tui

import (
	"context"
	"fmt"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/craig/composectl/internal/client"
)

// Every request the TUI makes goes through internal/client. Nothing here
// constructs a URL or decodes a body — that boundary is why a second front end
// was cheap to build, and it stops being cheap the moment one of them starts
// speaking HTTP on its own and the two drift.

// Per-request timeouts. A TUI that blocks on a hung control plane stops
// redrawing, which looks exactly like a crash; failing fast and reporting it in
// the status line keeps the session alive and honest.
const (
	quickTimeout = 5 * time.Second
	walkTimeout  = 30 * time.Second
)

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func resolveOrgCmd(c *client.Client, slug string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
		defer cancel()
		orgs, err := c.ListOrgs(ctx)
		if err != nil {
			return orgMsg{err: err}
		}
		for _, o := range orgs {
			if o.Slug == slug {
				return orgMsg{id: o.ID, at: time.Now()}
			}
		}
		// Fatal rather than retried: a slug that does not exist will not start
		// existing, and every other request needs the id.
		return orgMsg{err: fmt.Errorf("no organization with slug %q", slug)}
	}
}

func healthCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
		defer cancel()
		h, err := c.Health(ctx)
		if err != nil {
			return healthMsg{at: time.Now(), err: err}
		}
		return healthMsg{ok: h.Status == "ok", at: time.Now()}
	}
}

func fleetCmd(c *client.Client, orgID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
		defer cancel()
		nodes, err := c.ListNodes(ctx, orgID)
		if err != nil {
			return fleetMsg{at: time.Now(), err: err}
		}
		// Sorted by hostname so a node does not jump rows between polls and
		// move the cursor out from under whoever is reading it.
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Hostname < nodes[j].Hostname })
		return fleetMsg{nodes: nodes, at: time.Now()}
	}
}

func eventsCmd(c *client.Client, orgID string, limit int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
		defer cancel()
		evs, err := c.ListEvents(ctx, orgID, limit, 0)
		if err != nil {
			return eventsMsg{at: time.Now(), err: err}
		}
		return eventsMsg{events: evs, at: time.Now()}
	}
}

func deploymentsCmd(c *client.Client, envID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
		defer cancel()
		ds, err := c.ListDeployments(ctx, envID)
		if err != nil {
			return deploymentsMsg{envID: envID, at: time.Now(), err: err}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].Revision > ds[j].Revision })
		return deploymentsMsg{envID: envID, list: ds, at: time.Now()}
	}
}

// catalogCmd walks apps → stacks → environments.
//
// There is no endpoint that lists an organization's environments, so this is
// one request per app plus one per stack — 15 against the dev fleet, growing
// with the catalog. That cost is the whole reason the walk runs on the slow
// tier and only while its pane is in front.
//
// Sequential on purpose: the control plane is also running the scheduler,
// controller and reaper loops, and a burst of parallel requests from a
// dashboard is a poor trade for latency on a screen that refreshes every twenty
// seconds. A partial failure returns what it has rather than nothing — a
// catalog missing one app is more useful than an empty pane, and the error is
// still reported.
func catalogCmd(c *client.Client, orgID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), walkTimeout)
		defer cancel()

		apps, err := c.ListApps(ctx, orgID)
		if err != nil {
			return catalogMsg{at: time.Now(), err: err}
		}
		var rows []envRow
		var firstErr error
		for _, app := range apps {
			stacks, err := c.ListStacks(ctx, app.ID)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, st := range stacks {
				envs, err := c.ListEnvs(ctx, st.ID)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				for _, e := range envs {
					rows = append(rows, envRow{
						App: app.Slug, Stack: st.Slug, Env: e.Slug,
						EnvID:    e.ID,
						Hostname: e.Hostname,
						// LiveDeploymentID is the environment's own record of
						// what is serving, so "has a live deployment" costs
						// nothing extra here — the revisions themselves are
						// fetched only for the selected row.
						HasLive:   e.LiveDeploymentID != nil && *e.LiveDeploymentID != "",
						Ephemeral: e.Ephemeral,
						ExpiresAt: e.ExpiresAt,
					})
				}
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].App != rows[j].App {
				return rows[i].App < rows[j].App
			}
			if rows[i].Stack != rows[j].Stack {
				return rows[i].Stack < rows[j].Stack
			}
			return rows[i].Env < rows[j].Env
		})
		return catalogMsg{rows: rows, at: time.Now(), err: firstErr}
	}
}
