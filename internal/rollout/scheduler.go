// Package rollout runs the two control-plane loops that turn a pending
// deployment into running, health-gated containers: the scheduler (placement)
// and the controller (health aggregation, promotion, teardown).
package rollout

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
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
// retry next tick; one that cannot fit is failed. Placement is trivial in
// Sprint 2 (first ready node with room); Sprint 4 adds scoring.
func (sc *Scheduler) ScheduleOnce(ctx context.Context) error {
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
	var chosen *store.Node
	for i := range nodes {
		if nodes[i].FreeMemoryBytes() >= peakMemory && nodes[i].FreeCPUMillis() >= peakCPU {
			chosen = &nodes[i]
			break
		}
	}
	if chosen == nil {
		reason := fmt.Sprintf("no ready node has %d bytes and %d millicpu free for the rollout", peakMemory, peakCPU)
		return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, reason)
	}

	insts := make([]store.NewInstance, 0, len(dep.ResolvedSpec.Services))
	for name, svc := range dep.ResolvedSpec.Services {
		insts = append(insts, store.NewInstance{
			ServiceName: name, Swappable: svc.Swappable, ImageRef: svc.Image,
		})
	}
	if err := sc.st.CreateServiceInstances(ctx, dep.ID, chosen.ID, insts); err != nil {
		return err
	}
	return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployScheduling, "")
}
