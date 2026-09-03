package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/craigderington/navarch/internal/client"
	"github.com/craigderington/navarch/internal/version"
)

type Server struct {
	api      string
	log      *slog.Logger
	mux      *http.ServeMux
	sessions *sessions
	pages    map[string]*template.Template
}

func New(apiURL string, log *slog.Logger) (*Server, error) {
	p, err := parsePages()
	if err != nil {
		return nil, err
	}
	s := &Server{api: strings.TrimRight(apiURL, "/"), log: log, mux: http.NewServeMux(),
		sessions: newSessions(), pages: p}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok "+version.Version)
	})
	s.mux.HandleFunc("GET /login", s.getLogin)
	s.mux.HandleFunc("POST /login", s.postLogin)
	s.mux.HandleFunc("POST /logout", s.postLogout)
	s.mux.HandleFunc("GET /invite", s.getInvite)
	s.mux.HandleFunc("POST /invite", s.postInvite)
	s.mux.HandleFunc("GET /request-access", s.getRequestAccess)
	s.mux.HandleFunc("POST /request-access", s.postRequestAccess)

	s.mux.HandleFunc("GET /{$}", s.guard(s.fleet))
	s.mux.HandleFunc("GET /orgs/{org}", s.guard(s.environments))
	s.mux.HandleFunc("GET /orgs/{org}/events", s.guard(s.events))
	s.mux.HandleFunc("GET /orgs/{org}/access-requests", s.guard(s.accessRequests))
	s.mux.HandleFunc("GET /envs/{env}", s.guard(s.environment))
	s.mux.HandleFunc("GET /deployments/{id}", s.guard(s.deployment))
	s.routeActions()
}

// guard resolves the session and builds the client for it. Every page below
// receives a client already carrying the operator's own token, so a page cannot
// accidentally act as anybody else — and cannot reach the API without one.
func (s *Server) guard(h func(http.ResponseWriter, *http.Request, *client.Client, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		sess, ok := s.sessions.get(c.Value)
		if !ok {
			clearCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		cl, err := client.New(s.api, sess.token)
		if err != nil {
			s.fail(w, r, sess, err)
			return
		}
		h(w, r, cl, sess)
	}
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"API": s.api, "Error": r.URL.Query().Get("error")})
}

// postLogin verifies before it stores, exactly as `navarch login` does. A
// session holding a token the control plane rejects is worse than no session:
// every page then fails and nothing points at the step that said it worked.
func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=bad+form", http.StatusSeeOther)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		http.Redirect(w, r, "/login?error=a+token+is+required", http.StatusSeeOther)
		return
	}
	cl, err := client.New(s.api, token)
	if err != nil {
		http.Redirect(w, r, "/login?error=the+console+is+misconfigured", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	me, err := cl.Whoami(ctx)
	if err != nil {
		s.log.Warn("login rejected", "err", err)
		http.Redirect(w, r, "/login?error=that+token+was+not+accepted", http.StatusSeeOther)
		return
	}
	email := "operator"
	if me.Operator != nil {
		email = me.Operator.Email
	}
	id, err := s.sessions.create(token, email)
	if err != nil {
		s.fail(w, r, session{}, err)
		return
	}
	setCookie(w, r, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// getInvite renders the acceptance page. It does not redeem: a GET must not
// spend a single-use credential, because link previewers, mail scanners and
// browser prefetch all issue GETs nobody asked for — and the person would find
// their invitation already used before they ever saw it.
func (s *Server) getInvite(w http.ResponseWriter, r *http.Request) {
	s.render(w, "invite.html", map[string]any{
		"Token": r.URL.Query().Get("token"),
		"Error": r.URL.Query().Get("error"),
	})
}

// postInvite redeems the invitation and signs the new operator in.
//
// No CSRF token, and it is the one form here without one: there is no session
// yet to hold a token against. What stands in its place is the invitation
// itself — a cross-site POST would have to already know it, and anyone who
// knows it can simply redeem it directly. SameSite=Lax on the session cookie
// this sets still applies from the next request onward.
func (s *Server) postInvite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/invite?error=bad+form", http.StatusSeeOther)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" {
		http.Redirect(w, r, "/invite?error=that+link+carried+no+invitation", http.StatusSeeOther)
		return
	}
	// A client with no bearer token of its own: the person accepting has none,
	// which is the entire point of an invitation.
	cl, err := client.New(s.api, "")
	if err != nil {
		s.fail(w, r, session{}, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := cl.RedeemInvite(ctx, token, "console")
	if err != nil {
		s.log.Warn("invite rejected", "err", err)
		// Expired, revoked, unknown and already-used are deliberately
		// indistinguishable at the API, so the page says all of them rather
		// than picking one and being wrong.
		http.Redirect(w, r,
			"/invite?error=that+invitation+is+no+longer+valid+—+it+may+be+expired%2C+already+used%2C+or+revoked",
			http.StatusSeeOther)
		return
	}
	email := "operator"
	if res.Operator != nil {
		email = res.Operator.Email
	}
	id, err := s.sessions.create(res.Token, email)
	if err != nil {
		s.fail(w, r, session{}, err)
		return
	}
	setCookie(w, r, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.destroy(c.Value)
	}
	clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ------------------------------------------------------------------- pages

func (s *Server) fleet(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	me, err := cl.Whoami(ctx)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	type orgFleet struct {
		Org   client.Organization
		Nodes []client.Node
		Err   string
	}
	fleets := make([]orgFleet, 0, len(me.Orgs))
	for _, o := range me.Orgs {
		f := orgFleet{Org: o}
		nodes, err := cl.ListNodes(ctx, o.ID)
		if err != nil {
			// One org failing must not blank the page: an operator with access
			// to several needs to see the ones that answered.
			f.Err = err.Error()
		}
		f.Nodes = nodes
		fleets = append(fleets, f)
	}
	s.render(w, "fleet.html", map[string]any{
		"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r), "Me": me, "Fleets": fleets, "Now": time.Now(),
	})
}

func (s *Server) environments(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	org := r.PathValue("org")
	envs, err := cl.ListOrgEnvironments(ctx, org)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	s.render(w, "environments.html", map[string]any{
		"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r), "Org": org, "Envs": envs,
	})
}

func (s *Server) environment(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	id := r.PathValue("env")
	env, err := cl.GetEnv(ctx, id)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	deps, err := cl.ListDeployments(ctx, id)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	s.render(w, "environment.html", map[string]any{
		"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r), "Env": env, "Deployments": deps,
	})
}

func (s *Server) deployment(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := cl.GetDeployment(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	s.render(w, "deployment.html", map[string]any{"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r), "D": d})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	evs, err := cl.ListEvents(ctx, r.PathValue("org"), 100, 0)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	s.render(w, "events.html", map[string]any{
		"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r), "Org": r.PathValue("org"), "Events": evs,
	})
}

// ------------------------------------------------------------------ output

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The console renders operator-supplied strings — hostnames, failure
	// reasons, event messages. html/template escapes them by construction,
	// which is most of why this is server-rendered rather than assembled in
	// JavaScript from JSON.
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("no such template", "template", name)
		return
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render failed", "template", name, "err", err)
	}
}

// fail shows the error rather than a blank page. An operator looking at a
// console during an incident needs to know whether the platform is unreachable
// or simply has nothing to show, and those look identical when a page renders
// empty.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, sess session, err error) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		if c, cErr := r.Cookie(sessionCookie); cErr == nil {
			s.sessions.destroy(c.Value)
		}
		clearCookie(w)
		http.Redirect(w, r, "/login?error=your+session+is+no+longer+valid", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusBadGateway)
	s.render(w, "error.html", map[string]any{"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r), "Err": err.Error()})
}

// flash reads and clears any pending one-shot message for this session.
func (s *Server) flash(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return s.sessions.takeFlash(c.Value)
}
