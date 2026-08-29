package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/craigderington/navarch/internal/store"
)

type whoamiResponse struct {
	Operator *store.Operator      `json:"operator"`
	Orgs     []store.Organization `json:"organizations"`
}

// handleWhoami answers "who am I and what can I see". It addresses no object
// — the answer is the caller — so it is the one operator route with nothing to
// authorize beyond being authenticated.
//
// Small, and the first thing anyone runs when a 404 is ambiguous. Since a
// non-member and a missing object are deliberately indistinguishable, an
// operator who cannot see something needs *some* way to tell "I am in the
// wrong org" from "that id is wrong", and this is it.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	id, ok := identityFrom(r.Context())
	if !ok || !id.isOperator() {
		// Authentication disabled (in-process tests), or a machine credential.
		// Neither is a person, and inventing an answer would be worse than
		// saying so.
		writeError(w, http.StatusNotFound, "not an operator identity", nil)
		return
	}
	orgs, err := s.st.OrgsForOperator(ctx, id.operator.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, whoamiResponse{Operator: id.operator, Orgs: orgs})
}

type addMemberRequest struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
}

type addMemberResponse struct {
	Member store.OrgMember `json:"member"`
	// Token is set exactly once, when this call created the operator. It is the
	// same bargain node registration makes: a new identity needs a first
	// credential, there is nowhere to store the plaintext, and the alternative
	// is an operator who exists but can never authenticate. An existing
	// operator gets nothing here — adding someone to a second org must not mint
	// them a new credential.
	Token string `json:"token,omitempty"`
}

// handleAddMember adds an operator to an organization, creating them if the
// email is new. Creating on add rather than behind a separate endpoint keeps
// the common case — "let a colleague into this org" — one request, and there is
// no useful state for an operator who belongs to nothing.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	var req addMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	var token string
	op, err := s.st.GetOperatorByEmail(ctx, req.Email)
	if errors.Is(err, store.ErrNotFound) {
		op, err = s.st.CreateOperator(ctx, req.Email, req.Name)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		t, tErr := s.st.IssueOperatorToken(ctx, op.ID, "initial", nil)
		if tErr != nil {
			s.writeStoreError(w, tErr)
			return
		}
		token = t.Plaintext
	} else if err != nil {
		s.writeStoreError(w, err)
		return
	}

	if err := s.st.AddOrgMember(ctx, orgID, op.ID, req.Role); err != nil {
		s.writeStoreError(w, err)
		return
	}
	members, err := s.st.ListOrgMembers(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, m := range members {
		if m.OperatorID == op.ID {
			writeJSON(w, http.StatusCreated, addMemberResponse{Member: m, Token: token})
			return
		}
	}
	writeError(w, http.StatusInternalServerError, "member was added but could not be read back", nil)
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	members, err := s.st.ListOrgMembers(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// handleRemoveMember drops a membership. It refuses to remove the last member,
// because an org nobody belongs to is unreachable by every route in this file
// and could only be recovered with SQL — the same class of one-way door that
// drain was before uncordon existed.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	operatorID, ok := pathUUID(w, r, "operator")
	if !ok {
		return
	}
	members, err := s.st.ListOrgMembers(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if len(members) <= 1 {
		writeError(w, http.StatusConflict,
			"an organization must keep at least one member; add another before removing this one", nil)
		return
	}
	if err := s.st.RemoveOrgMember(ctx, orgID, operatorID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------- the caller's tokens

type createTokenRequest struct {
	Name string `json:"name,omitempty"`
	// ExpiresInDays bounds a token's life. Zero means no expiry, which is the
	// right default for the credential a person types into `navarch login` and
	// wrong for one pasted into CI — so it is offered rather than imposed.
	ExpiresInDays int `json:"expires_in_days,omitempty"`
}

// handleCreateToken mints a token for the calling operator.
//
// Self-scoped: an operator manages their own credentials and nobody else's,
// which is why these routes address `me` rather than an id. There is
// deliberately no route to mint a token *for* another operator — that would be
// a way to acquire someone else's identity, and the audit log would faithfully
// record the wrong person.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	id, ok := identityFrom(r.Context())
	if !ok || !id.isOperator() {
		writeError(w, http.StatusNotFound, "not an operator identity", nil)
		return
	}
	var req createTokenRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", nil)
			return
		}
	}
	var expires *time.Time
	if req.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		expires = &t
	}
	tok, err := s.st.IssueOperatorToken(ctx, id.operator.ID, req.Name, expires)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// The one response that carries the plaintext. Nothing stores it and no
	// later read can produce it.
	writeJSON(w, http.StatusCreated, tok)
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	id, ok := identityFrom(r.Context())
	if !ok || !id.isOperator() {
		writeError(w, http.StatusNotFound, "not an operator identity", nil)
		return
	}
	tokens, err := s.st.ListOperatorTokens(ctx, id.operator.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleRevokeToken deletes one of the caller's tokens, and refuses to delete
// the last one.
//
// Nothing else can issue a token to an existing operator — `member add` mints
// one only when it creates the operator — so revoking your last credential is
// unrecoverable without database access. That is the same one-way door drain
// was before uncordon existed, and the fix is the same: refuse, and say what to
// do instead. Rotating a compromised token is still possible, in the order that
// was always correct anyway: create the new one, then revoke the old.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	id, ok := identityFrom(r.Context())
	if !ok || !id.isOperator() {
		writeError(w, http.StatusNotFound, "not an operator identity", nil)
		return
	}
	tokenID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	tokens, err := s.st.ListOperatorTokens(ctx, id.operator.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if len(tokens) <= 1 {
		writeError(w, http.StatusConflict,
			"this is your only token; create another before revoking it or you will lock yourself out", nil)
		return
	}
	if err := s.st.RevokeOperatorToken(ctx, id.operator.ID, tokenID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
