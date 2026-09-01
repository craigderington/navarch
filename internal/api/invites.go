package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/craigderington/navarch/internal/mail"
	"github.com/craigderington/navarch/internal/store"
)

type createInviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role,omitempty"`
	// TTLHours is optional; the store defaults and caps it.
	TTLHours int `json:"ttl_hours,omitempty"`
}

type createInviteResponse struct {
	Invite store.OperatorInvite `json:"invite"`
	// URL is the link the invited person opens. It is returned to the inviter
	// as well as emailed, deliberately: the inviter is already authorized to
	// grant this access, so showing them the link is no escalation — and it
	// means an install with no mail configured, or one whose provider is having
	// a bad day, can still onboard somebody by pasting it.
	URL string `json:"url"`
	// Emailed says whether the message actually went. Reporting it is the whole
	// difference between an operator who knows to paste the link and one who
	// believes it arrived.
	Emailed bool   `json:"emailed"`
	Error   string `json:"email_error,omitempty"`
}

// handleCreateInvite invites somebody into an organization.
//
// The token is never the operator's credential — it is exchanged for one. A
// bearer token mailed directly would sit in an inbox, and in every archive and
// forward of it, for as long as it was valid; this one is single-use, expires,
// and is worth nothing after it has been redeemed once.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	var req createInviteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	var invitedBy *store.Operator
	if id, ok := identityFrom(r.Context()); ok && id.isOperator() {
		invitedBy = id.operator
	}
	params := store.CreateInviteParams{
		OrgID: orgID, Email: req.Email, Role: req.Role,
		TTL: time.Duration(req.TTLHours) * time.Hour,
	}
	if invitedBy != nil {
		params.InvitedBy = &invitedBy.ID
	}
	inv, err := s.st.CreateInvite(ctx, params)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	resp := createInviteResponse{Invite: *inv, URL: s.inviteURL(inv.Plaintext)}
	if s.mailer != nil {
		org, _ := s.st.GetOrganization(ctx, orgID)
		if err := s.mailer.Send(ctx, inviteMessage(inv, org, invitedBy, resp.URL)); err != nil {
			// Not fatal. The invitation exists and the caller has the link, so
			// failing the request would destroy a credential that was already
			// minted and leave the org with a dead row it must revoke.
			s.log.Warn("invite created but not emailed", "invite", inv.ID, "error", err)
			resp.Error = err.Error()
		} else {
			resp.Emailed = true
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	invites, err := s.st.ListInvites(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites})
}

func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	inviteID, ok := pathUUID(w, r, "invite")
	if !ok {
		return
	}
	if err := s.st.RevokeInvite(ctx, orgID, inviteID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type redeemInviteRequest struct {
	Token string `json:"token"`
	// TokenName labels the operator token this mints, so an operator can later
	// tell "the one the console made when I accepted" from one they created.
	TokenName string `json:"token_name,omitempty"`
}

type redeemInviteResponse struct {
	Operator *store.Operator `json:"operator"`
	Token    string          `json:"token"`
}

// handleRedeemInvite exchanges an invitation for an operator token.
//
// Unauthenticated by necessity: the person redeeming has no identity yet, which
// is the entire point. The invite token IS the credential, and the store's
// claim is a single UPDATE ... RETURNING so two simultaneous redemptions cannot
// both succeed.
func (s *Server) handleRedeemInvite(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	var req redeemInviteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	op, tok, err := s.st.RedeemInvite(ctx, strings.TrimSpace(req.Token), req.TokenName)
	if err != nil {
		// Unknown, expired, revoked and already-used arrive here identically.
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, redeemInviteResponse{Operator: op, Token: tok.Plaintext})
}

// inviteURL builds the link from configured state, never from the request.
//
// A link assembled out of the Host header is a link an attacker can aim: invite
// somebody, set Host to a site you control, and the victim types their
// invitation into it. The console URL is configuration for that reason.
func (s *Server) inviteURL(token string) string {
	base := strings.TrimSuffix(s.consoleURL, "/")
	if base == "" {
		// No console configured. Say so rather than emit a broken link — an
		// operator can still redeem with `navarch invite accept`.
		return "navarch invite accept " + token
	}
	return base + "/invite?token=" + token
}

func inviteMessage(inv *store.OperatorInvite, org *store.Organization, by *store.Operator, url string) mail.Message {
	orgName := "an organization"
	if org != nil {
		orgName = org.Name
		if orgName == "" {
			orgName = org.Slug
		}
	}
	var b strings.Builder
	if by != nil {
		fmt.Fprintf(&b, "%s has invited you to %s on Navarch.\n\n", by.Email, orgName)
	} else {
		fmt.Fprintf(&b, "You have been invited to %s on Navarch.\n\n", orgName)
	}
	fmt.Fprintf(&b, "  %s\n\n", url)
	fmt.Fprintf(&b, "The link works once and expires %s.\n", inv.ExpiresAt.UTC().Format(time.RFC1123))
	b.WriteString("Opening it signs you in and creates your own credentials; the link\n")
	b.WriteString("itself is worth nothing afterwards.\n\n")
	b.WriteString("If you were not expecting this, ignore it — nothing happens until\n")
	b.WriteString("the link is opened, and it stops working on the date above.\n")

	return mail.Message{
		To:      []string{inv.Email},
		Subject: fmt.Sprintf("You have been invited to %s on Navarch", orgName),
		Body:    b.String(),
	}
}

// Mailer is the part of internal/mail the API needs. Declared here rather than
// imported as a concrete type for the reason RouterSync is: the server stays
// testable without a provider, and nil is a supported configuration.
type Mailer interface {
	Send(ctx context.Context, m mail.Message) error
}
