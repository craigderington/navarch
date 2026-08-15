package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultURL    = "http://localhost:8417"
	envURL        = "COMPOSECTL_URL"
	envToken      = "COMPOSECTL_TOKEN"
	envTokenFile  = "COMPOSECTL_TOKEN_FILE"
	envAPIToken   = "COMPOSECTL_AGENT_TOKEN" // same token the stack already uses
	envConfigPath = "COMPOSECTL_CONFIG"
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

func loadConfigFile() Config {
	path := os.Getenv(envConfigPath)
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".config", "composectl", "config.yaml")
		}
	}
	if path == "" {
		return Config{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}
	}
	return cfg
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

	if v := firstEnv(envURL); v != "" {
		cfg.URL = v
	}
	if v := firstEnv(envToken, envAPIToken); v != "" {
		cfg.Token = v
	}
	if v := firstEnv(envTokenFile); v != "" {
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
