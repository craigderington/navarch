// Package spec defines the normalized DeploymentSpec: the immutable,
// versioned artifact produced by parsing a compose file.
//
// The spec is deliberately NOT a compose file. It is a flattened,
// validated, platform-specific description of what should run. The
// parser is the only thing that knows about compose; everything
// downstream (scheduler, agent, rollout controller) speaks Spec.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// SpecVersion is bumped when the spec shape changes incompatibly.
const SpecVersion = 1

// DeploymentSpec is the unit of deployment.
type DeploymentSpec struct {
	SpecVersion int                `json:"spec_version"`
	Services    map[string]Service `json:"services"`
	Volumes     map[string]Volume  `json:"volumes,omitempty"`
	Networks    []string           `json:"networks,omitempty"`
}

// Service is one container definition within the stack.
type Service struct {
	Name  string `json:"name"`
	Image string `json:"image"`

	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`

	// Env holds fully-resolved, non-secret values only.
	Env map[string]string `json:"env,omitempty"`

	// SecretEnv holds env vars whose value contains at least one secret
	// reference. The value is a TEMPLATE retaining ${secret:KEY} markers,
	// which the agent expands at container start. Storing the template
	// rather than a bare key lets a secret be embedded in a larger string
	// (a connection URL, say) and keeps plaintext off the control plane.
	SecretEnv map[string]string `json:"secret_env,omitempty"`

	Ports   []Port        `json:"ports,omitempty"`
	Mounts  []Mount       `json:"mounts,omitempty"`
	Depends []string      `json:"depends_on,omitempty"`
	Health  *HealthCheck  `json:"healthcheck,omitempty"`
	Limits  ResourceLimit `json:"limits"`

	Restart string `json:"restart,omitempty"`

	// Swappable is false when the service owns durable state. Non-swappable
	// services are excluded from blue/green: they are started once per
	// environment and shared across revisions.
	Swappable bool `json:"swappable"`

	// Ingress marks the service that receives external traffic. Exactly
	// zero or one service per stack may set this.
	Ingress *Ingress `json:"ingress,omitempty"`
}

type Port struct {
	Container int    `json:"container"`
	Protocol  string `json:"protocol"` // tcp | udp
}

type MountKind string

const (
	MountVolume MountKind = "volume"
	MountTmpfs  MountKind = "tmpfs"
)

type Mount struct {
	Kind     MountKind `json:"kind"`
	Source   string    `json:"source,omitempty"` // named volume
	Target   string    `json:"target"`
	ReadOnly bool      `json:"read_only,omitempty"`
}

type HealthCheck struct {
	Test        []string `json:"test"`
	IntervalSec int      `json:"interval_sec"`
	TimeoutSec  int      `json:"timeout_sec"`
	Retries     int      `json:"retries"`
	StartSec    int      `json:"start_period_sec"`
}

type ResourceLimit struct {
	CPUMillis   int   `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
}

type Volume struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	// Owner is the service that mounts it. Determines node pinning.
	Owner string `json:"owner"`
}

type Ingress struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// Digest returns a stable sha256 of the canonical spec encoding. Two specs
// that would produce identical containers must produce identical digests,
// so map iteration order must not leak in. json.Marshal sorts map keys,
// and slice fields are sorted at parse time.
func (s *DeploymentSpec) Digest() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ServiceNames returns service names in deterministic order.
func (s *DeploymentSpec) ServiceNames() []string {
	out := make([]string, 0, len(s.Services))
	for name := range s.Services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SwappableServices returns the services that participate in blue/green.
func (s *DeploymentSpec) SwappableServices() []string {
	var out []string
	for _, name := range s.ServiceNames() {
		if s.Services[name].Swappable {
			out = append(out, name)
		}
	}
	return out
}

// PinnedServices returns stateful services excluded from blue/green.
func (s *DeploymentSpec) PinnedServices() []string {
	var out []string
	for _, name := range s.ServiceNames() {
		if !s.Services[name].Swappable {
			out = append(out, name)
		}
	}
	return out
}

// PeakMemoryBytes estimates memory needed during a blue/green rollout:
// pinned services run once, swappable services run twice.
func (s *DeploymentSpec) PeakMemoryBytes() int64 {
	var total int64
	for _, svc := range s.Services {
		if svc.Swappable {
			total += svc.Limits.MemoryBytes * 2
		} else {
			total += svc.Limits.MemoryBytes
		}
	}
	return total
}

// PeakCPUMillis is the CPU counterpart to PeakMemoryBytes.
func (s *DeploymentSpec) PeakCPUMillis() int {
	var total int
	for _, svc := range s.Services {
		if svc.Swappable {
			total += svc.Limits.CPUMillis * 2
		} else {
			total += svc.Limits.CPUMillis
		}
	}
	return total
}

// SecretRefPattern matches a secret marker inside a SecretEnv template.
// Exported so the agent expands templates using exactly the same syntax
// the parser recognized.
var SecretRefPattern = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_.-]+)\}`)

// RequiredSecrets returns every distinct secret key referenced anywhere in
// the spec, sorted. The rollout controller uses this to fail fast when an
// environment is missing a secret, rather than discovering it when a
// container crash-loops.
func (s *DeploymentSpec) RequiredSecrets() []string {
	seen := map[string]struct{}{}
	for _, svc := range s.Services {
		for _, tmpl := range svc.SecretEnv {
			for _, m := range SecretRefPattern.FindAllStringSubmatch(tmpl, -1) {
				seen[m[1]] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IngressService returns the name of the service handling external traffic.
func (s *DeploymentSpec) IngressService() (string, bool) {
	for _, name := range s.ServiceNames() {
		if s.Services[name].Ingress != nil {
			return name, true
		}
	}
	return "", false
}

// DurableReasons lists what ties this stack to the node it was first placed on,
// empty when nothing does. It answers one question — may this environment be
// re-homed without losing anything — and the caller reports the reasons rather
// than re-deriving them.
//
// Two things count, and the second is the one a naive check misses:
//
//   - A pinned service. It runs once, is adopted across revisions by its stable
//     name, and its container lives on exactly one node.
//   - A named volume, mounted by ANY service, read-only or not. Read-only
//     describes the container's access to the bytes, not where the bytes are:
//     the volume is `cc-{env8}-{name}` on that node's daemon and moving the
//     environment leaves it behind. A swappable service with a read-only mount
//     therefore still pins the environment, which is exactly the case that looks
//     safe and is not.
//
// Erring toward "cannot move" is the correct direction: refusing to re-home an
// environment that would have survived it costs an operator an inconvenience,
// while allowing one that would not costs them their data.
func (s *DeploymentSpec) DurableReasons() []string {
	var out []string
	for _, name := range s.PinnedServices() {
		out = append(out, fmt.Sprintf("pinned service %q", name))
	}
	seen := map[string]bool{}
	for _, name := range s.ServiceNames() {
		for _, m := range s.Services[name].Mounts {
			if m.Kind != MountVolume || m.Source == "" || seen[m.Source] {
				continue
			}
			seen[m.Source] = true
			out = append(out, fmt.Sprintf("named volume %q mounted by %q", m.Source, name))
		}
	}
	// A volume declared but mounted by nothing is rejected at parse time, so it
	// cannot reach here — but if it ever did, the bytes would still be on that
	// node and this is the answer that keeps the data safe.
	for vol := range s.Volumes {
		if !seen[vol] {
			out = append(out, fmt.Sprintf("named volume %q", vol))
		}
	}
	return out
}
