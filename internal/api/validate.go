package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/craig/composectl/internal/parser"
)

// contextWithTimeout derives a bounded context from the request.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

type validateResponse struct {
	Valid   bool   `json:"valid"`
	Digest  string `json:"digest,omitempty"`
	Summary struct {
		Services  []string `json:"services"`
		Swappable []string `json:"swappable"`
		Pinned    []string `json:"pinned"`
		Ingress   string   `json:"ingress,omitempty"`
		// PeakMemoryBytes is what a blue/green rollout will require:
		// swappable services counted twice, pinned services once.
		PeakMemoryBytes int64 `json:"peak_memory_bytes"`
	} `json:"summary"`
}

// handleValidate parses a compose file and reports what would be deployed,
// without persisting anything. This is the fast feedback loop for the CLI
// and the endpoint that makes the platform's constraints discoverable.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body", nil)
		return
	}

	dspec, err := parser.Parse(ctx, raw, "validate")
	if err != nil {
		var verrs parser.ValidationErrors
		if errors.As(err, &verrs) {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{
				Error:   "compose file violates platform constraints",
				Details: verrs,
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	digest, err := dspec.Digest()
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	var resp validateResponse
	resp.Valid = true
	resp.Digest = digest
	resp.Summary.Services = dspec.ServiceNames()
	resp.Summary.Swappable = dspec.SwappableServices()
	resp.Summary.Pinned = dspec.PinnedServices()
	resp.Summary.PeakMemoryBytes = dspec.PeakMemoryBytes()
	if ing, ok := dspec.IngressService(); ok {
		resp.Summary.Ingress = ing
	}

	writeJSON(w, http.StatusOK, resp)
}
