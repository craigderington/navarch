package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/craigderington/navarch/internal/transport"
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

	// The opt-in for sending an operator token over plaintext HTTP to something
	// that is not loopback or a container network. Deliberately not a config
	// file key: a decision this consequential should have to be made in the
	// environment where the command runs, not inherited from a file somebody
	// edited months ago.
	envInsecure       = "NAVARCH_INSECURE"
	envInsecureLegacy = "COMPOSECTL_INSECURE"
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

// credential is the one thing --token and --token-file both express.
//
// It is resolved as a single value: whichever tier last names EITHER of them
// replaces the credential outright. That is not a stylistic preference. Layering
// the two fields independently and then combining them with "the token wins if
// it is set" — which is what this did — lets the LOWEST-precedence source beat
// the highest, because a `token:` in the config file is already non-empty by the
// time a --token-file on the command line is considered. The request then went
// out under a credential the operator did not name, silently, and against the
// wrong install if the stored one pointed somewhere else.
type credential struct{ token, file string }

func (c credential) given() bool { return c.token != "" || c.file != "" }

func resolveConfig(flags Config) (Config, error) {
	file := loadConfigFile()
	cfg := defaultConfig()
	if file.URL != "" {
		cfg.URL = file.URL
	}
	if file.Output != "" {
		cfg.Output = file.Output
	}

	// One credential, resolved across all three tiers in order. Within a single
	// tier the direct value still beats the indirection — `--token` over
	// `--token-file`, `NAVARCH_TOKEN` over `NAVARCH_TOKEN_FILE` — which is the
	// pre-existing behaviour and the reading that surprises nobody.
	//
	// The environment tier is itself two-dimensional and deliberate: the
	// dedicated CLI token outranks the shared stack token, and within each of
	// those the new name outranks its legacy predecessor.
	var cred credential
	for _, c := range []credential{
		{file.Token, file.TokenFile},
		{
			firstEnv(envToken, envTokenLegacy, envAgentToken, envAgentTokenLegacy),
			firstEnv(envTokenFile, envTokenFileLegacy),
		},
		{flags.Token, flags.TokenFile},
	} {
		if c.given() {
			cred = c
		}
	}
	cfg.Token, cfg.TokenFile = cred.token, cred.file

	if v := firstEnv(envURL, envURLLegacy); v != "" {
		cfg.URL = v
	}
	if flags.URL != "" {
		cfg.URL = flags.URL
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

// guardTransport refuses to send an operator token in the clear to somewhere it
// could be read, and says so loudly when the operator has opted in anyway.
//
// The CLI is where this belongs rather than internal/client: the client is the
// only package that knows the wire format, and "is this network trustworthy" is
// not the wire format. Putting it here also covers the TUI, which is handed the
// client this configuration built.
func guardTransport(rawURL string, stderr io.Writer) error {
	err := transport.CheckBaseURL(rawURL)
	if err == nil {
		return nil
	}
	var insecure *transport.InsecureError
	if !errors.As(err, &insecure) {
		// Malformed or unusable. No opt-in rescues this.
		return err
	}
	if !transport.Insecure(firstEnv(envInsecure, envInsecureLegacy)) {
		return fmt.Errorf("%w\n\nSet %s=1 to proceed anyway", err, envInsecure)
	}
	// Warned every single invocation, not once. A warning an operator has
	// learned to expect is still the last thing standing between a token and a
	// network, and a one-shot notice is one they will never see again after the
	// day they set the variable.
	fmt.Fprintf(stderr, "warning: sending credentials in the clear to %s (%s is set)\n",
		insecure.Host, envInsecure)
	return nil
}

// configPath is where `navarch login` writes. Always the new-name path: the
// composectl location is still *read* so an existing setup keeps working, but
// nothing should be creating one in 2026.
func configPath() (string, error) {
	if p := firstEnv(envConfigPath, envConfigPathLegacy); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a home directory to save config in: %w", err)
	}
	return filepath.Join(home, ".config", "navarch", "config.yaml"), nil
}

// saveConfig writes url and token to the config file, preserving whatever else
// is already in it.
//
// 0600 on the file and 0700 on the directory because this is a bearer
// credential at rest. It merges rather than overwrites so that logging in does
// not silently discard an `output: json` somebody set months ago — a config
// file that loses unrelated settings when you touch it is one people stop
// trusting.
func saveConfig(url, token string) (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	cfg := Config{}
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			// Refuse rather than overwrite. The file is probably hand-edited,
			// and replacing something unparseable destroys the only copy of
			// whatever the author meant.
			return "", fmt.Errorf("%s is not valid YAML; fix or remove it before logging in: %w", path, err)
		}
	}
	cfg.URL = url
	cfg.Token = token
	// A file holding both keys expresses two credentials of which only one can
	// take effect, and the one that loses is invisible. Logging in names the
	// token directly, so the indirection goes.
	cfg.TokenFile = ""

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	// Written via a temp file in the same directory and renamed, so an
	// interrupted write cannot leave a half-file where the credential was.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("save config: %w", err)
	}
	return path, nil
}

// clearToken removes the stored credential, leaving the rest of the file alone.
func clearToken() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil // nothing stored; logging out is already true
		}
		return "", err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("%s is not valid YAML: %w", path, err)
	}
	cfg.Token = ""
	cfg.TokenFile = ""
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}
