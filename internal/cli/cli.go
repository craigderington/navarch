// Package cli is the navarch command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/craigderington/navarch/internal/client"
)

// Build metadata, stamped by the linker at release time
// (scripts/release.sh). Vars, not consts, because -X can only set vars — and
// the defaults are deliberately not a version number: a binary someone built
// from a working tree should say so rather than claim to be a release.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Run executes composectl with the given argv (no program name) and returns
// the process exit code. stdout/stderr are injected so tests can capture them.
func Run(args []string, stdout, stderr io.Writer) int {
	flags, rest, err := splitGlobals(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if flags.help && len(rest) == 0 {
		fmt.Fprint(stdout, rootHelp)
		return 0
	}
	if len(rest) == 0 {
		fmt.Fprint(stderr, rootHelp)
		return 2
	}

	cfg, err := resolveConfig(flags.cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	cmd, rest := rest[0], rest[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(stdout, rootHelp)
		return 0
	}
	if cmd == "version" {
		// Commit and build date are here because the first question about a
		// misbehaving deployment is "which build is this", and a bare semver
		// cannot answer it for anything built between tags.
		if flags.cfg.Output == "json" {
			return printExit(printJSON(stdout, map[string]string{
				"version": version, "commit": commit, "built": date, "go": runtime.Version(),
			}), stderr)
		}
		fmt.Fprintf(stdout, "navarch %s\ncommit  %s\nbuilt   %s\ngo      %s\n",
			version, commit, date, runtime.Version())
		return 0
	}

	// login and logout run before the shared client exists: login supplies the
	// token rather than consuming one, and logout must work even when the
	// stored URL is unusable — otherwise the command for getting out of a bad
	// configuration would need that configuration to be good.
	if cmd == "login" || cmd == "logout" {
		e := env{cfg: cfg, out: stdout, err: stderr}
		var err error
		if cmd == "login" {
			err = cmdLogin(context.Background(), e, rest, flags.cfg.Token)
		} else {
			err = cmdLogout(context.Background(), e, rest)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			if isUsage(err) {
				return 2
			}
			return 1
		}
		return 0
	}

	// Before any credential is put on the wire, not after a request has already
	// carried it.
	if err := guardTransport(cfg.URL, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	c, err := client.New(cfg.URL, cfg.Token)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx := context.Background()
	env := env{cfg: cfg, c: c, out: stdout, err: stderr}

	if err := dispatch(ctx, env, cmd, rest); err != nil {
		fmt.Fprintln(stderr, err)
		if isUsage(err) {
			return 2
		}
		return 1
	}
	return 0
}

type env struct {
	cfg Config
	c   *client.Client
	out io.Writer
	err io.Writer
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func isUsage(err error) bool {
	_, ok := err.(usageError)
	return ok
}

func usage(msg string) error { return usageError{msg} }

type globalFlags struct {
	cfg  Config
	help bool
}

func splitGlobals(args []string) (globalFlags, []string, error) {
	var g globalFlags
	var rest []string
	seenCmd := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			return g, rest, nil
		case a == "-h" || a == "--help":
			g.help = true
		case a == "--url" || strings.HasPrefix(a, "--url="):
			v, n, err := takeArg(args, i, "--url")
			if err != nil {
				return g, nil, err
			}
			g.cfg.URL = v
			i += n
		case a == "--token" || strings.HasPrefix(a, "--token="):
			v, n, err := takeArg(args, i, "--token")
			if err != nil {
				return g, nil, err
			}
			g.cfg.Token = v
			i += n
		case a == "--token-file" || strings.HasPrefix(a, "--token-file="):
			v, n, err := takeArg(args, i, "--token-file")
			if err != nil {
				return g, nil, err
			}
			g.cfg.TokenFile = v
			i += n
		case a == "--output" || a == "-o" || strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "-o="):
			name := "--output"
			if strings.HasPrefix(a, "-o") && !strings.HasPrefix(a, "--") {
				name = "-o"
			}
			v, n, err := takeArg(args, i, name)
			if err != nil {
				return g, nil, err
			}
			g.cfg.Output = v
			i += n
		case strings.HasPrefix(a, "-"):
			if !seenCmd {
				return g, nil, fmt.Errorf("unknown flag %s", a)
			}
			rest = append(rest, a)
		default:
			seenCmd = true
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

func takeArg(args []string, i int, name string) (string, int, error) {
	a := args[i]
	if strings.Contains(a, "=") {
		return strings.SplitN(a, "=", 2)[1], 0, nil
	}
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("%s requires a value", name)
	}
	return args[i+1], 1, nil
}

func dispatch(ctx context.Context, e env, cmd string, args []string) error {
	switch cmd {
	case "health":
		return cmdHealth(ctx, e)
	case "validate":
		return cmdValidate(ctx, e, args)
	case "org", "orgs":
		return cmdOrg(ctx, e, args)
	case "app", "apps":
		return cmdApp(ctx, e, args)
	case "stack", "stacks":
		return cmdStack(ctx, e, args)
	case "env", "envs":
		return cmdEnv(ctx, e, args)
	case "preview", "previews":
		return cmdPreview(ctx, e, args)
	case "deploy":
		return cmdDeploy(ctx, e, args)
	case "deployment", "deployments":
		return cmdDeployment(ctx, e, args)
	case "promote":
		return cmdPromote(ctx, e, args)
	case "rollback":
		return cmdRollback(ctx, e, args)
	case "secret", "secrets":
		return cmdSecret(ctx, e, args)
	case "node", "nodes":
		return cmdNode(ctx, e, args)
	case "events":
		return cmdEvents(ctx, e, args)
	case "logs":
		return cmdLogs(ctx, e, args)
	case "wait":
		return cmdWait(ctx, e, args)
	case "whoami":
		return cmdWhoami(ctx, e, args)
	case "token", "tokens":
		return cmdToken(ctx, e, args)
	case "member", "members":
		return cmdMember(ctx, e, args)
	case "tui":
		return cmdTUI(ctx, e, args)
	default:
		return usage(fmt.Sprintf("unknown command %q\n\n%s", cmd, rootHelp))
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printTable(w io.Writer, headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
}

func emit(e env, v any, headers []string, rows [][]string) error {
	if e.cfg.Output == "json" {
		return printJSON(e.out, v)
	}
	printTable(e.out, headers, rows)
	return nil
}

func emitOne(e env, v any, headers []string, row []string) error {
	return emit(e, v, headers, [][]string{row})
}

func need(args []string, n int, usageMsg string) error {
	if len(args) < n {
		return usage(usageMsg)
	}
	return nil
}

func flagMap(args []string) (map[string]string, []string, error) {
	out := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if a == "-" {
			// A bare dash is the conventional "read the value from stdin"
			// marker and can never be a flag name. Without this it parses as a
			// flag called "" and the documented
			// `navarch secret set --env E KEY -` fails with "flag -- requires a
			// value" — pushing the operator back to passing the secret on argv,
			// which is precisely what that form exists to avoid.
			pos = append(pos, a)
			continue
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			kv := strings.SplitN(name, "=", 2)
			out[kv[0]] = kv[1]
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, usage(fmt.Sprintf("flag --%s requires a value", name))
		}
		i++
		out[name] = args[i]
	}
	return out, pos, nil
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func ptrTS(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

const rootHelp = `navarch — command the Navarch control plane

Usage:
  navarch [global flags] <command> [arguments]

Global flags:
  --url URL            Control plane URL (env NAVARCH_URL, default http://localhost:8417)
  --token TOKEN        Operator token (env NAVARCH_TOKEN or NAVARCH_AGENT_TOKEN)
                       Plaintext http:// is refused outside loopback and
                       container networks; NAVARCH_INSECURE=1 overrides.
  --token-file PATH    Read token from a file
  --output, -o FMT     table (default) or json

Commands:
  login                Store an operator token for this machine
  logout               Forget the stored token (it stays valid: see token revoke)
  health               Check control-plane + database
  whoami               Show who this token belongs to and which orgs it sees
  token                List, create, and revoke your own operator tokens
  validate FILE        Parse a compose file without deploying
  org                  Create and list organizations
  member               List, add, and remove an organization's operators
  app                  Create and list applications
  stack                Create, list, get, push, and version stacks
  env                  Create, list, and get environments
  preview              Create an ephemeral preview environment
  deploy               Roll out a stack version to an environment
  deployment           List and get deployments
  promote ID           Manually promote a healthy deployment
  rollback             Re-deploy an earlier revision
  secret               Set, list, and delete environment secrets
  node                 List, get, drain, uncordon, rotate keys, issue join tokens
  events               Organization audit timeline
  logs ENV             Container output for one service (--service, --follow)
  wait ID              Block until a deployment reaches a state
  tui                  Read-only full-screen dashboard for the fleet
  version              Print CLI version

Naming things:
  Anywhere an id is accepted, a slug path rooted at the organization works too.
  Depth follows the hierarchy, and segments may mix ids and slugs.

    --org    dev
    --app    dev/preview
    --stack  dev/preview/main
    --env    dev/preview/main/staging
    node     dev/dev-node-1            (organization + hostname)

Config file (lowest precedence): $NAVARCH_CONFIG or ~/.config/navarch/config.yaml

The previous COMPOSECTL_* variables and ~/.config/composectl/config.yaml are
still read, at lower precedence, so an existing setup keeps working.

Examples:
  navarch login --url https://navarch.example.com   # prompts, never takes a token on argv
  navarch health
  navarch validate examples/hello/compose.yaml
  navarch stack push dev/preview/main examples/hello/compose.yaml
  navarch deploy --env dev/preview/main/staging
  navarch secret set --env dev/preview/main/staging db_password -   # value from stdin (or @file)
  navarch events --org dev --limit 20
  navarch logs dev/shop/main/staging --service api --follow
`

// printExit turns an output error into an exit code, for the handful of
// commands that run before the client (and its error handling) exists.
func printExit(err error, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
