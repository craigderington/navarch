package api

import (
	"net/http"
	"regexp"
	"time"

	"github.com/craigderington/navarch/internal/secrets"
)

// secretKeyPattern is the anchored form of the ${secret:KEY} key charset. It
// validates the whole key — unlike spec.SecretRefPattern, which is deliberately
// unanchored to find markers embedded in larger strings.
var secretKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Secret handlers. The control plane never sees plaintext at rest: a value
// arrives here once, is encrypted to the nodes that should open it, and
// only the ciphertext is stored. See internal/secrets for the encrypt
// boundary.

type setSecretRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	if !s.authorizeEnv(w, r, envID) {
		return
	}
	var req setSecretRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	// A secret key must match the valid key pattern: alphanumerics, dots,
	// underscores, and hyphens only.
	if !secretKeyPattern.MatchString(req.Key) {
		writeError(w, http.StatusBadRequest, "invalid secret key", nil)
		return
	}
	recipients, err := s.st.RecipientsForEnvironment(ctx, envID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if len(recipients) == 0 {
		writeError(w, http.StatusUnprocessableEntity,
			"no ready node with an encryption key; is an agent running?", nil)
		return
	}
	ct, err := secrets.Encrypt(req.Value, recipients)
	if err != nil {
		s.log.Error("secret encryption failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to encrypt secret", nil)
		return
	}
	// key_id records what it was sealed to, for audit/rotation. The recipients
	// double as the id in Sprint 2's single-node world.
	if err := s.st.SetSecret(ctx, envID, req.Key, ct, recipients[0]); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key": req.Key})
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	if !s.authorizeEnv(w, r, envID) {
		return
	}
	metas, err := s.st.SecretKeysForEnv(ctx, envID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": metas})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	if !s.authorizeEnv(w, r, envID) {
		return
	}
	if err := s.st.DeleteSecret(ctx, envID, r.PathValue("key")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
