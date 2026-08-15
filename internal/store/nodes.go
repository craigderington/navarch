package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
// that is actively registering is, by definition, ready. A plaintext token
// is issued only when the node has no token_hash yet (first register or
// post-migration); re-registration does not rotate it.
func (s *Store) RegisterNode(ctx context.Context, p RegisterNodeParams) (*Node, error) {
	labels, err := json.Marshal(orEmpty(p.Labels))
	if err != nil {
		return nil, err
	}
	token, tokenHash, err := newNodeToken()
	if err != nil {
		return nil, err
	}
	var n Node
	var labelsOut []byte
	var issuedHash *string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO nodes (org_id, hostname, advertise_addr, state,
		                   cpu_millis, memory_bytes, labels, agent_version, age_recipient,
		                   token_hash, last_heartbeat)
		VALUES ($1,$2,$3,'ready',$4,$5,$6,NULLIF($7,''),NULLIF($8,''),$9,now())
		ON CONFLICT (org_id, hostname) DO UPDATE SET
			advertise_addr = EXCLUDED.advertise_addr,
			state          = CASE WHEN nodes.state = 'draining' THEN nodes.state ELSE 'ready' END,
			cpu_millis     = EXCLUDED.cpu_millis,
			memory_bytes   = EXCLUDED.memory_bytes,
			labels         = EXCLUDED.labels,
			agent_version  = EXCLUDED.agent_version,
			age_recipient  = EXCLUDED.age_recipient,
			token_hash     = COALESCE(nodes.token_hash, EXCLUDED.token_hash),
			last_heartbeat = now()
		RETURNING id, org_id, hostname, host(advertise_addr), state,
		          cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		          labels, COALESCE(agent_version,''), COALESCE(age_recipient,''),
		          last_heartbeat, created_at, token_hash
	`, p.OrgID, p.Hostname, p.AdvertiseAddr, p.CPUMillis, p.MemoryBytes,
		labels, p.AgentVersion, p.AgeRecipient, tokenHash).
		Scan(&n.ID, &n.OrgID, &n.Hostname, &n.AdvertiseAddr, &n.State,
			&n.CPUMillis, &n.MemoryBytes, &n.AllocCPUMillis, &n.AllocMemoryBytes,
			&labelsOut, &n.AgentVersion, &n.AgeRecipient, &n.LastHeartbeat, &n.CreatedAt,
			&issuedHash)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(labelsOut, &n.Labels); err != nil {
		return nil, err
	}
	if issuedHash != nil && *issuedHash == tokenHash {
		n.Token = token
	}
	return &n, nil
}

func newNodeToken() (plain, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	plain = hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(plain))
	return plain, hex.EncodeToString(sum[:]), nil
}

func hashNodeToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// NodeTokenValid reports whether plaintext is this node's current token.
func (s *Store) NodeTokenValid(ctx context.Context, nodeID uuid.UUID, plain string) (bool, error) {
	var hash *string
	err := s.pool.QueryRow(ctx, `SELECT token_hash FROM nodes WHERE id=$1`, nodeID).Scan(&hash)
	if err != nil {
		return false, mapErr(err)
	}
	if hash == nil || *hash == "" || plain == "" {
		return false, nil
	}
	got := hashNodeToken(plain)
	if len(got) != len(*hash) {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(*hash)) == 1, nil
}

type HeartbeatParams struct {
	AllocCPUMillis   int
	AllocMemoryBytes int64
}

func (s *Store) Heartbeat(ctx context.Context, nodeID uuid.UUID, p HeartbeatParams) error {
	// Alloc is owned by PlaceDeployment / DeleteInstances. Heartbeat only
	// proves liveness and will not un-drain a node or clobber a reservation.
	// The CASE needs an explicit ::node_state. Every branch is an unknown-type
	// literal, so Postgres resolves the CASE as text and then refuses to
	// assign text to the enum column ("column \"state\" is of type node_state
	// but expression is of type text"). A bare `state='draining'` coerces
	// fine, which is why DrainNode never hit this — it is the CASE that
	// forces the resolution. Same family as the deployment-state enum
	// gotchas: enum columns need the cast spelled out.
	tag, err := s.pool.Exec(ctx, `
		UPDATE nodes SET last_heartbeat=now(),
		       state = (CASE
		           WHEN state = 'draining' THEN 'draining'
		           WHEN state = 'retired' THEN 'retired'
		           ELSE 'ready'
		       END)::node_state
		WHERE id=$1 AND state <> 'retired'
	`, nodeID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DrainNode(ctx context.Context, nodeID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE nodes SET state='draining'
		WHERE id=$1 AND state IN ('ready','unreachable')
	`, nodeID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MarkStaleNodesUnreachable(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET state='unreachable'
		WHERE state='ready'
		  AND (last_heartbeat IS NULL OR last_heartbeat < now() - interval '30 seconds')
	`)
	return mapErr(err)
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
