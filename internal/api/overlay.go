package api

import (
	"maps"

	"github.com/craig/composectl/internal/spec"
)

// applyEnvConfig produces the resolved spec for a deployment by overlaying
// the environment's config onto the stack spec.
//
// The overlay is deliberately shallow and additive: env config can only
// set environment variables, never change images, mounts, or limits.
// Allowing structural overrides would mean the spec digest no longer
// identifies what actually runs, and two environments could diverge in
// ways the stack author never reviewed.
//
// The returned spec is a deep copy — the caller must not mutate the
// stack version's stored spec, which is immutable by contract.
func applyEnvConfig(base *spec.DeploymentSpec, config map[string]string) *spec.DeploymentSpec {
	out := &spec.DeploymentSpec{
		SpecVersion: base.SpecVersion,
		Services:    make(map[string]spec.Service, len(base.Services)),
		Volumes:     maps.Clone(base.Volumes),
		Networks:    append([]string(nil), base.Networks...),
	}

	for name, svc := range base.Services {
		cp := svc
		cp.Command = append([]string(nil), svc.Command...)
		cp.Entrypoint = append([]string(nil), svc.Entrypoint...)
		cp.Ports = append([]spec.Port(nil), svc.Ports...)
		cp.Mounts = append([]spec.Mount(nil), svc.Mounts...)
		cp.Depends = append([]string(nil), svc.Depends...)
		cp.SecretEnv = maps.Clone(svc.SecretEnv)

		// Env config supplies defaults across the stack, but a value set
		// explicitly in the compose file is the author's deliberate choice
		// for that service and wins. Copying config last would silently
		// override per-service settings — e.g. an env-wide LOG_LEVEL
		// clobbering a worker that intentionally runs at debug.
		cp.Env = make(map[string]string, len(svc.Env)+len(config))
		maps.Copy(cp.Env, config)
		maps.Copy(cp.Env, svc.Env)

		// A secret template also takes precedence: env config must never
		// replace a value the stack routes through the secret store.
		for k := range cp.SecretEnv {
			delete(cp.Env, k)
		}

		if svc.Health != nil {
			hc := *svc.Health
			hc.Test = append([]string(nil), svc.Health.Test...)
			cp.Health = &hc
		}
		if svc.Ingress != nil {
			ing := *svc.Ingress
			cp.Ingress = &ing
		}

		out.Services[name] = cp
	}
	return out
}
