// Package client is the typed HTTP client for the composectl control plane.
// It is the only package the CLI uses to talk to the API: no store, no pgx.
package client

import "time"

type Organization struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Application struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Stack struct {
	ID        string    `json:"id"`
	AppID     string    `json:"app_id"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

type StackVersion struct {
	ID         string    `json:"id"`
	StackID    string    `json:"stack_id"`
	Version    int       `json:"version"`
	SpecDigest string    `json:"spec_digest"`
	CreatedBy  string    `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Environment struct {
	ID               string            `json:"id"`
	StackID          string            `json:"stack_id"`
	Slug             string            `json:"slug"`
	Strategy         string            `json:"strategy"`
	Hostname         string            `json:"hostname,omitempty"`
	Config           map[string]string `json:"config"`
	LiveDeploymentID *string           `json:"live_deployment_id,omitempty"`
	Ephemeral        bool              `json:"ephemeral"`
	ExpiresAt        *time.Time        `json:"expires_at,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
}

type Deployment struct {
	ID              string     `json:"id"`
	EnvironmentID   string     `json:"environment_id"`
	StackVersionID  string     `json:"stack_version_id"`
	Revision        int        `json:"revision"`
	Slot            string     `json:"slot"`
	ProjectName     string     `json:"project_name"`
	State           string     `json:"state"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	CreatedBy       string     `json:"created_by,omitempty"`
	PeakMemoryBytes int64      `json:"peak_memory_bytes,omitempty"`
	PromotedAt      *time.Time `json:"promoted_at,omitempty"`
	SupersededAt    *time.Time `json:"superseded_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type Node struct {
	ID               string     `json:"id"`
	OrgID            string     `json:"org_id"`
	Hostname         string     `json:"hostname"`
	AdvertiseAddr    string     `json:"advertise_addr"`
	State            string     `json:"state"`
	CPUMillis        int        `json:"cpu_millis"`
	MemoryBytes      int64      `json:"memory_bytes"`
	AllocCPUMillis   int        `json:"alloc_cpu_millis"`
	AllocMemoryBytes int64      `json:"alloc_memory_bytes"`
	AgentVersion     string     `json:"agent_version,omitempty"`
	AgeRecipient     string     `json:"age_recipient,omitempty"`
	LastHeartbeat    *time.Time `json:"last_heartbeat,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

type SecretMeta struct {
	Key       string    `json:"key"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	ID           int64          `json:"id"`
	OrgID        *string        `json:"org_id,omitempty"`
	DeploymentID *string        `json:"deployment_id,omitempty"`
	NodeID       *string        `json:"node_id,omitempty"`
	Kind         string         `json:"kind"`
	Message      string         `json:"message"`
	Payload      map[string]any `json:"payload"`
	CreatedAt    time.Time      `json:"created_at"`
}

type ValidateResult struct {
	Valid   bool   `json:"valid"`
	Digest  string `json:"digest,omitempty"`
	Summary struct {
		Services        []string `json:"services"`
		Swappable       []string `json:"swappable"`
		Pinned          []string `json:"pinned"`
		Ingress         string   `json:"ingress,omitempty"`
		PeakMemoryBytes int64    `json:"peak_memory_bytes"`
	} `json:"summary"`
}

type Preview struct {
	EnvironmentID string     `json:"environment_id"`
	Hostname      string     `json:"hostname"`
	DeploymentID  string     `json:"deployment_id"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

type Health struct {
	Status string `json:"status"`
}
