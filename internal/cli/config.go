package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The CLI is named navarch; NAVARCH_* is the name for its environment. Each
// variable keeps its COMPOSECTL_* predecessor as a lower-precedence fallback so
// the rename does not fail closed on a shell profile that was set before it —
// these carry the bearer token, and an unset token is an immediate hard error
// rather than a degraded mode. The legacy half of each pair is removable once
// nothing sets it; nothing else depends on the old names.
const (
	defaultURL = "http://localhost:8417"

	envURL       = "NAVARCH_URL"
	envToken     = "NAVARCH_TOKEN"
	envTokenFile = "NAVARCH_TOKEN_FILE"
	// The shared token the dev stack already exports. It stays *below* the
	// dedicated CLI token in the chain — that ordering is the existing
	// behaviour and is preserved across the rename.
	envAgentToken = "NAVARCH_AGENT_TOKEN"
	envConfigPath = "NAVARCH_CONFIG"

	envURLLegacy        = "COMPOSECTL_URL"
	envTokenLegacy      = "COMPOSECTL_TOKEN"
	envTokenFileLegacy  = "COMPOSECTL_TOKEN_FILE"
	envAgentTokenLegacy = "COMPOSECTL_AGENT_TOKEN"
	envConfigPathLegacy = "COMPOSECTL_CONFIG"
)

// Config is how composectl finds the control plane. Precedence, highest first:
// flag, environment, config file, built-in default.
type Config struct {
	URL       string `yaml:"url"`
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
	Output    string `yaml:"output"`
}

func defaultConfig() Config {
	return Config{URL: defaultURL, Output: "table"}
}

// configPaths lists candidate config files, new name first. The composectl
// path is still read so the rename does not silently drop settings already on
// disk — a config file that stops being consulted looks exactly like one whose
// contents stopped taking effect, and neither reports anything.
func configPaths() []string {
	if p := firstEnv(envConfigPath, envConfigPathLegacy); p != "" {
		return []string{p}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".config", "navarch", "config.yaml"),
		filepath.Join(home, ".config", "composectl", "config.yaml"),
	}
}

func loadConfigFile() Config {
	for _, path := range configPaths() {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg Config
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			// A malformed file is not silently skipped in favour of the next
			// candidate: that would apply the older config while the one the
			// user just edited is the broken one.
			return Config{}
		}
		return cfg
	}
	return Config{}
}

func resolveConfig(flags Config) (Config, error) {
	file := loadConfigFile()
	cfg := defaultConfig()
	if file.URL != "" {
		cfg.URL = file.URL
	}
	if file.Token != "" {
		cfg.Token = file.Token
	}
	if file.TokenFile != "" {
		cfg.TokenFile = file.TokenFile
	}
	if file.Output != "" {
		cfg.Output = file.Output
	}

	if v := firstEnv(envURL, envURLLegacy); v != "" {
		cfg.URL = v
	}
	// Ordering is two-dimensional and deliberate: the dedicated CLI token
	// outranks the shared stack token (pre-existing behaviour), and within each
	// of those tiers the new name outranks the legacy one.
	if v := firstEnv(envToken, envTokenLegacy, envAgentToken, envAgentTokenLegacy); v != "" {
		cfg.Token = v
	}
	if v := firstEnv(envTokenFile, envTokenFileLegacy); v != "" {
		cfg.TokenFile = v
	}

	if flags.URL != "" {
		cfg.URL = flags.URL
	}
	if flags.Token != "" {
		cfg.Token = flags.Token
	}
	if flags.TokenFile != "" {
		cfg.TokenFile = flags.TokenFile
	}
	if flags.Output != "" {
		cfg.Output = flags.Output
	}

	if cfg.Token == "" && cfg.TokenFile != "" {
		b, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return cfg, fmt.Errorf("read token file: %w", err)
		}
		cfg.Token = strings.TrimSpace(string(b))
	}
	switch cfg.Output {
	case "", "table", "json":
		if cfg.Output == "" {
			cfg.Output = "table"
		}
	default:
		return cfg, fmt.Errorf("output must be table or json, not %q", cfg.Output)
	}
	return cfg, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
