// Package parser converts a compose file into a normalized DeploymentSpec.
//
// This is the ONLY package that imports compose-go. Everything downstream
// speaks spec.DeploymentSpec. That boundary is deliberate: it means we can
// swap the compose implementation, or accept other input formats, without
// touching the scheduler or agent.
package parser

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/craig/composectl/internal/spec"
)

// Defaults applied when the compose file omits resource limits. Without
// these the scheduler has no basis for placement and a stack could
// consume an entire node.
const (
	DefaultCPUMillis   = 250
	DefaultMemoryBytes = 256 << 20 // 256 MiB
)

// secretRefPattern matches ${secret:KEY} anywhere in an environment value.
// It is deliberately NOT anchored: secrets are commonly embedded inside a
// larger string, e.g.
//
//	DATABASE_URL: postgres://app:${secret:db_password}@db:5432/app
//
// An anchored pattern would store that value as a literal and hand the
// container a broken URL, with the failure surfacing only at runtime.
var secretRefPattern = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_.-]+)\}`)

// Parse loads compose YAML and returns a validated DeploymentSpec.
// projectName is used only to satisfy compose-go; it does not appear in
// the resulting spec, which must be project-name independent so the same
// spec can be deployed to multiple environments.
func Parse(ctx context.Context, raw []byte, projectName string) (*spec.DeploymentSpec, error) {
	// File-loading directives are rejected from the raw document so the
	// loader never opens a tenant-supplied path on the control plane.
	if verrs := preflight(raw); len(verrs) > 0 {
		sort.Slice(verrs, func(i, j int) bool { return verrs[i].Field < verrs[j].Field })
		return nil, verrs
	}

	// Load with compose-go's consistency check deferred. That check is
	// fail-fast: it returns on the first problem, and two of the problems
	// it looks for (dependency cycles, depends_on naming an unknown
	// service) are ones we report ourselves with proper field paths. Left
	// enabled it short-circuits the load and every platform violation in
	// the file goes unreported, which defeats collecting them in one pass.
	// It is re-applied below once our own checks are clean — deferred, not
	// discarded, because it also covers rules we do not model.
	project, err := load(ctx, raw, projectName, true)
	if err != nil {
		return nil, fmt.Errorf("load compose: %w", err)
	}

	s := &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services:    make(map[string]spec.Service, len(project.Services)),
		Volumes:     make(map[string]spec.Volume),
	}

	var verrs ValidationErrors

	for name, svc := range project.Services {
		converted, errs := convertService(name, svc)
		verrs = append(verrs, errs...)
		s.Services[name] = converted
	}

	// Register named volumes and assign ownership. Ownership determines
	// node pinning: the volume lives wherever its owning service runs.
	misparsed := misparsedVolumeNames(project.Services)
	for volName, vol := range project.Volumes {
		if len(vol.DriverOpts) > 0 {
			verrs = append(verrs, ValidationError{
				Field:   "volumes." + volName,
				Message: "volume driver_opts are not supported",
			})
		}
		if vol.Driver != "" && vol.Driver != "local" {
			verrs = append(verrs, ValidationError{
				Field:   "volumes." + volName,
				Message: "custom volume drivers are not supported",
			})
		}
		if vol.External {
			verrs = append(verrs, ValidationError{
				Field:   "volumes." + volName,
				Message: "external volumes are not supported",
			})
		}
		owner, count := volumeOwner(project.Services, volName)
		if count == 0 {
			if misparsed[volName] {
				// The volume *is* mounted; the mount just didn't survive
				// parsing. convertService already reported that precisely,
				// at the offending line. Saying "not mounted" here as well
				// would point the author at the wrong place entirely.
				continue
			}
			// Declared but unmounted. Harmless, but almost always a typo.
			verrs = append(verrs, ValidationError{
				Field:   "volumes." + volName,
				Message: "volume is declared but not mounted by any service",
			})
			continue
		}
		if count > 1 {
			verrs = append(verrs, ValidationError{
				Field:   "volumes." + volName,
				Message: "volume is mounted by multiple services; shared writable volumes are not supported",
			})
		}
		s.Volumes[volName] = spec.Volume{
			Name:   volName,
			Driver: "local",
			Owner:  owner,
		}
	}

	if errs := validateGraph(s); len(errs) > 0 {
		verrs = append(verrs, errs...)
	}
	if len(s.Services) == 0 {
		verrs = append(verrs, ValidationError{
			Field:   "services",
			Message: "services is empty; a stack must declare at least one service",
		})
	}

	if len(verrs) > 0 {
		sort.Slice(verrs, func(i, j int) bool { return verrs[i].Field < verrs[j].Field })
		return nil, verrs
	}

	// Our checks are clean, so nothing of ours is left to collect and
	// compose-go's consistency check can no longer mask anything. Apply it
	// now as a final gate: it validates rules we deliberately do not model
	// (healthcheck test form, mounts naming an undeclared volume, legacy
	// mem_limit/cpus conflicting with deploy.resources), and no stack may
	// reach a deployment without passing it.
	if _, err := load(ctx, raw, projectName, false); err != nil {
		return nil, fmt.Errorf("load compose: %w", err)
	}
	return s, nil
}

// load runs compose-go's loader. deferConsistency skips the fail-fast
// consistency pass; see Parse for why that is done in two stages. Details
// are rebuilt per call because the loader caches parsed content into them.
func load(ctx context.Context, raw []byte, projectName string, deferConsistency bool) (*types.Project, error) {
	details := types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: raw}},
		Environment: types.Mapping{},
	}
	return loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		// Interpolation of host env vars is meaningless on the control
		// plane — the control plane's environment is not the deploy
		// target's. Values come from env config + secrets instead.
		o.SkipInterpolation = true
		o.SkipResolveEnvironment = true
		o.ResolvePaths = false
		o.SkipConsistencyCheck = deferConsistency
		// Defense in depth: preflight already rejected these, but if a
		// future loader path bypasses preflight the files still must
		// not be opened.
		o.SkipInclude = true
		o.SkipExtends = true
	})
}

func convertService(name string, svc types.ServiceConfig) (spec.Service, ValidationErrors) {
	var errs ValidationErrors
	field := func(f string) string { return "services." + name + "." + f }

	out := spec.Service{
		Name:       name,
		Image:      svc.Image,
		Command:    []string(svc.Command),
		Entrypoint: []string(svc.Entrypoint),
		WorkingDir: svc.WorkingDir,
		User:       svc.User,
		Restart:    svc.Restart,
		Env:        map[string]string{},
		SecretEnv:  map[string]string{},
		Swappable:  true,
	}

	// --- rejected directives -------------------------------------------
	// These break the platform's isolation model. Reject rather than
	// silently drop: silently dropping produces a stack that runs but
	// behaves differently than the author expects.

	if svc.Image == "" {
		errs = append(errs, ValidationError{field("image"),
			"image is required; build directives are not supported (pre-build and push instead)"})
	}
	if svc.Privileged {
		errs = append(errs, ValidationError{field("privileged"),
			"privileged containers are not permitted"})
	}
	if svc.NetworkMode != "" && svc.NetworkMode != "bridge" {
		errs = append(errs, ValidationError{field("network_mode"),
			"custom network_mode is not permitted; each revision gets an isolated network"})
	}
	if svc.ContainerName != "" {
		errs = append(errs, ValidationError{field("container_name"),
			"container_name is not permitted; names are assigned per revision to avoid collisions"})
	}
	if len(svc.CapAdd) > 0 {
		errs = append(errs, ValidationError{field("cap_add"),
			"added capabilities are not permitted"})
	}
	if len(svc.CapDrop) > 0 {
		errs = append(errs, ValidationError{field("cap_drop"),
			"capability changes are not permitted"})
	}
	if svc.Pid != "" {
		errs = append(errs, ValidationError{field("pid"),
			"custom pid mode is not permitted"})
	}
	if svc.Ipc != "" {
		errs = append(errs, ValidationError{field("ipc"),
			"custom ipc mode is not permitted"})
	}
	if svc.Uts != "" {
		errs = append(errs, ValidationError{field("uts"),
			"custom uts mode is not permitted"})
	}
	if svc.UserNSMode != "" {
		errs = append(errs, ValidationError{field("userns_mode"),
			"custom userns_mode is not permitted"})
	}
	if svc.Runtime != "" {
		errs = append(errs, ValidationError{field("runtime"),
			"custom runtime is not permitted"})
	}
	if len(svc.Devices) > 0 {
		errs = append(errs, ValidationError{field("devices"),
			"host device passthrough is not permitted"})
	}
	if len(svc.DeviceCgroupRules) > 0 {
		errs = append(errs, ValidationError{field("device_cgroup_rules"),
			"device cgroup rules are not permitted"})
	}
	if len(svc.Gpus) > 0 {
		errs = append(errs, ValidationError{field("gpus"),
			"gpu passthrough is not permitted"})
	}
	if len(svc.SecurityOpt) > 0 {
		errs = append(errs, ValidationError{field("security_opt"),
			"security_opt is not permitted"})
	}
	if len(svc.Sysctls) > 0 {
		errs = append(errs, ValidationError{field("sysctls"),
			"sysctls are not permitted"})
	}
	if svc.CgroupParent != "" || svc.Cgroup != "" {
		errs = append(errs, ValidationError{field("cgroup"),
			"custom cgroup configuration is not permitted"})
	}
	if len(svc.VolumesFrom) > 0 {
		errs = append(errs, ValidationError{field("volumes_from"),
			"volumes_from is not permitted"})
	}
	if len(svc.ExtraHosts) > 0 {
		errs = append(errs, ValidationError{field("extra_hosts"),
			"extra_hosts is not permitted"})
	}
	if svc.Logging != nil || svc.LogDriver != "" || len(svc.LogOpt) > 0 {
		errs = append(errs, ValidationError{field("logging"),
			"custom logging drivers are not permitted"})
	}
	if svc.Isolation != "" {
		errs = append(errs, ValidationError{field("isolation"),
			"custom isolation is not permitted"})
	}
	if len(svc.StorageOpt) > 0 {
		errs = append(errs, ValidationError{field("storage_opt"),
			"storage_opt is not permitted"})
	}
	if svc.VolumeDriver != "" {
		errs = append(errs, ValidationError{field("volume_driver"),
			"volume_driver is not permitted"})
	}
	if svc.CredentialSpec != nil {
		errs = append(errs, ValidationError{field("credential_spec"),
			"credential_spec is not permitted"})
	}
	if svc.OomKillDisable {
		errs = append(errs, ValidationError{field("oom_kill_disable"),
			"oom_kill_disable is not permitted"})
	}
	if svc.Build != nil {
		errs = append(errs, ValidationError{field("build"),
			"build directives are not supported (pre-build and push instead)"})
	}
	if len(svc.Secrets) > 0 {
		errs = append(errs, ValidationError{field("secrets"),
			"compose secrets are not supported; use ${secret:KEY} environment templates"})
	}
	if len(svc.Configs) > 0 {
		errs = append(errs, ValidationError{field("configs"),
			"compose configs are not supported"})
	}
	if len(svc.Tmpfs) > 0 {
		errs = append(errs, ValidationError{field("tmpfs"),
			"tmpfs is not supported"})
	}
	if len(svc.Links) > 0 || len(svc.ExternalLinks) > 0 {
		errs = append(errs, ValidationError{field("links"),
			"links are not permitted"})
	}

	// --- environment & secrets ------------------------------------------

	for k, v := range svc.Environment {
		if v == nil {
			// `FOO` with no value: resolved from the deploy environment.
			continue
		}
		// A value containing any secret marker is stored as a template in
		// SecretEnv and expanded by the agent. Values without markers are
		// plain config and live in Env.
		if secretRefPattern.MatchString(*v) {
			out.SecretEnv[k] = *v
			continue
		}
		out.Env[k] = *v
	}

	// --- ports ----------------------------------------------------------
	// Published host ports are rejected: two revisions of the same stack
	// would collide on the host port. External access goes through the
	// router, which addresses containers on the revision network.

	for _, p := range svc.Ports {
		if p.Published != "" {
			errs = append(errs, ValidationError{field("ports"),
				fmt.Sprintf("published host port %q is not permitted; declare x-composectl.ingress instead", p.Published)})
			continue
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		out.Ports = append(out.Ports, spec.Port{Container: int(p.Target), Protocol: proto})
	}
	sort.Slice(out.Ports, func(i, j int) bool {
		if out.Ports[i].Container != out.Ports[j].Container {
			return out.Ports[i].Container < out.Ports[j].Container
		}
		return out.Ports[i].Protocol < out.Ports[j].Protocol
	})

	// --- mounts ---------------------------------------------------------
	// A writable named volume no longer *decides* the classification — the
	// author does, via x-composectl.rollout. It survives as a constraint:
	// two writers on one filesystem is still refused below. Recorded here,
	// enforced once the declaration has been read.

	var hasWritableVolume bool

	for _, v := range svc.Volumes {
		switch v.Type {
		case "volume":
			// An empty Source means compose-go produced an *anonymous*
			// volume. Two very different inputs land here, and both must be
			// rejected before the mount is recorded — otherwise the service
			// is silently pinned out of blue/green by durable state its
			// author never declared.
			if v.Source == "" {
				if name, target, ok := driveLetterMount(v.Target); ok {
					errs = append(errs, ValidationError{field("volumes"), fmt.Sprintf(
						"mount %q was read as a Windows drive path, not volume %q mounted at %q: "+
							"a single-character volume name is ambiguous. Rename the volume or use the long mount syntax.",
						v.Target, name, target)})
				} else {
					errs = append(errs, ValidationError{field("volumes"), fmt.Sprintf(
						"anonymous volume at %q is not supported; declare a named volume so it can be tracked and pinned to a node",
						v.Target)})
				}
				continue
			}
			out.Mounts = append(out.Mounts, spec.Mount{
				Kind:     spec.MountVolume,
				Source:   v.Source,
				Target:   v.Target,
				ReadOnly: v.ReadOnly,
			})
			if !v.ReadOnly {
				hasWritableVolume = true
			}
		case "tmpfs":
			errs = append(errs, ValidationError{field("volumes"),
				fmt.Sprintf("tmpfs mount at %q is not supported", v.Target)})
		case "bind":
			errs = append(errs, ValidationError{field("volumes"),
				fmt.Sprintf("bind mount %q is not permitted; host paths are not portable across nodes", v.Source)})
		default:
			errs = append(errs, ValidationError{field("volumes"),
				fmt.Sprintf("unsupported mount type %q", v.Type)})
		}
	}
	sort.Slice(out.Mounts, func(i, j int) bool { return out.Mounts[i].Target < out.Mounts[j].Target })

	// --- depends_on -----------------------------------------------------

	for dep, cfg := range svc.DependsOn {
		if cfg.Condition != "" && cfg.Condition != types.ServiceConditionStarted {
			errs = append(errs, ValidationError{field("depends_on"),
				fmt.Sprintf("depends_on condition %q is not supported; services start in dependency order", cfg.Condition)})
		}
		out.Depends = append(out.Depends, dep)
	}
	sort.Strings(out.Depends)

	// --- healthcheck ----------------------------------------------------

	if hc := svc.HealthCheck; hc != nil && !hc.Disable {
		out.Health = &spec.HealthCheck{
			Test:        []string(hc.Test),
			IntervalSec: durSecs(hc.Interval, 10),
			TimeoutSec:  durSecs(hc.Timeout, 5),
			Retries:     int(uint64OrDefault(hc.Retries, 3)),
			StartSec:    durSecs(hc.StartPeriod, 0),
		}
	}

	// --- resource limits ------------------------------------------------

	out.Limits = spec.ResourceLimit{
		CPUMillis:   DefaultCPUMillis,
		MemoryBytes: DefaultMemoryBytes,
	}
	if svc.Deploy != nil && svc.Deploy.Resources.Limits != nil {
		lim := svc.Deploy.Resources.Limits
		if lim.NanoCPUs > 0 {
			out.Limits.CPUMillis = int(float64(lim.NanoCPUs) * 1000)
		}
		if lim.MemoryBytes > 0 {
			out.Limits.MemoryBytes = int64(lim.MemoryBytes)
		}
	}
	if svc.Deploy != nil && svc.Deploy.Replicas != nil && *svc.Deploy.Replicas != 1 {
		errs = append(errs, ValidationError{field("deploy.replicas"),
			"replicas are managed by the platform, not the compose file"})
	}
	// Legacy `scale:` is checked on its own rather than via Deploy.Replicas.
	// compose-go folds Scale into Deploy.Replicas only inside the
	// consistency pass, and only when a deploy block already exists — so
	// reading it through Deploy would miss a bare `scale:` entirely and
	// depend on a mutation we deliberately defer.
	if svc.Scale != nil && *svc.Scale != 1 {
		errs = append(errs, ValidationError{field("scale"),
			"scale is managed by the platform, not the compose file"})
	}

	// --- ingress (x-composectl extension) --------------------------------

	if ing, err := parseIngressExtension(svc.Extensions); err != nil {
		errs = append(errs, ValidationError{field("x-composectl.ingress"), err.Error()})
	} else if ing != nil {
		out.Ingress = ing
	}

	// --- rollout mode (x-composectl extension) ---------------------------
	// Cardinality is declared, never inferred. A writable named volume tells
	// you two processes would corrupt a filesystem; it tells you nothing
	// about whether running a program twice is semantically equivalent to
	// running it once. Those are different properties, and the platform used
	// to compute the first and act on the second.
	//
	// The gap was the effect-singleton: a scheduler, cron runner, migration
	// step or broker that owns no local state but whose correctness assumes
	// exactly one instance. It mounts nothing, so the old rule called it
	// swappable and blue/green ran two copies against the shared pinned
	// database — every periodic task firing twice, external side effects and
	// all, while the rollout reported success. No compose-visible property
	// distinguishes that service from a stateless worker, so the platform
	// stops guessing and requires the author to say.
	//
	// Mandatory rather than opt-in: an optional `pin` is exactly the field
	// forgotten by the author who has not realised blue/green changes
	// cardinality, and that author is the one the rule exists to protect.
	// Missing is a parse error, so there is no default to forget.

	mode, declared, err := parseRolloutExtension(svc.Extensions)
	switch {
	case err != nil:
		errs = append(errs, ValidationError{field("x-composectl.rollout"), err.Error()})
	case !declared:
		errs = append(errs, ValidationError{field("x-composectl.rollout"),
			"rollout mode must be declared: swap (duplicated blue/green) or pin (one instance shared across revisions)"})
	default:
		out.Swappable = mode == RolloutSwap
	}

	// The volume rule, demoted from definition to constraint. Duplicating a
	// service that writes a named volume puts two processes on one
	// filesystem — the failure the classification existed to prevent, and
	// the one thing an author may not declare their way into.
	if declared && mode == RolloutSwap && hasWritableVolume {
		errs = append(errs, ValidationError{field("x-composectl.rollout"),
			"service declares rollout: swap but mounts a writable named volume; two revisions would write one filesystem"})
	}

	// Ingress must participate in blue/green or there is no zero-downtime
	// rollout to speak of. Previously this was caught in validateGraph via
	// the volume inference; it is a declaration conflict now, and belongs
	// with the declaration so the error names the field the author wrote.
	if declared && mode == RolloutPin && out.Ingress != nil {
		errs = append(errs, ValidationError{field("x-composectl.rollout"),
			"ingress service declares rollout: pin; it cannot participate in blue/green rollout"})
	}

	return out, errs
}

// RolloutMode is the declared cardinality of a service during a rollout.
type RolloutMode string

const (
	// RolloutSwap duplicates the service: both revisions run their own copy,
	// each on its own revision network.
	RolloutSwap RolloutMode = "swap"
	// RolloutPin runs the service once and attaches it to every revision's
	// network, so both revisions address the same container.
	RolloutPin RolloutMode = "pin"
)

// parseRolloutExtension reads the rollout mode from the x-composectl block:
//
//	x-composectl:
//	  rollout: swap
//
// Returns whether it was declared at all, so a missing declaration is
// reported as a missing declaration rather than defaulting silently.
func parseRolloutExtension(ext types.Extensions) (RolloutMode, bool, error) {
	rawExt, ok := ext["x-composectl"]
	if !ok {
		return "", false, nil
	}
	m, ok := rawExt.(map[string]any)
	if !ok {
		// parseIngressExtension reports the same malformed block; saying it
		// twice would double every error for one typo.
		return "", false, nil
	}
	raw, ok := m["rollout"]
	if !ok {
		return "", false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("rollout must be a string: %q or %q", RolloutSwap, RolloutPin)
	}
	switch RolloutMode(s) {
	case RolloutSwap, RolloutPin:
		return RolloutMode(s), true, nil
	default:
		return "", true, fmt.Errorf("unknown rollout mode %q; must be %q or %q", s, RolloutSwap, RolloutPin)
	}
}

// parseIngressExtension reads the x-composectl extension block:
//
//	x-composectl:
//	  ingress:
//	    port: 8080
//	    path: /
func parseIngressExtension(ext types.Extensions) (*spec.Ingress, error) {
	rawExt, ok := ext["x-composectl"]
	if !ok {
		return nil, nil
	}
	m, ok := rawExt.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("x-composectl must be a mapping")
	}
	rawIng, ok := m["ingress"]
	if !ok {
		return nil, nil
	}
	im, ok := rawIng.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ingress must be a mapping")
	}
	port, ok := toInt(im["port"])
	if !ok || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("ingress.port must be an integer between 1 and 65535")
	}
	ing := &spec.Ingress{Port: port}
	if p, ok := im["path"].(string); ok {
		ing.Path = p
	}
	return ing, nil
}

// validateGraph checks constraints that span services.
func validateGraph(s *spec.DeploymentSpec) ValidationErrors {
	var errs ValidationErrors

	// depends_on must reference real services.
	for _, name := range s.ServiceNames() {
		for _, dep := range s.Services[name].Depends {
			if _, ok := s.Services[dep]; !ok {
				errs = append(errs, ValidationError{
					Field:   "services." + name + ".depends_on",
					Message: fmt.Sprintf("references unknown service %q", dep),
				})
			}
		}
	}

	// depends_on must be acyclic — the rollout controller starts services
	// in topological order and would otherwise deadlock.
	if cycle := findCycle(s); len(cycle) > 0 {
		errs = append(errs, ValidationError{
			Field:   "depends_on",
			Message: "dependency cycle: " + strings.Join(cycle, " -> "),
		})
	}

	// Exactly zero or one ingress service.
	var ingress []string
	for _, name := range s.ServiceNames() {
		if s.Services[name].Ingress != nil {
			ingress = append(ingress, name)
		}
	}
	if len(ingress) > 1 {
		errs = append(errs, ValidationError{
			Field:   "x-composectl.ingress",
			Message: "multiple services declare ingress: " + strings.Join(ingress, ", "),
		})
	}

	// The ingress-may-not-be-pinned rule lives in convertService now. It used
	// to be checked here because pinning was inferred from a volume and only
	// the assembled spec knew the answer; it is a declared field today, so
	// the error can name x-composectl.rollout on the offending service
	// instead of pointing at the service generically. Checking it in both
	// places would report one mistake twice.

	return errs
}

// findCycle returns a cycle in the depends_on graph, or nil.
func findCycle(s *spec.DeploymentSpec) []string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var path []string
	var out []string

	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		path = append(path, n)
		for _, dep := range s.Services[n].Depends {
			if _, ok := s.Services[dep]; !ok {
				continue
			}
			switch color[dep] {
			case grey:
				for i, p := range path {
					if p == dep {
						out = append(append([]string{}, path[i:]...), dep)
						return true
					}
				}
				out = append(append([]string{}, path...), dep)
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return false
	}

	for _, name := range s.ServiceNames() {
		if color[name] == white && visit(name) {
			return out
		}
	}
	return nil
}

// driveLetterMount reports whether target is compose-go's Windows-drive
// reading of the short syntax `name:/path`. A single-character name is
// indistinguishable from a drive letter, so the loader keeps the whole
// string as the target and drops the source. Returns the name and path the
// author almost certainly meant.
func driveLetterMount(target string) (name, path string, ok bool) {
	if len(target) < 3 || target[1] != ':' {
		return "", "", false
	}
	if target[2] != '/' && target[2] != '\\' {
		return "", "", false
	}
	return target[:1], target[2:], true
}

// misparsedVolumeNames returns the volume names swallowed by that
// drive-letter reading. Their declarations must not additionally be
// reported as unmounted — the mount is there, it just did not survive the
// loader, and that is already reported at the mount itself.
func misparsedVolumeNames(services types.Services) map[string]bool {
	out := map[string]bool{}
	for _, svc := range services {
		for _, v := range svc.Volumes {
			if v.Type == "volume" && v.Source == "" {
				if name, _, ok := driveLetterMount(v.Target); ok {
					out[name] = true
				}
			}
		}
	}
	return out
}

// volumeOwner returns the single service mounting volName, and the count
// of services mounting it.
func volumeOwner(services types.Services, volName string) (string, int) {
	var owner string
	count := 0
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range services[n].Volumes {
			if v.Type == "volume" && v.Source == volName {
				if count == 0 {
					owner = n
				}
				count++
				break
			}
		}
	}
	return owner, count
}

func durSecs(d *types.Duration, def int) int {
	if d == nil {
		return def
	}
	return int(time.Duration(*d).Seconds())
}

func uint64OrDefault(v *uint64, def uint64) uint64 {
	if v == nil {
		return def
	}
	return *v
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
