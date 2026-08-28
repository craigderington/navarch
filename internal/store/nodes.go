package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
//
// A changing age_recipient is the one field a node may not simply assign.
// Every secret set afterwards for environments homed here would be sealed to
// the new key, so accepting it on the word of whoever registered is a
// credential redirect — and anyone holding the shared service token can
// register. A differing key is therefore recorded as *pending* and takes no
// effect until an operator promotes it (RotateNodeRecipient).
//
// Three cases, and they are not the same:
//
//   - no recorded recipient yet: set it. Nothing is displaced, and a node with
//     no key was excluded from every prior sealing decision anyway.
//   - same recipient: no change, and any stale pending value is cleared.
//   - a different non-empty recipient: recorded as pending; the effective one
//     is untouched.
//
// An empty incoming recipient is ignored rather than treated as a removal.
// Writing it through would let any agent erase the platform's record of its own
// key by failing to read a file — a denial-of-decryption with no operator in
// the loop. An agent that genuinely lost its identity generates a new one and
// advertises that, which is the pending case; empty means "not doing secrets
// right now", and the answer to that is to keep what we already know.
//
// The node still registers, heartbeats and keeps its capacity throughout.
// Refusing the registration would take a node out of the fleet over a key it
// has not been allowed to use, which punishes the fleet for someone else's
// request.
func (s *Store) RegisterNode(ctx context.Context, p RegisterNodeParams) (*Node, error) {
	labels, err := json.Marshal(orEmpty(p.Labels))
	if err != nil {
		return nil, err
	}
	token, tokenHash, err := newBearerToken()
	if err != nil {
		return nil, err
	}
	var n Node
	var labelsOut []byte
	var issuedHash *string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// The prior values are needed to decide whether this registration is
		// proposing a change, and RETURNING only sees post-update values — so
		// read them first. A node that does not exist yet has neither.
		var oldRecipient, oldPending *string
		if err := tx.QueryRow(ctx,
			`SELECT age_recipient, pending_age_recipient FROM nodes WHERE org_id=$1 AND hostname=$2`,
			p.OrgID, p.Hostname).Scan(&oldRecipient, &oldPending); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		err := tx.QueryRow(ctx, `
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
				-- The recipient rules above, in SQL. EXCLUDED.age_recipient is
				-- already NULLIF'd, so NULL here means "the agent advertised
				-- nothing", not "the agent asked for removal".
				age_recipient = CASE
					WHEN EXCLUDED.age_recipient IS NULL THEN nodes.age_recipient
					WHEN nodes.age_recipient IS NULL    THEN EXCLUDED.age_recipient
					ELSE nodes.age_recipient
				END,
				pending_age_recipient = CASE
					WHEN EXCLUDED.age_recipient IS NULL              THEN nodes.pending_age_recipient
					WHEN nodes.age_recipient IS NULL                 THEN NULL
					WHEN EXCLUDED.age_recipient = nodes.age_recipient THEN NULL
					ELSE EXCLUDED.age_recipient
				END,
				token_hash     = COALESCE(nodes.token_hash, EXCLUDED.token_hash),
				last_heartbeat = now()
			RETURNING id, org_id, hostname, host(advertise_addr), state,
			          cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
			          labels, COALESCE(agent_version,''), COALESCE(age_recipient,''),
			          COALESCE(pending_age_recipient,''), last_heartbeat, created_at, token_hash
		`, p.OrgID, p.Hostname, p.AdvertiseAddr, p.CPUMillis, p.MemoryBytes,
			labels, p.AgentVersion, p.AgeRecipient, tokenHash).
			Scan(&n.ID, &n.OrgID, &n.Hostname, &n.AdvertiseAddr, &n.State,
				&n.CPUMillis, &n.MemoryBytes, &n.AllocCPUMillis, &n.AllocMemoryBytes,
				&labelsOut, &n.AgentVersion, &n.AgeRecipient, &n.PendingAgeRecipient,
				&n.LastHeartbeat, &n.CreatedAt, &issuedHash)
		if err != nil {
			return err
		}
		// A newly pending key is worth an event; the same key still pending is
		// not. An agent that restarts in a loop re-registers each time, and a
		// timeline that repeats the same request forever buries the one entry
		// an operator needs to see.
		proposed := p.AgeRecipient != "" && oldRecipient != nil && *oldRecipient != "" &&
			p.AgeRecipient != *oldRecipient
		alreadyPending := oldPending != nil && *oldPending == p.AgeRecipient
		if proposed && !alreadyPending {
			if err := appendEventTx(ctx, tx, p.OrgID, nil, &n.ID,
				"node.recipient_rotation_pending",
				"node advertised a new age recipient; it takes no effect until an operator rotates it",
				map[string]any{"hostname": p.Hostname}); err != nil {
				return err
			}
		}
		return nil
	})
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

// newBearerToken mints the one token format this codebase uses: 32 bytes of
// crypto/rand, hex-encoded, with only its SHA-256 stored. Shared by node
// registration and operator token issuance so there is never a second scheme
// to reason about.
func newBearerToken() (plain, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	plain = hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(plain))
	return plain, hex.EncodeToString(sum[:]), nil
}

func hashBearerToken(plain string) string {
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
	got := hashBearerToken(plain)
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

// UncordonNode returns a drained node to service. Draining was a one-way door
// until this existed: DrainNode sets `draining`, Heartbeat's CASE preserves it,
// and RegisterNode's upsert preserves it too, so not even reinstalling the agent
// cleared the flag — the only way back was hand-written SQL against a live
// database. A cordon an operator cannot lift is a worse tool than no cordon.
//
// The state it lands in is derived from the last heartbeat rather than declared.
// Uncordoning removes an operator's *intent* not to schedule here; it says
// nothing about whether the node is alive, and only the heartbeat knows that.
// Declaring `ready` outright would put a node that has been silent for an hour
// into that state in `navarch node list` — a lie on exactly the surface the
// operator is watching, even though ListReadyNodes' own freshness filter would
// have kept work off it. Declaring `unreachable` outright would libel a node
// that is heartbeating perfectly well. So the CASE below asks the same question
// MarkStaleNodesUnreachable and ListReadyNodes ask, with the same 30-second
// window, and either answer is self-correcting: a live agent's next heartbeat
// promotes `unreachable` to `ready` within one poll interval, and a dead one
// stays `unreachable`, which is true.
//
// `retired` is refused rather than lifted. Heartbeat will not touch a retired
// node (`WHERE state <> 'retired'`), so resurrecting one here would produce a
// row claiming readiness that nothing can ever update again.
//
// Uncordoning a node that is not draining succeeds and changes nothing. The
// caller asked for a node that is not cordoned and that is what they have; a
// 404 here would report "no such node" for a node plainly in the listing.
func (s *Store) UncordonNode(ctx context.Context, nodeID uuid.UUID) error {
	return mapErr(s.tx(ctx, func(tx pgx.Tx) error {
		var state NodeState
		if err := tx.QueryRow(ctx,
			`SELECT state FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&state); err != nil {
			return err // pgx.ErrNoRows -> ErrNotFound via mapErr
		}
		if state == NodeRetired {
			return fmt.Errorf("%w: node is retired and cannot be returned to service", ErrConflict)
		}
		if state != NodeDraining {
			return nil
		}
		// The explicit ::node_state is not optional: every branch is an
		// unknown-type literal, so Postgres resolves the CASE as text and then
		// refuses to assign text to the enum column. Same trap documented on
		// Heartbeat above, which is where it was first hit.
		_, err := tx.Exec(ctx, `
			UPDATE nodes SET state = (CASE
			    WHEN last_heartbeat > now() - interval '30 seconds' THEN 'ready'
			    ELSE 'unreachable'
			END)::node_state
			WHERE id=$1
		`, nodeID)
		return err
	}))
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
		       labels, COALESCE(agent_version,''), COALESCE(age_recipient,''),
		       COALESCE(pending_age_recipient,''), last_heartbeat, created_at
		FROM nodes WHERE org_id=$1 ORDER BY hostname
	`, orgID)
}

// ListReadyNodes returns nodes eligible for placement: ready and heartbeating.
func (s *Store) ListReadyNodes(ctx context.Context, orgID uuid.UUID) ([]Node, error) {
	return s.queryNodes(ctx, `
		SELECT id, org_id, hostname, host(advertise_addr), state,
		       cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		       labels, COALESCE(agent_version,''), COALESCE(age_recipient,''),
		       COALESCE(pending_age_recipient,''), last_heartbeat, created_at
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
			&labels, &n.AgentVersion, &n.AgeRecipient, &n.PendingAgeRecipient,
			&n.LastHeartbeat, &n.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(labels, &n.Labels); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// EnvironmentsHomedPerNode counts the environments bound to each node in an
// organization. It feeds the scheduler's spread term, and it counts *homed*
// environments rather than running deployments on purpose: a binding outlives
// any individual deployment, including a failed one, because the volumes it was
// created under are what make it permanent.
//
// Nodes with no environments are absent from the map rather than present with
// zero; the caller looks up a missing key as zero, which is what it means.
func (s *Store) EnvironmentsHomedPerNode(ctx context.Context, orgID uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.home_node_id, count(*)
		FROM environments e
		JOIN stacks s       ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE a.org_id = $1 AND e.home_node_id IS NOT NULL
		GROUP BY e.home_node_id
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// RotateNodeRecipient promotes the key a node has been advertising to the one
// its environments' secrets are sealed to.
//
// The operator does not supply the key — they approve the one already recorded
// as pending. That is the only workable shape: the agent generates its identity
// and the control plane never sees the private half, so what a human is
// asserting here is "this node legitimately has a new key", which is precisely
// the judgement the control plane cannot make and they can.
//
// Nothing pending is ErrConflict rather than a quiet success. A rotation that
// reports "done" without having changed anything is exactly the ambiguity this
// whole path exists to remove.
func (s *Store) RotateNodeRecipient(ctx context.Context, nodeID uuid.UUID) (*Node, error) {
	var n *Node
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var orgID uuid.UUID
		var hostname, promoted string
		err := tx.QueryRow(ctx, `
			UPDATE nodes
			SET age_recipient = pending_age_recipient,
			    pending_age_recipient = NULL
			WHERE id = $1
			  AND pending_age_recipient IS NOT NULL
			  AND pending_age_recipient <> ''
			RETURNING org_id, hostname, age_recipient
		`, nodeID).Scan(&orgID, &hostname, &promoted)
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing was promoted. Separate "no such node" from "nothing to
			// do", because they call for different actions from the operator.
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT true FROM nodes WHERE id=$1`, nodeID).Scan(&exists); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			return fmt.Errorf("%w: node has no pending age recipient", ErrConflict)
		}
		if err != nil {
			return err
		}
		// The event names the promotion, not the request — node.recipient_rotated
		// now means it actually took effect. The actor rides on the context.
		if err := appendEventTx(ctx, tx, orgID, nil, &nodeID,
			"node.recipient_rotated", "operator promoted the node's pending age recipient",
			map[string]any{"hostname": hostname}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	n, err = s.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return n, nil
}
