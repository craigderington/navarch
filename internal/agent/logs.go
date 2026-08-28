package agent

import (
	"context"

	"github.com/craigderington/navarch/internal/agent/dockerd"
	"github.com/craigderington/navarch/internal/store"
)

// LogDelivery is one request's answer on its way back to the control plane.
// Data and Err are exclusive: a read either produced output or a reason it did
// not, and reporting both would leave the requester unable to tell which it got.
type LogDelivery struct {
	RequestID string
	Data      string
	Err       string
}

// CollectLogs acts on the log instructions the control plane sent with this
// node's desired state.
//
// A failure here is per-request and never aborts the tick. The alternative —
// returning on the first error — would let one container that has been removed
// between the request and the read stall reporting for every other instance on
// the node, which is the shape of bug that took a session to find the last time
// it was written that way.
//
// Nothing in this function logs the content it moves. Container stdout may
// carry a tenant's secrets, and a debug line here would write them to the
// agent's own log, which is the one place the platform's careful handling of
// secret plaintext could be undone by a print statement.
func (r *Reconciler) CollectLogs(ctx context.Context, reqs []store.PendingLogRequest) []LogDelivery {
	if len(reqs) == 0 {
		return nil
	}
	out := make([]LogDelivery, 0, len(reqs))
	for _, req := range reqs {
		opt := dockerd.LogOptions{Tail: req.TailLines}
		if req.SinceAt != nil {
			// Follow advances since_at on the control plane after each delivery,
			// so this asks only for what arrived in the last tick. Tail still
			// applies as the belt to that braces.
			opt.Since = *req.SinceAt
		}
		data, err := r.drv.ContainerLogs(ctx, req.ContainerID, opt)
		d := LogDelivery{RequestID: req.ID.String(), Data: data}
		if err != nil {
			// The container being gone is the common case, not an anomaly: a
			// tail outlives a blue/green flip routinely. The requester is told,
			// and decides whether to open a new request against the new
			// revision — the agent has no business guessing that.
			d.Data = ""
			d.Err = err.Error()
		}
		out = append(out, d)
	}
	return out
}
