package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// References the user types are either a UUID or a slug path rooted at the
// organization:
//
//	org          dev
//	app          dev/preview
//	stack        dev/preview/main
//	environment  dev/preview/main/staging
//	node         dev/dev-node-1            (organization + hostname)
//
// Segments may be mixed: an id for the parent and a slug for the leaf resolves
// fine, which is what makes a path usable from a script that already captured
// one id.
//
// Whether a reference is an id is decided by *parsing*, never by shape. A UUID
// is itself valid slug syntax — lowercase alphanumeric with internal dashes, 36
// characters, inside the store's 63-character cap — so nothing stops a stack
// being named after one, and "looks like an id" is a guess where "parses as
// one" is a rule.
//
// Only the canonical dashed form counts. uuid.Parse also accepts a bare 32-hex
// string, which is indistinguishable from an ordinary slug; treating that as an
// id would make one unlucky name permanently unaddressable.
func isID(ref string) bool {
	if len(ref) != 36 {
		return false
	}
	_, err := uuid.Parse(ref)
	return err == nil
}

// pathSegments splits a reference and checks its depth against the shape the
// caller expects, so a wrong-depth path fails here with the shape spelled out
// rather than as a 404 from whichever level happened to be consulted last.
func pathSegments(ref, what, shape string) ([]string, error) {
	segs := strings.Split(ref, "/")
	if len(segs) != strings.Count(shape, "/")+1 {
		return nil, usage(fmt.Sprintf("%q is not %s: want %s, or a UUID", ref, what, shape))
	}
	for _, s := range segs {
		if s == "" {
			return nil, usage(fmt.Sprintf("%q is not %s: empty path segment", ref, what))
		}
	}
	return segs, nil
}

// Each resolver returns immediately for an id, so a reference that was already
// a UUID costs no extra request and existing scripts issue exactly the calls
// they always did. Only a slug path pays for the walk — one request per level.

func (e env) resolveOrg(ctx context.Context, ref string) (string, error) {
	if isID(ref) {
		return ref, nil
	}
	segs, err := pathSegments(ref, "an organization", "ORG")
	if err != nil {
		return "", err
	}
	orgs, err := e.c.ListOrgs(ctx)
	if err != nil {
		return "", err
	}
	for _, o := range orgs {
		if o.Slug == segs[0] {
			return o.ID, nil
		}
	}
	return "", fmt.Errorf("no organization with slug %q", segs[0])
}

func (e env) resolveApp(ctx context.Context, ref string) (string, error) {
	if isID(ref) {
		return ref, nil
	}
	segs, err := pathSegments(ref, "an application", "ORG/APP")
	if err != nil {
		return "", err
	}
	orgID, err := e.resolveOrg(ctx, segs[0])
	if err != nil {
		return "", err
	}
	apps, err := e.c.ListApps(ctx, orgID)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if a.Slug == segs[1] {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("no application %q in organization %q", segs[1], segs[0])
}

func (e env) resolveStack(ctx context.Context, ref string) (string, error) {
	if isID(ref) {
		return ref, nil
	}
	segs, err := pathSegments(ref, "a stack", "ORG/APP/STACK")
	if err != nil {
		return "", err
	}
	appID, err := e.resolveApp(ctx, strings.Join(segs[:2], "/"))
	if err != nil {
		return "", err
	}
	stacks, err := e.c.ListStacks(ctx, appID)
	if err != nil {
		return "", err
	}
	for _, s := range stacks {
		if s.Slug == segs[2] {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no stack %q in application %q", segs[2], strings.Join(segs[:2], "/"))
}

func (e env) resolveEnv(ctx context.Context, ref string) (string, error) {
	if isID(ref) {
		return ref, nil
	}
	segs, err := pathSegments(ref, "an environment", "ORG/APP/STACK/ENV")
	if err != nil {
		return "", err
	}
	stackID, err := e.resolveStack(ctx, strings.Join(segs[:3], "/"))
	if err != nil {
		return "", err
	}
	envs, err := e.c.ListEnvs(ctx, stackID)
	if err != nil {
		return "", err
	}
	for _, ev := range envs {
		if ev.Slug == segs[3] {
			return ev.ID, nil
		}
	}
	return "", fmt.Errorf("no environment %q in stack %q", segs[3], strings.Join(segs[:3], "/"))
}

// Nodes are addressed by hostname rather than a slug, and hostname carries no
// uniqueness constraint in the schema — two nodes may legitimately share one.
// An ambiguous reference is therefore an error naming the candidates, never a
// silent pick: draining the wrong node is not a mistake worth guessing into.
func (e env) resolveNode(ctx context.Context, ref string) (string, error) {
	if isID(ref) {
		return ref, nil
	}
	segs, err := pathSegments(ref, "a node", "ORG/HOSTNAME")
	if err != nil {
		return "", err
	}
	orgID, err := e.resolveOrg(ctx, segs[0])
	if err != nil {
		return "", err
	}
	nodes, err := e.c.ListNodes(ctx, orgID)
	if err != nil {
		return "", err
	}
	var matched []string
	for _, n := range nodes {
		if n.Hostname == segs[1] {
			matched = append(matched, n.ID)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", fmt.Errorf("no node %q in organization %q", segs[1], segs[0])
	default:
		return "", fmt.Errorf("hostname %q matches %d nodes in organization %q (%s) — use an id",
			segs[1], len(matched), segs[0], strings.Join(matched, ", "))
	}
}
