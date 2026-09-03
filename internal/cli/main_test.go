package cli

import (
	"os"
	"testing"
)

// Every test in this package runs with a home directory and an environment of
// its own, whether it asks for one or not.
//
// Configuration here resolves from the real `$HOME/.config/navarch/config.yaml`
// and the real NAVARCH_*/COMPOSECTL_* variables, so without this a test asserting
// on "the token that reached the server" is asserting on whatever the developer
// happens to be logged into. That is not a hypothetical: TestTokenFileAndAPIErrorExit
// failed on exactly this, and its failure message printed a live operator token
// into a terminal. CI stayed green throughout, because CI has no config file —
// which is the worst version of this, since the machine that would notice is the
// one where nobody is looking.
//
// TestEnvPrecedenceAcrossTheRename already did this for itself. Doing it once
// for the package means the next test does not have to know to.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "navarch-cli-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	os.Setenv("HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir)
	for _, k := range []string{
		envURL, envToken, envTokenFile, envAgentToken, envConfigPath,
		envURLLegacy, envTokenLegacy, envTokenFileLegacy, envAgentTokenLegacy, envConfigPathLegacy,
		envInsecure, envInsecureLegacy,
	} {
		os.Unsetenv(k)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
