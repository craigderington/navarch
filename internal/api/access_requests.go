package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/mail"
	"github.com/craigderington/navarch/internal/store"
)

// The request-access door.
//
// This is the third unauthenticated route in the system, after the liveness
// probe and invite redemption, and it is the only one that writes a row for a
// caller who holds no credential at all. It is defensible for one reason: it
// creates no identity. Self-serve signup — which the Sprint 9 plan describes,
// and which this deliberately is not — needs email verification before it is
// safe, because an unverified operator row lets somebody squat an address and
// then be collected by an invitation meant for its real owner. A request row
// grants nothing, so there is nothing to squat.
//
// What it does need, and does not have, is rate limiting. There is none
// anywhere in the system (the 2026-08-19 audit recorded it as S10), and this
// route removes the two things that made that acceptable: authentication and a
// caller who can be identified. The mitigations here are specific rather than
// general — one pending row per address, so submissions do not accumulate, and
// a notification only when a request is actually new, so mail cannot be
// amplified by resubmission. Neither bounds an attacker with a supply of
// distinct addresses. That is the standing gap, and it is the reason this door
// is off unless an operator opens it.

type accessRequestBody struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Note  string `json:"note,omitempty"`
}

// handleCreateAccessRequest files a request against the configured org.
//
// It answers 202 for every outcome a caller could learn something from. Whether
// the address already asked, already has an account, or was declined last week
// are all facts about somebody else's organization, and an endpoint that
// distinguished them would be an account-existence oracle that anybody on the
// internet could query. The one thing it does distinguish is its own
// misconfiguration: a slug naming no organization is a 503 and a log line, not
// a 404, because a 404 there is indistinguishable from "the door is closed" and
// would hide a broken install behind an expected answer.
func (s *Server) handleCreateAccessRequest(w http.ResponseWriter, r *http.Request) {
	if s.signupOrg == "" {
		// Not configured: the door does not exist. 404 rather than 403, for the
		// same reason every other refusal in this API is a 404 — a distinct
		// status would map the surface.
		s.notFound(w)
		return
	}
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	org, err := s.st.GetOrganizationBySlug(ctx, s.signupOrg)
	if err != nil {
		s.log.Error("access requests are enabled but the configured organization does not exist",
			"org", s.signupOrg, "error", err)
		writeError(w, http.StatusServiceUnavailable, "access requests are not available", nil)
		return
	}

	var body accessRequestBody
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	ar, fresh, err := s.st.CreateAccessRequest(ctx, store.CreateAccessRequestParams{
		OrgID: org.ID, Email: body.Email, Name: body.Name, Note: body.Note,
	})
	if err != nil {
		// A malformed address is the caller's own mistake and telling them is
		// no leak — it is about what they typed, not about who exists.
		s.writeStoreError(w, err)
		return
	}

	// Only a genuinely new request notifies. A public form gets submitted twice
	// by anyone unsure it worked, and a mail per submission is how an operator
	// learns to filter the sender.
	if fresh && s.mailer != nil {
		to, err := s.st.OrgOperatorEmails(ctx, org.ID)
		if err != nil {
			s.log.Warn("access request filed but recipients could not be resolved",
				"request", ar.ID, "error", err)
		} else if len(to) > 0 {
			if err := s.mailer.Send(ctx, accessRequestMessage(ar, org, to, s.consoleURL)); err != nil {
				// A log line, like a failed rollout's notification: the request
				// is already durable, and the list is the better copy of what
				// the mail says. Failing here would refuse a request that was
				// in fact filed.
				s.log.Warn("access request filed but not emailed", "request", ar.ID, "error", err)
			}
		}
	}

	// No body worth reading. The caller learns that it was accepted, and
	// nothing about what already existed.
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "received"})
}

func (s *Server) handleListAccessRequests(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	reqs, err := s.st.ListAccessRequests(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_requests": reqs})
}

type approveAccessRequestBody struct {
	Role     string `json:"role,omitempty"`
	TTLHours int    `json:"ttl_hours,omitempty"`
}

// handleApproveAccessRequest turns a request into an invitation.
//
// The response is the invitation and its link, exactly as creating one by hand
// returns them — including whether the mail actually went, because an operator
// who believes it arrived will not paste the link and the invitation will
// simply expire unused.
func (s *Server) handleApproveAccessRequest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	requestID, ok := pathUUID(w, r, "request")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	var body approveAccessRequestBody
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", nil)
			return
		}
	}

	params := store.ApproveAccessRequestParams{
		OrgID: orgID, RequestID: requestID, Role: body.Role,
		TTL: time.Duration(body.TTLHours) * time.Hour,
	}
	var approver *store.Operator
	if id, ok := identityFrom(r.Context()); ok && id.isOperator() {
		approver = id.operator
		params.DecidedBy = &approver.ID
	}

	ar, inv, err := s.st.ApproveAccessRequest(ctx, params)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	resp := map[string]any{
		"access_request": ar,
		"invite":         inv,
		"url":            s.inviteURL(inv.Plaintext),
		"emailed":        false,
	}
	if s.mailer != nil {
		org, _ := s.st.GetOrganization(ctx, orgID)
		if err := s.mailer.Send(ctx, inviteMessage(inv, org, approver, resp["url"].(string))); err != nil {
			s.log.Warn("access request approved but the invitation was not emailed",
				"request", ar.ID, "invite", inv.ID, "error", err)
			resp["email_error"] = err.Error()
		} else {
			resp["emailed"] = true
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleDeclineAccessRequest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	requestID, ok := pathUUID(w, r, "request")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	var decidedBy *uuid.UUID
	if id, ok := identityFrom(r.Context()); ok && id.isOperator() {
		decidedBy = &id.operator.ID
	}

	ar, err := s.st.DeclineAccessRequest(ctx, orgID, requestID, decidedBy)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_request": ar})
}

// accessRequestMessage tells an organization's operators that somebody is
// waiting.
//
// Plain text, and the note is included because it is the only part of a request
// that helps decide — but it is text a stranger typed, so it goes through the
// same truncation and line-break stripping every other tenant-sourced string in
// a message does. The link is to the console, built from the configured base
// rather than from any request, for the reason the invitation link is.
func accessRequestMessage(ar *store.AccessRequest, org *store.Organization, to []string, consoleURL string) mail.Message {
	orgName := "Navarch"
	if org != nil && org.Name != "" {
		orgName = org.Name
	}
	who := ar.Email
	if ar.Name != "" {
		who = ar.Name + " <" + ar.Email + ">"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s asked for access to %s.\n\n", who, orgName)
	if ar.Note != "" {
		b.WriteString("They said:\n")
		fmt.Fprintf(&b, "  %s\n\n", ar.Note)
	}
	if consoleURL != "" {
		fmt.Fprintf(&b, "Review it: %s/access-requests\n\n", consoleURL)
	}
	b.WriteString("Nothing has been granted. Approving sends them an invitation,\n")
	b.WriteString("which is what actually creates their account.\n")

	return mail.Message{
		To:      to,
		Subject: "Access request: " + ar.Email,
		Body:    b.String(),
	}
}
