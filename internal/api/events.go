package api

import (
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	beforeID, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	events, err := s.st.ListEvents(ctx, orgID, beforeID, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
