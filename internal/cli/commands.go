package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/craig/composectl/internal/client"
)

func cmdHealth(ctx context.Context, e env) error {
	h, err := e.c.Health(ctx)
	if err != nil {
		return err
	}
	return emitOne(e, h, []string{"STATUS"}, []string{h.Status})
}

func cmdValidate(ctx context.Context, e env, args []string) error {
	if err := need(args, 1, "usage: navarch validate FILE"); err != nil {
		return err
	}
	raw, err := readFileOrStdin(args[0])
	if err != nil {
		return err
	}
	res, err := e.c.Validate(ctx, raw)
	if err != nil {
		return err
	}
	if e.cfg.Output == "json" {
		return printJSON(e.out, res)
	}
	fmt.Fprintf(e.out, "valid\t%v\n", res.Valid)
	fmt.Fprintf(e.out, "digest\t%s\n", res.Digest)
	fmt.Fprintf(e.out, "services\t%s\n", join(res.Summary.Services))
	fmt.Fprintf(e.out, "swappable\t%s\n", join(res.Summary.Swappable))
	fmt.Fprintf(e.out, "pinned\t%s\n", join(res.Summary.Pinned))
	if res.Summary.Ingress != "" {
		fmt.Fprintf(e.out, "ingress\t%s\n", res.Summary.Ingress)
	}
	fmt.Fprintf(e.out, "peak_memory_bytes\t%d\n", res.Summary.PeakMemoryBytes)
	return nil
}

func cmdOrg(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch org list|create ...")
	}
	switch args[0] {
	case "list":
		orgs, err := e.c.ListOrgs(ctx)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(orgs))
		for _, o := range orgs {
			rows = append(rows, []string{o.ID, o.Slug, o.Name, ts(o.CreatedAt)})
		}
		return emit(e, orgs, []string{"ID", "SLUG", "NAME", "CREATED"}, rows)
	case "create":
		if err := need(args, 2, "usage: navarch org create SLUG [--name NAME]"); err != nil {
			return err
		}
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if len(pos) < 1 {
			return usage("usage: navarch org create SLUG [--name NAME]")
		}
		name := flags["name"]
		if name == "" {
			name = pos[0]
		}
		o, err := e.c.CreateOrg(ctx, pos[0], name)
		if err != nil {
			return err
		}
		return emitOne(e, o, []string{"ID", "SLUG", "NAME"}, []string{o.ID, o.Slug, o.Name})
	default:
		return usage("usage: navarch org list|create ...")
	}
}

func cmdApp(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch app list|create --org ID ...")
	}
	switch args[0] {
	case "list":
		flags, _, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["org"] == "" {
			return usage("usage: navarch app list --org ORG")
		}
		org, err := e.resolveOrg(ctx, flags["org"])
		if err != nil {
			return err
		}
		apps, err := e.c.ListApps(ctx, org)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(apps))
		for _, a := range apps {
			rows = append(rows, []string{a.ID, a.Slug, a.Name, a.OrgID})
		}
		return emit(e, apps, []string{"ID", "SLUG", "NAME", "ORG"}, rows)
	case "create":
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if len(pos) < 1 || flags["org"] == "" {
			return usage("usage: navarch app create SLUG --org ORG [--name NAME]")
		}
		name := flags["name"]
		if name == "" {
			name = pos[0]
		}
		org, err := e.resolveOrg(ctx, flags["org"])
		if err != nil {
			return err
		}
		a, err := e.c.CreateApp(ctx, org, pos[0], name)
		if err != nil {
			return err
		}
		return emitOne(e, a, []string{"ID", "SLUG", "NAME", "ORG"}, []string{a.ID, a.Slug, a.Name, a.OrgID})
	default:
		return usage("usage: navarch app list|create --org ID ...")
	}
}

func cmdStack(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch stack list|create|get|push|versions ...")
	}
	switch args[0] {
	case "list":
		flags, _, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["app"] == "" {
			return usage("usage: navarch stack list --app ORG/APP")
		}
		app, err := e.resolveApp(ctx, flags["app"])
		if err != nil {
			return err
		}
		stacks, err := e.c.ListStacks(ctx, app)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(stacks))
		for _, s := range stacks {
			rows = append(rows, []string{s.ID, s.Slug, s.AppID})
		}
		return emit(e, stacks, []string{"ID", "SLUG", "APP"}, rows)
	case "create":
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if len(pos) < 1 || flags["app"] == "" {
			return usage("usage: navarch stack create SLUG --app ORG/APP")
		}
		app, err := e.resolveApp(ctx, flags["app"])
		if err != nil {
			return err
		}
		s, err := e.c.CreateStack(ctx, app, pos[0])
		if err != nil {
			return err
		}
		return emitOne(e, s, []string{"ID", "SLUG", "APP"}, []string{s.ID, s.Slug, s.AppID})
	case "get":
		if err := need(args, 2, "usage: navarch stack get ORG/APP/STACK"); err != nil {
			return err
		}
		stack, err := e.resolveStack(ctx, args[1])
		if err != nil {
			return err
		}
		s, err := e.c.GetStack(ctx, stack)
		if err != nil {
			return err
		}
		return emitOne(e, s, []string{"ID", "SLUG", "APP"}, []string{s.ID, s.Slug, s.AppID})
	case "push":
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return usage("usage: navarch stack push ORG/APP/STACK FILE [--created-by NAME]")
		}
		raw, err := readFileOrStdin(pos[1])
		if err != nil {
			return err
		}
		stack, err := e.resolveStack(ctx, pos[0])
		if err != nil {
			return err
		}
		sv, err := e.c.PushStack(ctx, stack, raw, flags["created-by"])
		if err != nil {
			return err
		}
		return emitOne(e, sv, []string{"ID", "VERSION", "DIGEST"}, []string{sv.ID, strconv.Itoa(sv.Version), sv.SpecDigest})
	case "versions":
		if err := need(args, 2, "usage: navarch stack versions ORG/APP/STACK"); err != nil {
			return err
		}
		stack, err := e.resolveStack(ctx, args[1])
		if err != nil {
			return err
		}
		vs, err := e.c.ListStackVersions(ctx, stack)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(vs))
		for _, v := range vs {
			rows = append(rows, []string{v.ID, strconv.Itoa(v.Version), v.SpecDigest, v.CreatedBy, ts(v.CreatedAt)})
		}
		return emit(e, vs, []string{"ID", "VERSION", "DIGEST", "CREATED_BY", "CREATED"}, rows)
	default:
		return usage("usage: navarch stack list|create|get|push|versions ...")
	}
}

func cmdEnv(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch env list|create|get ...")
	}
	switch args[0] {
	case "list":
		flags, _, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["stack"] == "" {
			return usage("usage: navarch env list --stack ORG/APP/STACK")
		}
		stack, err := e.resolveStack(ctx, flags["stack"])
		if err != nil {
			return err
		}
		envs, err := e.c.ListEnvs(ctx, stack)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(envs))
		for _, ev := range envs {
			live := ""
			if ev.LiveDeploymentID != nil {
				live = *ev.LiveDeploymentID
			}
			rows = append(rows, []string{ev.ID, ev.Slug, ev.Hostname, ev.Strategy, live})
		}
		return emit(e, envs, []string{"ID", "SLUG", "HOSTNAME", "STRATEGY", "LIVE"}, rows)
	case "create":
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if len(pos) < 1 || flags["stack"] == "" {
			return usage("usage: navarch env create SLUG --stack ORG/APP/STACK [--hostname HOST] [--config k=v]")
		}
		in := client.CreateEnvInput{Slug: pos[0], Hostname: flags["hostname"], Strategy: flags["strategy"]}
		if flags["config"] != "" {
			in.Config = parseKV(flags["config"])
		}
		stack, err := e.resolveStack(ctx, flags["stack"])
		if err != nil {
			return err
		}
		ev, err := e.c.CreateEnv(ctx, stack, in)
		if err != nil {
			return err
		}
		return emitOne(e, ev, []string{"ID", "SLUG", "HOSTNAME"}, []string{ev.ID, ev.Slug, ev.Hostname})
	case "get":
		if err := need(args, 2, "usage: navarch env get ORG/APP/STACK/ENV"); err != nil {
			return err
		}
		envID, err := e.resolveEnv(ctx, args[1])
		if err != nil {
			return err
		}
		ev, err := e.c.GetEnv(ctx, envID)
		if err != nil {
			return err
		}
		live := ""
		if ev.LiveDeploymentID != nil {
			live = *ev.LiveDeploymentID
		}
		return emitOne(e, ev, []string{"ID", "SLUG", "HOSTNAME", "LIVE"}, []string{ev.ID, ev.Slug, ev.Hostname, live})
	default:
		return usage("usage: navarch env list|create|get ...")
	}
}

func cmdPreview(ctx context.Context, e env, args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return usage("usage: navarch preview create --stack ID --slug SLUG [--inherit ENV_SLUG] [--ttl HOURS] [--version ID]")
	}
	flags, _, err := flagMap(args[1:])
	if err != nil {
		return err
	}
	if flags["stack"] == "" || flags["slug"] == "" {
		return usage("usage: navarch preview create --stack ID --slug SLUG [--inherit ENV_SLUG] [--ttl HOURS] [--version ID]")
	}
	in := client.CreatePreviewInput{
		Slug:               flags["slug"],
		StackVersionID:     flags["version"],
		InheritSecretsFrom: flags["inherit"],
		CreatedBy:          flags["created-by"],
	}
	if flags["ttl"] != "" {
		n, err := strconv.Atoi(flags["ttl"])
		if err != nil {
			return usage("--ttl must be an integer number of hours")
		}
		in.TTLHours = n
	}
	stack, err := e.resolveStack(ctx, flags["stack"])
	if err != nil {
		return err
	}
	p, err := e.c.CreatePreview(ctx, stack, in)
	if err != nil {
		return err
	}
	return emitOne(e, p, []string{"ENV", "HOSTNAME", "DEPLOYMENT", "EXPIRES"},
		[]string{p.EnvironmentID, p.Hostname, p.DeploymentID, ptrTS(p.ExpiresAt)})
}

func cmdDeploy(ctx context.Context, e env, args []string) error {
	flags, _, err := flagMap(args)
	if err != nil {
		return err
	}
	if flags["env"] == "" {
		return usage("usage: navarch deploy --env ORG/APP/STACK/ENV [--version ID] [--created-by NAME]")
	}
	envID, err := e.resolveEnv(ctx, flags["env"])
	if err != nil {
		return err
	}
	d, err := e.c.Deploy(ctx, envID, flags["version"], flags["created-by"])
	if err != nil {
		return err
	}
	return emitOne(e, d, []string{"ID", "REV", "SLOT", "STATE"},
		[]string{d.ID, strconv.Itoa(d.Revision), d.Slot, d.State})
}

func cmdDeployment(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch deployment list|get ...")
	}
	switch args[0] {
	case "list":
		flags, _, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["env"] == "" {
			return usage("usage: navarch deployment list --env ORG/APP/STACK/ENV")
		}
		envID, err := e.resolveEnv(ctx, flags["env"])
		if err != nil {
			return err
		}
		ds, err := e.c.ListDeployments(ctx, envID)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(ds))
		for _, d := range ds {
			rows = append(rows, []string{d.ID, strconv.Itoa(d.Revision), d.Slot, d.State, d.FailureReason})
		}
		return emit(e, ds, []string{"ID", "REV", "SLOT", "STATE", "FAILURE"}, rows)
	case "get":
		if err := need(args, 2, "usage: navarch deployment get ID"); err != nil {
			return err
		}
		d, err := e.c.GetDeployment(ctx, args[1])
		if err != nil {
			return err
		}
		return emitOne(e, d, []string{"ID", "REV", "SLOT", "STATE", "PROJECT"},
			[]string{d.ID, strconv.Itoa(d.Revision), d.Slot, d.State, d.ProjectName})
	default:
		return usage("usage: navarch deployment list|get ...")
	}
}

func cmdPromote(ctx context.Context, e env, args []string) error {
	if err := need(args, 1, "usage: navarch promote DEPLOYMENT_ID"); err != nil {
		return err
	}
	out, err := e.c.Promote(ctx, args[0])
	if err != nil {
		return err
	}
	if e.cfg.Output == "json" {
		return printJSON(e.out, out)
	}
	fmt.Fprintf(e.out, "promoted\t%v\n", out["promoted"])
	if v, ok := out["superseded"]; ok {
		fmt.Fprintf(e.out, "superseded\t%v\n", v)
	}
	return nil
}

func cmdRollback(ctx context.Context, e env, args []string) error {
	flags, _, err := flagMap(args)
	if err != nil {
		return err
	}
	if flags["env"] == "" {
		return usage("usage: navarch rollback --env ORG/APP/STACK/ENV [--to REVISION]")
	}
	to := 0
	if flags["to"] != "" {
		to, err = strconv.Atoi(flags["to"])
		if err != nil {
			return usage("--to must be a revision number")
		}
	}
	envID, err := e.resolveEnv(ctx, flags["env"])
	if err != nil {
		return err
	}
	d, err := e.c.Rollback(ctx, envID, to)
	if err != nil {
		return err
	}
	return emitOne(e, d, []string{"ID", "REV", "SLOT", "STATE"},
		[]string{d.ID, strconv.Itoa(d.Revision), d.Slot, d.State})
}

func cmdSecret(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch secret list|set|delete ...")
	}
	switch args[0] {
	case "list":
		flags, _, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["env"] == "" {
			return usage("usage: navarch secret list --env ORG/APP/STACK/ENV")
		}
		envID, err := e.resolveEnv(ctx, flags["env"])
		if err != nil {
			return err
		}
		secs, err := e.c.ListSecrets(ctx, envID)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(secs))
		for _, s := range secs {
			rows = append(rows, []string{s.Key, strconv.Itoa(s.Version), ts(s.CreatedAt)})
		}
		return emit(e, secs, []string{"KEY", "VERSION", "CREATED"}, rows)
	case "set":
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["env"] == "" || len(pos) < 2 {
			return usage("usage: navarch secret set --env ORG/APP/STACK/ENV KEY VALUE")
		}
		envID, err := e.resolveEnv(ctx, flags["env"])
		if err != nil {
			return err
		}
		if err := e.c.SetSecret(ctx, envID, pos[0], pos[1]); err != nil {
			return err
		}
		if e.cfg.Output == "json" {
			return printJSON(e.out, map[string]string{"key": pos[0]})
		}
		fmt.Fprintf(e.out, "set\t%s\n", pos[0])
		return nil
	case "delete":
		flags, pos, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["env"] == "" || len(pos) < 1 {
			return usage("usage: navarch secret delete --env ORG/APP/STACK/ENV KEY")
		}
		envID, err := e.resolveEnv(ctx, flags["env"])
		if err != nil {
			return err
		}
		if err := e.c.DeleteSecret(ctx, envID, pos[0]); err != nil {
			return err
		}
		if e.cfg.Output == "json" {
			return printJSON(e.out, map[string]string{"deleted": pos[0]})
		}
		fmt.Fprintf(e.out, "deleted\t%s\n", pos[0])
		return nil
	default:
		return usage("usage: navarch secret list|set|delete ...")
	}
}

func cmdNode(ctx context.Context, e env, args []string) error {
	if len(args) == 0 {
		return usage("usage: navarch node list|get|drain|uncordon ...")
	}
	switch args[0] {
	case "list":
		flags, _, err := flagMap(args[1:])
		if err != nil {
			return err
		}
		if flags["org"] == "" {
			return usage("usage: navarch node list --org ORG")
		}
		org, err := e.resolveOrg(ctx, flags["org"])
		if err != nil {
			return err
		}
		nodes, err := e.c.ListNodes(ctx, org)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(nodes))
		for _, n := range nodes {
			rows = append(rows, []string{n.ID, n.Hostname, n.State, n.AdvertiseAddr,
				strconv.Itoa(n.CPUMillis), formatLabels(n.Labels)})
		}
		return emit(e, nodes, []string{"ID", "HOSTNAME", "STATE", "ADDR", "CPU_MILLIS", "LABELS"}, rows)
	case "get":
		if err := need(args, 2, "usage: navarch node get ORG/HOSTNAME"); err != nil {
			return err
		}
		nodeID, err := e.resolveNode(ctx, args[1])
		if err != nil {
			return err
		}
		n, err := e.c.GetNode(ctx, nodeID)
		if err != nil {
			return err
		}
		return emitOne(e, n, []string{"ID", "HOSTNAME", "STATE", "ADDR", "LABELS"},
			[]string{n.ID, n.Hostname, n.State, n.AdvertiseAddr, formatLabels(n.Labels)})
	case "drain":
		if err := need(args, 2, "usage: navarch node drain ORG/HOSTNAME"); err != nil {
			return err
		}
		nodeID, err := e.resolveNode(ctx, args[1])
		if err != nil {
			return err
		}
		if err := e.c.DrainNode(ctx, nodeID); err != nil {
			return err
		}
		// Report the resolved id, not what was typed: after a rename the two
		// differ, and the id is what identifies the node that was drained.
		if e.cfg.Output == "json" {
			return printJSON(e.out, map[string]string{"status": "draining", "id": nodeID})
		}
		fmt.Fprintf(e.out, "draining\t%s\n", nodeID)
		return nil
	case "uncordon":
		if err := need(args, 2, "usage: navarch node uncordon ORG/HOSTNAME"); err != nil {
			return err
		}
		nodeID, err := e.resolveNode(ctx, args[1])
		if err != nil {
			return err
		}
		// The reported state is whatever the control plane derived from the
		// node's last heartbeat, not a fixed "ready" — an uncordoned node that
		// has been silent comes back `unreachable`, and printing "ready" would
		// contradict the next `node list`.
		state, err := e.c.UncordonNode(ctx, nodeID)
		if err != nil {
			return err
		}
		if e.cfg.Output == "json" {
			return printJSON(e.out, map[string]string{"status": state, "id": nodeID})
		}
		fmt.Fprintf(e.out, "%s\t%s\n", state, nodeID)
		return nil
	default:
		return usage("usage: navarch node list|get|drain|uncordon ...")
	}
}

func cmdWait(ctx context.Context, e env, args []string) error {
	flags, pos, err := flagMap(args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usage("usage: navarch wait DEPLOYMENT_ID [--state live] [--timeout 180]")
	}
	want := flags["state"]
	if want == "" {
		want = "live"
	}
	timeoutSec := 180
	if flags["timeout"] != "" {
		timeoutSec, err = strconv.Atoi(flags["timeout"])
		if err != nil || timeoutSec <= 0 {
			return usage("--timeout must be a positive number of seconds")
		}
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for {
		d, err := e.c.GetDeployment(ctx, pos[0])
		if err != nil {
			return err
		}
		if d.State == want {
			return emitOne(e, d, []string{"ID", "REV", "STATE"},
				[]string{d.ID, strconv.Itoa(d.Revision), d.State})
		}
		if d.State == "failed" && want != "failed" {
			return fmt.Errorf("deployment %s failed: %s", d.ID, d.FailureReason)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s (stuck at %s)", want, d.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func cmdEvents(ctx context.Context, e env, args []string) error {
	flags, _, err := flagMap(args)
	if err != nil {
		return err
	}
	if flags["org"] == "" {
		return usage("usage: navarch events --org ORG [--limit N] [--before ID]")
	}
	limit := 0
	if flags["limit"] != "" {
		limit, err = strconv.Atoi(flags["limit"])
		if err != nil {
			return usage("--limit must be an integer")
		}
	}
	var before int64
	if flags["before"] != "" {
		before, err = strconv.ParseInt(flags["before"], 10, 64)
		if err != nil {
			return usage("--before must be an event id")
		}
	}
	org, err := e.resolveOrg(ctx, flags["org"])
	if err != nil {
		return err
	}
	evs, err := e.c.ListEvents(ctx, org, limit, before)
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(evs))
	for _, ev := range evs {
		rows = append(rows, []string{strconv.FormatInt(ev.ID, 10), ev.Kind, ev.Message, ts(ev.CreatedAt)})
	}
	return emit(e, evs, []string{"ID", "KIND", "MESSAGE", "CREATED"}, rows)
}

func join(ss []string) string {
	if len(ss) == 0 {
		return "-"
	}
	out := ss[0]
	for i := 1; i < len(ss); i++ {
		out += "," + ss[i]
	}
	return out
}

func parseKV(s string) map[string]string {
	m := map[string]string{}
	for _, part := range splitComma(s) {
		k, v, ok := splitOnce(part, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func splitOnce(s, sep string) (string, string, bool) {
	i := 0
	for i < len(s)-len(sep)+1 {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
		i++
	}
	return s, "", false
}

// formatLabels renders node labels for a table cell, sorted so the same node
// always prints the same way — map iteration order would otherwise make the
// output of two identical `node list` calls differ.
func formatLabels(l map[string]string) string {
	if len(l) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

// cmdLogs prints a service's container output.
//
// The latency is stated in the header rather than hidden. Output reaches here by
// the agent's poll, so a follow runs a tick or two behind — and a user who
// believes they are watching live will read a lull as a silent container and go
// hunting for a fault that is not there.
func cmdLogs(ctx context.Context, e env, args []string) error {
	// flagMap treats every flag as taking a value, so a bare --follow would
	// swallow the next argument — or fail outright when it is last, which is
	// where anyone would naturally put it. Lift the boolean out first.
	args, follow := takeBoolFlag(args, "follow", "f")
	flags, pos, err := flagMap(args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usage("usage: navarch logs ORG/APP/STACK/ENV --service NAME [--tail N] [--follow]")
	}
	service := flags["service"]
	if service == "" {
		return usage("--service is required: logs are per service, not per stack")
	}
	tail := 0
	if flags["tail"] != "" {
		if tail, err = strconv.Atoi(flags["tail"]); err != nil || tail <= 0 {
			return usage("--tail must be a positive number of lines")
		}
	}
	envID, err := e.resolveEnv(ctx, pos[0])
	if err != nil {
		return err
	}

	req, err := e.c.OpenLogs(ctx, envID, service, tail, follow)
	if err != nil {
		return err
	}
	// Closing matters more than it looks: a followed request left pending keeps
	// its node reading Docker every tick until the TTL expires it.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.c.CloseLogs(closeCtx, req.ID)
	}()

	if follow {
		fmt.Fprintf(e.err, "# following %s (delivered via the node's poll, so roughly a tick behind — not live)\n", service)
	}

	var cursor int64
	deadline := time.Now().Add(30 * time.Second) // for a non-following read: how long to wait for the first delivery
	for {
		page, err := e.c.ReadLogs(ctx, req.ID, cursor)
		if err != nil {
			return err
		}
		cursor = page.Cursor
		if page.Dropped {
			fmt.Fprintln(e.err, "# output dropped: the container produced more than the control plane buffers")
		}
		for _, c := range page.Chunks {
			fmt.Fprint(e.out, c.Data)
		}
		if page.Request != nil {
			if page.Request.State == "failed" {
				return fmt.Errorf("log read failed on the node: %s", page.Request.LastError)
			}
			// A non-following request is finished once the node has answered it.
			if !follow && page.Request.State == "done" {
				return nil
			}
		}
		if !follow && len(page.Chunks) == 0 && time.Now().After(deadline) {
			return fmt.Errorf("no output delivered within 30s — is the node reconciling?")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// takeBoolFlag removes a valueless flag from args and reports whether it was
// present, under any of the given names. It exists because flagMap consumes the
// following argument for every flag it sees, which is right for --tail 50 and
// wrong for --follow.
func takeBoolFlag(args []string, names ...string) ([]string, bool) {
	want := map[string]bool{}
	for _, n := range names {
		want["--"+n] = true
		want["-"+n] = true
	}
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if want[a] {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}
