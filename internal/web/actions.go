package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/craigderington/navarch/internal/client"
)

// Actions: the console's mutating half.
//
// `navarch tui` stays read-only and TestNoKeyPerformsAnAction stays with it.
// That rule is about a full-screen terminal app, where a keystroke is one
// character away from an accident and there is nowhere to put a confirmation
// that does not also become a keystroke. A web form is a different interaction:
// a deliberate click, on a page that names the exact object and says what will
// happen, submitting a token the page had to be served to know.

// action describes one mutating operation: what it is called, what it does to
// what, and how dangerous it is. Kept as data because the confirmation page,
// the routing and the audit-facing wording all have to agree, and three
// hand-written copies of the same sentence drift.
type action struct {
	Key     string // stable id used in URLs
	Verb    string // the button
	Title   string // the confirmation heading
	Explain string // what actually happens, in the operator's terms
	Danger  bool   // needs the stronger styling and the plainer warning
	Path    func(id string) string
}

var actions = map[string]action{
	"deploy": {
		Key: "deploy", Verb: "Deploy", Title: "Deploy the latest version",
		Explain: "Creates a new revision from this stack's most recent version and rolls it out. " +
			"The current revision keeps serving until the new one is healthy.",
		Path: func(id string) string { return "/envs/" + id + "/deploy" },
	},
	"promote": {
		Key: "promote", Verb: "Promote", Title: "Promote this revision",
		Explain: "Makes this revision live and supersedes the one serving now. " +
			"Traffic moves as soon as the router catches up.",
		Path: func(id string) string { return "/deployments/" + id + "/promote" },
	},
	"rollback": {
		Key: "rollback", Verb: "Roll back", Title: "Roll back to the previous revision",
		Explain: "Re-deploys the revision before the live one as a NEW revision. " +
			"Nothing is rewritten — rollback moves forward through the same rollout path.",
		Danger: true,
		Path:   func(id string) string { return "/envs/" + id + "/rollback" },
	},
	"drain": {
		Key: "drain", Verb: "Drain", Title: "Drain this node",
		Explain: "Cordons the node so nothing new is placed on it, and moves what it safely can. " +
			"Environments holding durable state cannot move and will be reported as stranded.",
		Danger: true,
		Path:   func(id string) string { return "/nodes/" + id + "/drain" },
	},
	"uncordon": {
		Key: "uncordon", Verb: "Uncordon", Title: "Return this node to service",
		Explain: "Lifts the drain. The node becomes ready or unreachable depending on whether " +
			"it has heartbeated recently — the control plane derives that rather than assuming it.",
		Path: func(id string) string { return "/nodes/" + id + "/uncordon" },
	},
	"rotate-recipient": {
		Key: "rotate-recipient", Verb: "Approve key", Title: "Approve this node's new age key",
		Explain: "Promotes the key this node has been advertising. Secrets set after this are " +
			"sealed to it. Only do this if you know why the node's key changed.",
		Danger: true,
		Path:   func(id string) string { return "/nodes/" + id + "/rotate-recipient" },
	},
}

// getConfirm renders the interstitial. One route for every action, because the
// page differs only in wording — and because a confirmation somebody can skip
// by knowing a URL is not a confirmation.
func (s *Server) getConfirm(w http.ResponseWriter, r *http.Request, _ *client.Client, sess session) {
	a, ok := actions[r.URL.Query().Get("action")]
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "confirm.html", map[string]any{
		"Email": sess.email, "CSRF": sess.csrf, "Flash": "",
		"A": map[string]any{
			"Title": a.Title, "Explain": a.Explain, "Verb": a.Verb,
			"Danger": a.Danger, "Path": a.Path(id),
		},
		"Subject": r.URL.Query().Get("subject"),
		"Back":    backTo(r.URL.Query().Get("back")),
	})
}

// backTo keeps the post-action redirect on this console. An open redirect is
// the classic way a "return to where you were" parameter becomes a phishing
// hop, and there is no case here for leaving the site.
func backTo(v string) string {
	if len(v) > 1 && v[0] == '/' && v[1] != '/' {
		return v
	}
	return "/"
}

// act wraps every mutating handler: CSRF first, then the call, then a flash
// describing what the API actually answered.
func (s *Server) act(fn func(context.Context, *client.Client, string, *http.Request) (string, error)) http.HandlerFunc {
	return s.guard(func(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
		if err := r.ParseForm(); err != nil {
			s.fail(w, r, sess, fmt.Errorf("could not read the form: %w", err))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.validCSRF(c.Value, r.PostFormValue("csrf")) {
			// Deliberately terse and deliberately not a redirect: a request
			// that failed this check did not come from a page this console
			// served, and there is nowhere sensible to send it back to.
			http.Error(w, "this request did not come from a form on this console", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		id := firstNonEmpty(r.PathValue("env"), r.PathValue("id"))
		msg, err := fn(ctx, cl, id, r)
		if err != nil {
			// The failure goes in the flash rather than an error page: the
			// operator is mid-task and needs to see it next to the thing they
			// were acting on, not on a dead end.
			s.sessions.setFlash(c.Value, "✗ "+err.Error())
		} else {
			s.sessions.setFlash(c.Value, "✓ "+msg)
		}
		http.Redirect(w, r, backTo(r.PostFormValue("back")), http.StatusSeeOther)
	})
}

func (s *Server) routeActions() {
	s.mux.HandleFunc("GET /confirm", s.guard(s.getConfirm))

	s.mux.HandleFunc("POST /envs/{env}/deploy", s.act(
		func(ctx context.Context, cl *client.Client, id string, r *http.Request) (string, error) {
			// Empty version means latest, which is what the API's own default
			// does — the console does not resolve it and then send something
			// that could differ from what the operator was shown.
			d, err := cl.Deploy(ctx, id, "", "console:"+r.PostFormValue("by"))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("revision %d created (%s)", d.Revision, d.State), nil
		}))

	s.mux.HandleFunc("POST /deployments/{id}/promote", s.act(
		func(ctx context.Context, cl *client.Client, id string, _ *http.Request) (string, error) {
			if _, err := cl.Promote(ctx, id); err != nil {
				return "", err
			}
			return "promoted; traffic moves as the router catches up", nil
		}))

	s.mux.HandleFunc("POST /envs/{env}/rollback", s.act(
		func(ctx context.Context, cl *client.Client, id string, r *http.Request) (string, error) {
			to, _ := strconv.Atoi(r.PostFormValue("to_revision"))
			d, err := cl.Rollback(ctx, id, to)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("rolled back as new revision %d", d.Revision), nil
		}))

	s.mux.HandleFunc("POST /nodes/{id}/drain", s.act(
		func(ctx context.Context, cl *client.Client, id string, _ *http.Request) (string, error) {
			res, err := cl.DrainNode(ctx, id)
			if err != nil {
				return "", err
			}
			// Both halves, always. Drain's contract is that it evacuates what
			// it can and reports what it cannot, and an operator who is told
			// only the good half will believe the node is empty.
			msg := fmt.Sprintf("cordoned; %d environment(s) released", len(res.Released))
			if len(res.Stranded) > 0 {
				msg += fmt.Sprintf(", %d stranded and still running here", len(res.Stranded))
			}
			return msg, nil
		}))

	s.mux.HandleFunc("POST /nodes/{id}/uncordon", s.act(
		func(ctx context.Context, cl *client.Client, id string, _ *http.Request) (string, error) {
			state, err := cl.UncordonNode(ctx, id)
			if err != nil {
				return "", err
			}
			// The state the control plane derived, not a hopeful "ready".
			return "uncordoned; the node is now " + state, nil
		}))

	s.mux.HandleFunc("POST /nodes/{id}/rotate-recipient", s.act(
		func(ctx context.Context, cl *client.Client, id string, _ *http.Request) (string, error) {
			n, err := cl.RotateNodeRecipient(ctx, id)
			if err != nil {
				return "", err
			}
			return "approved; secrets are now sealed to " + n.Hostname + "'s new key", nil
		}))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
