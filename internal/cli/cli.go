// Package cli is the composectl command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/craig/composectl/internal/client"
)

const version = "0.1.0"

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
		fmt.Fprintf(stdout, "composectl %s\n", version)
		return 0
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
	case "wait":
		return cmdWait(ctx, e, args)
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

const rootHelp = `composectl — command the Navarch / composectl control plane

Usage:
  composectl [global flags] <command> [arguments]

Global flags:
  --url URL            Control plane URL (env COMPOSECTL_URL, default http://localhost:8417)
  --token TOKEN        Bearer token (env COMPOSECTL_TOKEN or COMPOSECTL_AGENT_TOKEN)
  --token-file PATH    Read token from a file
  --output, -o FMT     table (default) or json

Commands:
  health               Check control-plane + database
  validate FILE        Parse a compose file without deploying
  org                  Create and list organizations
  app                  Create and list applications
  stack                Create, list, get, push, and version stacks
  env                  Create, list, and get environments
  preview              Create an ephemeral preview environment
  deploy               Roll out a stack version to an environment
  deployment           List and get deployments
  promote ID           Manually promote a healthy deployment
  rollback             Re-deploy an earlier revision
  secret               Set, list, and delete environment secrets
  node                 List, get, and drain nodes
	events               Organization audit timeline
  wait ID              Block until a deployment reaches a state
  version              Print CLI version

Config file (lowest precedence): $COMPOSECTL_CONFIG or ~/.config/composectl/config.yaml

Examples:
  composectl health
  composectl validate examples/hello/compose.yaml
  composectl stack push $STACK examples/hello/compose.yaml
  composectl deploy --env $ENV
  composectl events --org $ORG --limit 20
`
