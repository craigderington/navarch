package api

import "net/http"

// Agent-facing endpoints and rollback are Sprint 2 work: they only become
// meaningful once the node agent exists to register, heartbeat and report.
// The store methods behind them are not written yet either.

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request)       { notImplemented(w) }
func (s *Server) handleRegisterNode(w http.ResponseWriter, r *http.Request)   { notImplemented(w) }
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request)      { notImplemented(w) }
func (s *Server) handleDesiredState(w http.ResponseWriter, r *http.Request)   { notImplemented(w) }
func (s *Server) handleInstanceReport(w http.ResponseWriter, r *http.Request) { notImplemented(w) }
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request)      { notImplemented(w) }

func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not implemented until sprint 2", nil)
}
