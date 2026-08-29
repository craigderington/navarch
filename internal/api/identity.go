package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/store"
)

// Who is making a request.
//
// Authentication happens in ServeHTTP, before the mux; authorization happens
// inside the handlers, after it. That split is not stylistic. ServeHTTP runs
// before any route has matched, so r.PathValue is empty there — the trap that
// once made every per-node endpoint return 401 unconditionally, and the reason
// nodeAgentPathID re-parses the path by hand. Answering "which org owns
// /v1/deployments/{id}" needs a database lookup keyed on a path id, and doing
// that before the mux would mean a second hand-written parser for every route.
// A parser that silently fails open leaks a tenant, which is a far worse
// failure than the 401 the first one caused.
//
// So: ServeHTTP answers *who*, and nothing else. Handlers answer *may they*.

type identityKind int

const (
	// identityNone is the zero value and never authorizes anything.
	identityNone identityKind = iota

	// identityOperator is a person, or a script acting as one, authenticated
	// by an operator token and authorized per organization by each handler.
	identityOperator

	// identityNode is an agent, authenticated by its own node token and
	// confined to that node's endpoints. Unchanged by operator identity.
	identityNode

	// identityService is the shared COMPOSECTL_AGENT_TOKEN. It used to open
	// every operator route; it now opens two machine-to-machine paths that
	// have no human behind them: POST /v1/nodes/register and GET /metrics.
	//
	// It survives, demoted, because an agent has no identity of its own until
	// it has registered — it authenticates that one call with the shared token
	// and receives its node token in the response. Replacing it outright would
	// mean every agent in the fleet failing to rejoin on the restart that
	// deployed the change, which is the worst possible moment to find out.
	// Metrics ride along because a scraper is not a person either, and the
	// surface carries no tenant data: the labels are route patterns and enums,
	// which the audit checked specifically.
	//
	// Removing it entirely wants an operator-issued node join token. That is a
	// real design with its own failure modes, and it is not a prerequisite for
	// closing the shared token's blast radius, which is what this is.
	identityService

	// identityJoin is a node enrolment credential: a join token, which names
	// exactly one organization. It reaches exactly one route,
	// POST /v1/nodes/register, and the org it carries is the org the node
	// joins — never the one the request body asks for.
	identityJoin
)

type identity struct {
	kind identityKind
	// operator is set for identityOperator and nil for every other kind.
	operator *store.Operator
	// nodeID is set for identityNode.
	nodeID uuid.UUID
	// orgID is set for identityJoin: the organization the presented join token
	// admits nodes to.
	orgID uuid.UUID
}

func (i identity) isOperator() bool { return i.kind == identityOperator && i.operator != nil }

// actor names who to record on an event. Empty for machine identities, which
// is honest: an agent's report has no human behind it, and inventing one would
// make the audit log worse rather than more complete.
func (i identity) actor() (*uuid.UUID, string) {
	if i.operator == nil {
		return nil, ""
	}
	return &i.operator.ID, i.operator.Email
}

type contextKey int

const identityContextKey contextKey = iota

func withIdentity(ctx context.Context, id identity) context.Context {
	return context.WithValue(ctx, identityContextKey, id)
}

// identityFrom reports the caller's identity. The second result is false when
// authentication is disabled entirely — an in-process test configuration only,
// since the control-plane config refuses an empty token at startup, so a
// deployed server always has one.
//
// Handlers treat "no identity at all" as permitted for exactly that reason.
// The consequence is that a test which does not configure a token cannot
// observe authorization, which is precisely how the 401 bug survived a green
// suite; the org-scoping tests configure one deliberately.
func identityFrom(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(identityContextKey).(identity)
	return id, ok
}
