package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/spec"
)

type Organization struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Application struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"org_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Stack struct {
	ID        uuid.UUID `json:"id"`
	AppID     uuid.UUID `json:"app_id"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

type StackVersion struct {
	ID         uuid.UUID            `json:"id"`
	StackID    uuid.UUID            `json:"stack_id"`
	Version    int                  `json:"version"`
	RawCompose string               `json:"raw_compose,omitempty"`
	Spec       *spec.DeploymentSpec `json:"spec,omitempty"`
	SpecDigest string               `json:"spec_digest"`
	CreatedBy  string               `json:"created_by,omitempty"`
	CreatedAt  time.Time            `json:"created_at"`
}

type RolloutStrategy string

const (
	StrategyBlueGreen RolloutStrategy = "blue_green"
	StrategyRolling   RolloutStrategy = "rolling"
	StrategyRecreate  RolloutStrategy = "recreate"
)

type Environment struct {
	ID               uuid.UUID         `json:"id"`
	StackID          uuid.UUID         `json:"stack_id"`
	Slug             string            `json:"slug"`
	Strategy         RolloutStrategy   `json:"strategy"`
	Hostname         string            `json:"hostname,omitempty"`
	Config           map[string]string `json:"config"`
	LiveDeploymentID *uuid.UUID        `json:"live_deployment_id,omitempty"`
	// Ephemeral marks a preview environment; the reaper deletes it once
	// ExpiresAt passes. Non-ephemeral environments never expire.
	Ephemeral bool       `json:"ephemeral"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// HomeNodeID is the node holding this environment's durable state, set by
	// its first placement and never changed. Every later deployment goes there:
	// the pinned container and named volumes cannot follow it elsewhere.
	HomeNodeID *uuid.UUID `json:"home_node_id,omitempty"`
	// HomeNode is HomeNodeID's hostname, resolved by the same query that reads
	// the environment. Every consumer wants the name rather than the id — a
	// column of UUIDs tells an operator nothing — and resolving it here costs a
	// LEFT JOIN on a primary key, where doing it client-side would cost a walk
	// up to the org just to be allowed to list its nodes. Empty when the
	// environment has never been placed.
	HomeNode  string    `json:"home_node,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// OrgEnvironment is an environment with the catalog path needed to identify it
// across an organization. ListOrgEnvironments returns these because an
// environment's slug is unique only within its stack: "prod" is meaningless
// without the app and stack that own it.
type OrgEnvironment struct {
	Environment
	AppSlug   string `json:"app_slug"`
	StackSlug string `json:"stack_slug"`
}

type DeploymentState string

const (
	DeployPending    DeploymentState = "pending"
	DeployScheduling DeploymentState = "scheduling"
	DeployStarting   DeploymentState = "starting"
	DeployHealthy    DeploymentState = "healthy"
	DeployLive       DeploymentState = "live"
	DeploySuperseded DeploymentState = "superseded"
	DeployFailed     DeploymentState = "failed"
	DeployStopped    DeploymentState = "stopped"
)

// ActiveDeployStates are the non-terminal states. The partial unique index
// deployments_one_active_idx enforces at most one per environment.
var ActiveDeployStates = []DeploymentState{
	DeployPending, DeployScheduling, DeployStarting, DeployHealthy,
}

type Deployment struct {
	ID             uuid.UUID            `json:"id"`
	EnvironmentID  uuid.UUID            `json:"environment_id"`
	StackVersionID uuid.UUID            `json:"stack_version_id"`
	Revision       int                  `json:"revision"`
	Slot           string               `json:"slot"`
	ProjectName    string               `json:"project_name"`
	State          DeploymentState      `json:"state"`
	ResolvedSpec   *spec.DeploymentSpec `json:"resolved_spec"`
	PromotedAt     *time.Time           `json:"promoted_at,omitempty"`
	SupersededAt   *time.Time           `json:"superseded_at,omitempty"`
	FailureReason  string               `json:"failure_reason,omitempty"`
	CreatedBy      string               `json:"created_by,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	// HomeNode and HomeNodeState describe the node this deployment's environment
	// is bound to. They are reported ALONGSIDE State rather than folded into it:
	// a deployment whose node went quiet is still live — nothing superseded it
	// and its containers are very likely still running — and writing a state
	// change would be the control plane asserting something about a world it has
	// just admitted it cannot see. It also would not unwind: `deployments` is
	// append-only and validTransitions has no path back to live, so a 30-second
	// blip would permanently rewrite history.
	//
	// The bargain is that if State keeps telling the truth, the reader must be
	// able to see the node is unreachable in the same breath — otherwise "do not
	// lie in the state column" quietly becomes "do not tell them at all".
	HomeNode      string    `json:"home_node,omitempty"`
	HomeNodeState NodeState `json:"home_node_state,omitempty"`
}

type NodeState string

const (
	NodePending     NodeState = "pending"
	NodeReady       NodeState = "ready"
	NodeDraining    NodeState = "draining"
	NodeUnreachable NodeState = "unreachable"
	NodeRetired     NodeState = "retired"
)

type Node struct {
	ID               uuid.UUID         `json:"id"`
	OrgID            uuid.UUID         `json:"org_id"`
	Hostname         string            `json:"hostname"`
	AdvertiseAddr    string            `json:"advertise_addr"`
	State            NodeState         `json:"state"`
	CPUMillis        int               `json:"cpu_millis"`
	MemoryBytes      int64             `json:"memory_bytes"`
	AllocCPUMillis   int               `json:"alloc_cpu_millis"`
	AllocMemoryBytes int64             `json:"alloc_memory_bytes"`
	Labels           map[string]string `json:"labels"`
	AgentVersion     string            `json:"agent_version,omitempty"`
	AgeRecipient     string            `json:"age_recipient,omitempty"`
	// PendingAgeRecipient is a key the node has advertised that no operator has
	// approved. It is never sealed to: RecipientsForEnvironment does not read
	// it, because sealing to an unapproved key is the failure the pending state
	// exists to prevent. Promote it with RotateNodeRecipient.
	PendingAgeRecipient string     `json:"pending_age_recipient,omitempty"`
	LastHeartbeat       *time.Time `json:"last_heartbeat,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	// Token is the plaintext node credential, returned once at first
	// registration. It is never stored and is omitted on later reads.
	Token string `json:"token,omitempty"`
}

// FreeMemoryBytes is capacity minus current allocation.
func (n Node) FreeMemoryBytes() int64 { return n.MemoryBytes - n.AllocMemoryBytes }

// FreeCPUMillis is capacity minus current allocation.
func (n Node) FreeCPUMillis() int { return n.CPUMillis - n.AllocCPUMillis }

type InstanceState string

const (
	InstancePending   InstanceState = "pending"
	InstancePulling   InstanceState = "pulling"
	InstanceStarting  InstanceState = "starting"
	InstanceRunning   InstanceState = "running"
	InstanceUnhealthy InstanceState = "unhealthy"
	InstanceStopped   InstanceState = "stopped"
	InstanceFailed    InstanceState = "failed"
)

type ServiceInstance struct {
	ID           uuid.UUID     `json:"id"`
	DeploymentID uuid.UUID     `json:"deployment_id"`
	NodeID       uuid.UUID     `json:"node_id"`
	ServiceName  string        `json:"service_name"`
	Swappable    bool          `json:"swappable"`
	ContainerID  string        `json:"container_id,omitempty"`
	ImageRef     string        `json:"image_ref"`
	State        InstanceState `json:"state"`
	HealthStatus string        `json:"health_status,omitempty"`
	RestartCount int           `json:"restart_count"`
	LastError    string        `json:"last_error,omitempty"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

type Event struct {
	ID           int64          `json:"id"`
	OrgID        *uuid.UUID     `json:"org_id,omitempty"`
	DeploymentID *uuid.UUID     `json:"deployment_id,omitempty"`
	NodeID       *uuid.UUID     `json:"node_id,omitempty"`
	Kind         string         `json:"kind"`
	Message      string         `json:"message"`
	Payload      map[string]any `json:"payload"`
	CreatedAt    time.Time      `json:"created_at"`
	// ActorEmail is denormalized so a purged operator still leaves a readable
	// name. An audit trail that degrades to a null uuid answers "someone" when
	// the question is "who".
	ActorOperatorID *uuid.UUID `json:"actor_operator_id,omitempty"`
	ActorEmail      *string    `json:"actor_email,omitempty"`
}
