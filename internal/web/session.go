// Package web is the browser console: a second consumer of internal/client,
// never a second implementation of the protocol.
//
// It renders HTML on the server and holds the operator's token itself. A
// single-page app talking to /v1 directly would have to keep a bearer token
// somewhere JavaScript can read it, and the control plane's credential model —
// bearer tokens, hashed at rest, constant-time compared — is deliberately
// machine-facing. Giving the browser a session cookie instead means the thing
// it holds is useless anywhere but here.
package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "navarch_session"
	sessionTTL    = 12 * time.Hour
)

type session struct {
	token   string
	email   string
	expires time.Time
}

// sessions is in memory on purpose. A console restart logging everybody out is
// correct and cheap; persisting operator tokens to disk to avoid it trades a
// real secret for a small convenience.
type sessions struct {
	mu sync.Mutex
	m  map[string]session
}

func newSessions() *sessions { return &sessions{m: map[string]session{}} }

func (s *sessions) create(token, email string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	// Opportunistic sweep: a console with a handful of operators never needs a
	// reaper goroutine, and one more map walk per login is cheaper than one
	// more thing that can leak.
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.expires) {
			delete(s.m, k)
		}
	}
	s.m[id] = session{token: token, email: email, expires: now.Add(sessionTTL)}
	return id, nil
}

func (s *sessions) get(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok || time.Now().After(v.expires) {
		delete(s.m, id)
		return session{}, false
	}
	return v, true
}

func (s *sessions) destroy(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// setCookie issues the session cookie. Secure is set only when the console was
// reached over TLS: forcing it on a plain-HTTP loopback console would make the
// cookie silently not stick, which looks exactly like a broken login.
func setCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		Expires:  time.Now().Add(sessionTTL),
	})
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}
