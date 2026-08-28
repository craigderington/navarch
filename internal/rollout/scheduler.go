// Package rollout runs the two control-plane loops that turn a pending
// deployment into running, health-gated containers: the scheduler (placement)
// and the controller (health aggregation, promotion, teardown).
package rollout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/store"
)

type Scheduler struct {
	st    *store.Store
	log   *slog.Logger
	orgID *uuid.UUID
}

func newSchedulerForOrg(st *store.Store, log *slog.Logger, orgID uuid.UUID) *Scheduler {
	return &Scheduler{st: st, log: log, orgID: &orgID}
}

func NewScheduler(st *store.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{st: st, log: log}
}

// ScheduleOnce places every pending deployment onto a ready node and writes its
// desired instance rows. A deployment with no ready node is left pending to
// retry next tick; one that cannot fit is failed. An environment already bound
// to a node goes there or nowhere (see place); an unbound one is scored across
// the fleet (see bestNode).
func (sc *Scheduler) ScheduleOnce(ctx context.Context) error {
	if err := sc.st.MarkStaleNodesUnreachable(ctx); err != nil {
		return err
	}
	var pending []store.PendingDeployment
	var err error
	if sc.orgID == nil {
		pending, err = sc.st.ListPendingDeployments(ctx)
	} else {
		pending, err = sc.st.ListPendingDeploymentsForOrg(ctx, *sc.orgID)
	}
	if err != nil {
		return err
	}
	for _, dep := range pending {
		if err := sc.place(ctx, dep); err != nil {
			sc.log.Warn("placement failed", "deployment", dep.ID, "err", err)
		}
	}
	return nil
}

func (sc *Scheduler) place(ctx context.Context, dep store.PendingDeployment) error {
	nodes, err := sc.st.ListReadyNodes(ctx, dep.OrgID)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		sc.log.Info("no ready node; leaving pending", "deployment", dep.ID)
		return nil // retried next tick
	}

	peakMemory := dep.ResolvedSpec.PeakMemoryBytes()
	peakCPU := dep.ResolvedSpec.PeakCPUMillis()

	// A homed environment is not a scheduling decision. Its pinned container and
	// named volumes are on that node and cannot follow the deployment, so the
	// only choices are "there" or "nowhere" — falling back to another node when
	// the home node is unavailable would deploy over an empty volume and report
	// success, which is the failure this binding exists to prevent.
	if dep.HomeNodeID != nil {
		home := nodeByID(nodes, *dep.HomeNodeID)
		if home == nil {
			sc.log.Info("home node not ready; leaving pending",
				"deployment", dep.ID, "node", *dep.HomeNodeID)
			return nil // retried next tick; the node may come back
		}
		if home.FreeMemoryBytes() < peakMemory || home.FreeCPUMillis() < peakCPU {
			reason := fmt.Sprintf("home node %s lacks capacity for this rollout (needs %d bytes, %d millicpu)",
				home.ID, peakMemory, peakCPU)
			return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, reason)
		}
		return sc.commit(ctx, dep, home.ID, peakCPU, peakMemory)
	}

	// An ingress service no longer constrains placement. It did in Slice B, when
	// the router could only reach a tenant by joining its revision network on a
	// shared daemon — so an ingress stack had to land on the node running the
	// router. The router now connects to the node's address and a published
	// port, which works from anywhere in the fleet, so any node can host an
	// ingress stack as long as *some* node runs a router. The ingress label
	// survives as a record of where the router is; it is no longer a filter.
	homed, err := sc.st.EnvironmentsHomedPerNode(ctx, dep.OrgID)
	if err != nil {
		return err
	}
	chosen := bestNode(nodes, homed, peakCPU, peakMemory)
	if chosen == nil {
		reason := fmt.Sprintf("no ready node has %d bytes and %d millicpu free for the rollout", peakMemory, peakCPU)
		return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, reason)
	}
	return sc.commit(ctx, dep, chosen.ID, peakCPU, peakMemory)
}

func nodeByID(nodes []store.Node, id uuid.UUID) *store.Node {
	for i := range nodes {
		if nodes[i].ID == id {
			return &nodes[i]
		}
	}
	return nil
}

// commit writes the placement. A store-side ErrConflict fails the deployment
// rather than retrying: the two things it reports — the node filled up between
// the score and the write, or the environment is homed elsewhere — are both
// states a retry cannot improve.
func (sc *Scheduler) commit(ctx context.Context, dep store.PendingDeployment, nodeID uuid.UUID, peakCPU int, peakMemory int64) error {
	insts := make([]store.NewInstance, 0, len(dep.ResolvedSpec.Services))
	for name, svc := range dep.ResolvedSpec.Services {
		insts = append(insts, store.NewInstance{
			ServiceName: name, Swappable: svc.Swappable, ImageRef: svc.Image,
		})
	}
	if err := sc.st.PlaceDeployment(ctx, dep.ID, nodeID, insts, peakCPU, peakMemory); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, err.Error())
		}
		return err
	}
	return nil
}

// bestNode picks where an unhomed environment should live. The capacity check
// is a hard filter, not a score term: a node that cannot fit the rollout is not
// a worse choice, it is not a choice.
//
// Among nodes that fit, the fleet is balanced by *environments homed*, not by
// current allocation. An environment is a lasting commitment — its volumes make
// it unmovable — so spreading the thing that cannot be undone matters more than
// levelling the memory in use right now.
//
// The final tie-break is the node id, which exists purely so the same fleet
// state always produces the same choice. A scheduler whose output depends on
// the order rows came back from Postgres cannot be asserted on, and its bugs
// reproduce only sometimes.
func bestNode(nodes []store.Node, homed map[uuid.UUID]int, peakCPU int, peakMemory int64) *store.Node {
	var best *store.Node
	var bestHomed int
	var bestFree float64
	for i := range nodes {
		n := &nodes[i]
		if n.FreeMemoryBytes() < peakMemory || n.FreeCPUMillis() < peakCPU {
			continue
		}
		h := homed[n.ID]
		free := freeRatio(n)
		switch {
		case best == nil:
		case h < bestHomed:
		case h > bestHomed:
			continue
		case free > bestFree:
		case free < bestFree:
			continue
		case n.ID.String() < best.ID.String():
		default:
			continue
		}
		best, bestHomed, bestFree = n, h, free
	}
	return best
}

// freeRatio is the smaller of the two free fractions, so a node with plenty of
// memory but no CPU left scores as the constrained resource says it should.
func freeRatio(n *store.Node) float64 {
	mem, cpu := 1.0, 1.0
	if n.MemoryBytes > 0 {
		mem = float64(n.FreeMemoryBytes()) / float64(n.MemoryBytes)
	}
	if n.CPUMillis > 0 {
		cpu = float64(n.FreeCPUMillis()) / float64(n.CPUMillis)
	}
	return min(mem, cpu)
}
