package store

import (
	"context"

	"github.com/craig/composectl/internal/metrics"
)

func (s *Store) OperationalGauges(ctx context.Context) (metrics.Gauges, error) {
	g := metrics.Gauges{DatabaseUp: true, Deployments: map[string]int64{}}
	for _, state := range []DeploymentState{DeployPending, DeployScheduling, DeployStarting, DeployHealthy, DeployLive, DeploySuperseded, DeployFailed, DeployStopped} {
		g.Deployments[string(state)] = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT state::text, count(*) FROM deployments GROUP BY state`)
	if err != nil {
		return metrics.Gauges{}, mapErr(err)
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return metrics.Gauges{}, err
		}
		g.Deployments[state] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return metrics.Gauges{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE state='ready' AND last_heartbeat > now()-interval '30 seconds'`).Scan(&g.ReadyNodes); err != nil {
		return metrics.Gauges{}, mapErr(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM environments WHERE ephemeral AND expires_at > now()`).Scan(&g.ActivePreviews); err != nil {
		return metrics.Gauges{}, mapErr(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM environment_tombstones WHERE created_at > now()-interval '24 hours'`).Scan(&g.RecentTombstones); err != nil {
		return metrics.Gauges{}, mapErr(err)
	}
	return g, nil
}
