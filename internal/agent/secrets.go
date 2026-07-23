package agent

// EnvSecrets is the Sprint 2 dev secret source: a static key→value map, loaded
// from the agent's own environment. It satisfies dockerd.SecretSource. Sprint 3
// replaces it with the encrypted per-environment store; nothing above this type
// changes when it does.
type EnvSecrets map[string]string

func (e EnvSecrets) Get(key string) (string, bool) { v, ok := e[key]; return v, ok }
