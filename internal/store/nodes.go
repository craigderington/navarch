package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type RegisterNodeParams struct {
	OrgID         uuid.UUID
	Hostname      string
	AdvertiseAddr string
	CPUMillis     int
	MemoryBytes   int64
	Labels        map[string]string
	AgentVersion  string
	AgeRecipient  string
}

// RegisterNode upserts by (org_id, hostname): a re-registering agent keeps
// its node identity but refreshes capacity and advertise address. A node
// that is actively registering is, by definition, ready.
func (s *Store) RegisterNode(ctx context.Context, p RegisterNodeParams) (*Node, error) {
	labels, err := json.Marshal(orEmpty(p.Labels))
	if err != nil {
		return nil, err
	}
	var n Node
	var labelsOut []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO nodes (org_id, hostname, advertise_addr, state,
		                   cpu_millis, memory_bytes, labels, agent_version, age_recipient, last_heartbeat)
		VALUES ($1,$2,$3,'ready',$4,$5,$6,NULLIF($7,''),NULLIF($8,''),now())
		ON CONFLICT (org_id, hostname) DO UPDATE SET
			advertise_addr = EXCLUDED.advertise_addr,
			state          = 'ready',
			cpu_millis     = EXCLUDED.cpu_millis,
			memory_bytes   = EXCLUDED.memory_bytes,
			labels         = EXCLUDED.labels,
			agent_version  = EXCLUDED.agent_version,
			age_recipient  = EXCLUDED.age_recipient,
			last_heartbeat = now()
		RETURNING id, org_id, hostname, host(advertise_addr), state,
		          cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		          labels, COALESCE(agent_version,''), COALESCE(age_recipient,''), last_heartbeat, created_at
	`, p.OrgID, p.Hostname, p.AdvertiseAddr, p.CPUMillis, p.MemoryBytes,
		labels, p.AgentVersion, p.AgeRecipient).
		Scan(&n.ID, &n.OrgID, &n.Hostname, &n.AdvertiseAddr, &n.State,
			&n.CPUMillis, &n.MemoryBytes, &n.AllocCPUMillis, &n.AllocMemoryBytes,
			&labelsOut, &n.AgentVersion, &n.AgeRecipient, &n.LastHeartbeat, &n.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(labelsOut, &n.Labels); err != nil {
		return nil, err
	}
	return &n, nil
}

type HeartbeatParams struct {
	AllocCPUMillis   int
	AllocMemoryBytes int64
}

func (s *Store) Heartbeat(ctx context.Context, nodeID uuid.UUID, p HeartbeatParams) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE nodes SET alloc_cpu_millis=$2, alloc_memory_bytes=$3,
		       last_heartbeat=now(), state='ready'
		WHERE id=$1 AND state <> 'retired'
	`, nodeID, p.AllocCPUMillis, p.AllocMemoryBytes)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListNodes(ctx context.Context, orgID uuid.UUID) ([]Node, error) {
	return s.queryNodes(ctx, `
		SELECT id, org_id, hostname, host(advertise_addr), state,
		       cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		       labels, COALESCE(agent_version,''), COALESCE(age_recipient,''), last_heartbeat, created_at
		FROM nodes WHERE org_id=$1 ORDER BY hostname
	`, orgID)
}

// ListReadyNodes returns nodes eligible for placement: ready and heartbeating.
func (s *Store) ListReadyNodes(ctx context.Context, orgID uuid.UUID) ([]Node, error) {
	return s.queryNodes(ctx, `
		SELECT id, org_id, hostname, host(advertise_addr), state,
		       cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		       labels, COALESCE(agent_version,''), COALESCE(age_recipient,''), last_heartbeat, created_at
		FROM nodes
		WHERE org_id=$1 AND state='ready'
		  AND last_heartbeat > now() - interval '30 seconds'
		ORDER BY hostname
	`, orgID)
}

func (s *Store) queryNodes(ctx context.Context, sql string, args ...any) ([]Node, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var n Node
		var labels []byte
		if err := rows.Scan(&n.ID, &n.OrgID, &n.Hostname, &n.AdvertiseAddr, &n.State,
			&n.CPUMillis, &n.MemoryBytes, &n.AllocCPUMillis, &n.AllocMemoryBytes,
			&labels, &n.AgentVersion, &n.AgeRecipient, &n.LastHeartbeat, &n.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(labels, &n.Labels); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
