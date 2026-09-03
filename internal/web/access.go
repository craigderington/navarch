package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/craigderington/navarch/internal/client"
)

// The request-access door, from the console's side.
//
// Two halves that do not resemble each other. The public form is served to
// anybody, holds no session and carries no CSRF token; the review page is an
// ordinary guarded page whose actions go through the same confirmation
// interstitial as draining a node. That asymmetry is the design: asking is not
// a privileged act, and deciding is.

// getRequestAccess renders the public form.
//
// GET renders and only POST files, the same split /invite makes. Link
// previewers, mail scanners and browser prefetch all issue GETs nobody asked
// for, and while a stray access request is far less costly than a spent
// invitation, a form that acted on GET would file one every time somebody
// pasted the URL into a chat window.
func (s *Server) getRequestAccess(w http.ResponseWriter, r *http.Request) {
	s.render(w, "request-access.html", map[string]any{
		"Error": r.URL.Query().Get("error"),
		"Done":  r.URL.Query().Get("done") != "",
		"Email": r.URL.Query().Get("email"),
		"Name":  "",
		"Note":  "",
	})
}

// postRequestAccess files the request through the API's public route.
//
// No CSRF token, and it is the second form here without one — but for a
// different reason than /invite. There is no session to hold a token against,
// and there is also nothing worth protecting: a cross-site POST here achieves
// exactly what the attacker could achieve by submitting the form themselves.
//
// The client is built with no token, like /invite's. The console holds each
// session's own operator credential and has none of its own, which is what
// keeps this from being a confused deputy: an anonymous visitor cannot borrow
// the console's authority, because the console has none to lend.
func (s *Server) postRequestAccess(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/request-access?error=bad+form", http.StatusSeeOther)
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	if email == "" {
		http.Redirect(w, r, "/request-access?error=an+email+address+is+required", http.StatusSeeOther)
		return
	}
	cl, err := client.New(s.api, "")
	if err != nil {
		s.fail(w, r, session{}, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := cl.RequestAccess(ctx, client.RequestAccessInput{
		Email: email,
		Name:  strings.TrimSpace(r.PostFormValue("name")),
		Note:  strings.TrimSpace(r.PostFormValue("note")),
	}); err != nil {
		s.log.Warn("access request refused", "err", err)
		// The API distinguishes a malformed address (400) from a door that is
		// shut (404) from a misconfigured one (503), and none of those is a
		// fact about who exists — so all three can be reported without leaking
		// anything. They are collapsed into one sentence anyway, because a
		// visitor can act on only one of them.
		http.Redirect(w, r,
			"/request-access?error=that+could+not+be+sent+—+check+the+address%2C+or+try+again+later",
			http.StatusSeeOther)
		return
	}
	// Redirect rather than render, so a refresh does not re-file it.
	http.Redirect(w, r, "/request-access?done=1", http.StatusSeeOther)
}

// accessRequests is the operator's side: who has asked, and what to do about it.
func (s *Server) accessRequests(w http.ResponseWriter, r *http.Request, cl *client.Client, sess session) {
	org := r.PathValue("org")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	reqs, err := cl.ListAccessRequests(ctx, org)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	s.render(w, "access-requests.html", map[string]any{
		"Email": sess.email, "CSRF": sess.csrf, "Flash": s.flash(r),
		"Org": org, "Requests": reqs,
	})
}
