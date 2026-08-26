package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

// Log handlers move container output from a node to the operator who asked for
// it, without ever storing it. The request is a row; the answer is a bounded
// in-memory buffer that is dropped once read. Container stdout routinely carries
// secrets, and the platform's whole secret design is that plaintext does not
// reach the control plane's database — a log feature is not a good enough reason
// to make an exception, and nobody asked for logs to be durable.

type createLogRequest struct {
	Service string `json:"service"`
	Tail    int    `json:"tail,omitempty"`
	Follow  bool   `json:"follow,omitempty"`
}

// handleCreateLogRequest opens a request against one service of an environment.
//
// The environment and service are resolved to a concrete container by the store,
// so the agent is handed an id for a container it already runs. A caller cannot
// name a container directly, which is what stops this endpoint being a way to
// read any container on any node.
func (s *Server) handleCreateLogRequest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	var req createLogRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	lr, err := s.st.CreateLogRequest(ctx, store.CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: req.Service,
		TailLines: req.Tail, Follow: req.Follow,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, lr)
}

type logDeliveryRequest struct {
	Deliveries []struct {
		RequestID uuid.UUID `json:"request_id"`
		Data      string    `json:"data,omitempty"`
		Error     string    `json:"error,omitempty"`
	} `json:"deliveries"`
}

// handleLogDelivery accepts what an agent read. The content goes to memory and
// the row only records that the read happened, so a delivery is two very
// different writes to two very different lifetimes.
func (s *Server) handleLogDelivery(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	nodeID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req logDeliveryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	for _, d := range req.Deliveries {
		// Ownership gates the content, not just the row. Without this check
		// a node could forge output into an operator's tail under any request
		// id it knows — "credentials rotated, re-auth required" in a log tail
		// is acted on, not read — or allocate buffer entries for ids nobody
		// opened. The write happens before the completion because completing
		// a one-shot request flips it to done, and a reader polling in that
		// window would be told "finished" and shown nothing.
		owned, err := s.st.LogRequestOwnedByNode(ctx, nodeID, d.RequestID)
		if err != nil || !owned {
			// A vanished request is routine rather than a failure, exactly as
			// for instance reports: the requester may have closed the tail
			// between the agent's poll and its delivery. Aborting the batch
			// here would drop every other delivery in it.
			continue
		}
		if d.Data != "" && s.logs != nil {
			s.logs.Write(d.RequestID, d.Data)
		}
		if err := s.st.CompleteLogRequest(ctx, nodeID, d.RequestID, d.Error); err != nil {
			continue
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleGetLogs returns what has arrived for a request since the caller's
// cursor, plus the request's state so a caller can tell "nothing yet" from
// "nothing ever".
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	lr, err := s.st.GetLogRequest(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	var cursor int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "cursor must be a non-negative integer", nil)
			return
		}
		cursor = n
	}
	var chunks any = []any{}
	var next = cursor
	var dropped bool
	if s.logs != nil {
		got, n, d := s.logs.Read(id, cursor)
		if got != nil {
			chunks = got
		}
		next, dropped = n, d
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request": lr, "chunks": chunks, "cursor": next, "dropped": dropped,
	})
}

// handleCloseLogRequest ends a tail. Without it a followed request stays pending
// until its TTL, and its node keeps reading Docker every tick for output that
// nobody is reading.
func (s *Server) handleCloseLogRequest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.st.CloseLogRequest(ctx, id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if s.logs != nil {
		s.logs.Drop(id)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
}
