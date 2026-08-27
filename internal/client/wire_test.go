package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The client is the only package that knows the wire format, and both front
// ends (CLI and TUI) depend on it — a wrong URL or envelope here surfaces only
// through whatever command happens to call it. These tests pin each method's
// method, path and request body against a recording server, so a rename or
// route change fails here rather than in a demo.

type wireRecord struct {
	Method string
	Path   string
	Body   string
}

// wireServer answers every request with `reply` and records the last request.
func wireServer(t *testing.T, reply string) (*wireRecord, *Client) {
	t.Helper()
	rec := &wireRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.Method = r.Method
		// EscapedPath, not Path: Path decodes %2F back into a separator, so a
		// test asserting that a secret key was escaped would see it decoded
		// and never know.
		rec.Path = r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			rec.Path += "?" + r.URL.RawQuery
		}
		rec.Body = string(b)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "t")
	if err != nil {
		t.Fatal(err)
	}
	return rec, c
}

func assertWire(t *testing.T, got *wireRecord, method, path, bodyContains string) {
	t.Helper()
	if got.Method != method || got.Path != path {
		t.Fatalf("wire = %s %s, want %s %s", got.Method, got.Path, method, path)
	}
	if bodyContains != "" && !jsonContains(got.Body, bodyContains) {
		t.Fatalf("body %s should contain %q", got.Body, bodyContains)
	}
}

// jsonContains reports whether the JSON body carries the key:value pair,
// tolerating marshalled key order.
func jsonContains(body, pair string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body == pair
	}
	k, v, ok := splitPair(pair)
	if !ok {
		return false
	}
	got, exists := m[k]
	return exists && got == v
}

func splitPair(pair string) (string, any, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte("{"+pair+"}"), &m); err != nil || len(m) != 1 {
		return "", nil, false
	}
	for k, v := range m {
		return k, v, true
	}
	return "", nil, false
}

func TestCatalogWire(t *testing.T) {
	t.Run("CreateOrg", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"o1","slug":"s","name":"n"}`)
		if _, err := c.CreateOrg(context.Background(), "s", "n"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/orgs", `"slug":"s"`)
	})
	t.Run("CreateApp", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"a1"}`)
		if _, err := c.CreateApp(context.Background(), "org-1", "web", "Web"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/orgs/org-1/apps", `"slug":"web"`)
	})
	t.Run("ListApps", func(t *testing.T) {
		rec, c := wireServer(t, `{"applications":[]}`)
		if _, err := c.ListApps(context.Background(), "org-1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/orgs/org-1/apps", "")
	})
	t.Run("CreateStack", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"s1"}`)
		if _, err := c.CreateStack(context.Background(), "app-1", "main"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/apps/app-1/stacks", `"slug":"main"`)
	})
	t.Run("ListStacks", func(t *testing.T) {
		rec, c := wireServer(t, `{"stacks":[]}`)
		if _, err := c.ListStacks(context.Background(), "app-1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/apps/app-1/stacks", "")
	})
	t.Run("GetStack", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"s1"}`)
		if _, err := c.GetStack(context.Background(), "s1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/stacks/s1", "")
	})
	t.Run("ListStackVersions", func(t *testing.T) {
		rec, c := wireServer(t, `{"versions":[]}`)
		if _, err := c.ListStackVersions(context.Background(), "s1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/stacks/s1/versions", "")
	})
	t.Run("CreateEnv", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"e1"}`)
		if _, err := c.CreateEnv(context.Background(), "s1", CreateEnvInput{Slug: "prod", Hostname: "app.example.com"}); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/stacks/s1/envs", `"hostname":"app.example.com"`)
	})
	t.Run("ListEnvs", func(t *testing.T) {
		rec, c := wireServer(t, `{"environments":[]}`)
		if _, err := c.ListEnvs(context.Background(), "s1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/stacks/s1/envs", "")
	})
	t.Run("GetEnv", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"e1"}`)
		if _, err := c.GetEnv(context.Background(), "e1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/envs/e1", "")
	})
	t.Run("ListOrgEnvironments", func(t *testing.T) {
		rec, c := wireServer(t, `{"environments":[]}`)
		if _, err := c.ListOrgEnvironments(context.Background(), "org-1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/orgs/org-1/environments", "")
	})
}

func TestDeploymentWire(t *testing.T) {
	t.Run("Deploy latest with author", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"d1"}`)
		if _, err := c.Deploy(context.Background(), "e1", "", "craig"); err != nil {
			t.Fatal(err)
		}
		// An omitted version must not send the key at all: the server treats
		// a present-but-empty stack_version_id differently from an absent one.
		assertWire(t, rec, "POST", "/v1/envs/e1/deployments", `"created_by":"craig"`)
		if jsonContains(rec.Body, `"stack_version_id":""`) {
			t.Fatalf("empty version must be omitted from the body, got %s", rec.Body)
		}
	})
	t.Run("Deploy pinned version", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"d1"}`)
		if _, err := c.Deploy(context.Background(), "e1", "v9", ""); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/envs/e1/deployments", `"stack_version_id":"v9"`)
	})
	t.Run("ListDeployments", func(t *testing.T) {
		rec, c := wireServer(t, `{"deployments":[]}`)
		if _, err := c.ListDeployments(context.Background(), "e1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/envs/e1/deployments", "")
	})
	t.Run("GetDeployment", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"d1"}`)
		if _, err := c.GetDeployment(context.Background(), "d1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/deployments/d1", "")
	})
	t.Run("Promote", func(t *testing.T) {
		rec, c := wireServer(t, `{"promoted":"d1"}`)
		if _, err := c.Promote(context.Background(), "d1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/deployments/d1/promote", "")
	})
	t.Run("Rollback", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"d2"}`)
		if _, err := c.Rollback(context.Background(), "e1", 3); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/envs/e1/rollback", `"to_revision":3`)
	})
}

func TestPreviewWire(t *testing.T) {
	rec, c := wireServer(t, `{"id":"p1","hostname":"pr-1.preview.localhost"}`)
	if _, err := c.CreatePreview(context.Background(), "s1", CreatePreviewInput{
		Slug: "pr-1", TTLHours: 24, InheritSecretsFrom: "e1", CreatedBy: "ci",
	}); err != nil {
		t.Fatal(err)
	}
	assertWire(t, rec, "POST", "/v1/stacks/s1/previews", `"ttl_hours":24`)
	if !jsonContains(rec.Body, `"inherit_secrets_from":"e1"`) {
		t.Fatalf("inherit_secrets_from must be sent, got %s", rec.Body)
	}
}

func TestNodeWire(t *testing.T) {
	t.Run("ListNodes", func(t *testing.T) {
		rec, c := wireServer(t, `{"nodes":[]}`)
		if _, err := c.ListNodes(context.Background(), "org 1"); err != nil {
			t.Fatal(err)
		}
		// The org id is a path-unsafe value on purpose: it must be escaped
		// rather than spliced into the query.
		assertWire(t, rec, "GET", "/v1/nodes?org=org+1", "")
	})
	t.Run("GetNode", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"n1"}`)
		if _, err := c.GetNode(context.Background(), "n1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/nodes/n1", "")
	})
	t.Run("DrainNode", func(t *testing.T) {
		rec, c := wireServer(t, `{"released":[],"stranded":[]}`)
		if _, err := c.DrainNode(context.Background(), "n1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/nodes/n1/drain", "")
	})
	t.Run("UncordonNode", func(t *testing.T) {
		rec, c := wireServer(t, `{"status":"ready"}`)
		state, err := c.UncordonNode(context.Background(), "n1")
		if err != nil || state != "ready" {
			t.Fatalf("uncordon = %q, %v", state, err)
		}
		assertWire(t, rec, "POST", "/v1/nodes/n1/uncordon", "")
	})
}

func TestEventsAndSecretsWire(t *testing.T) {
	t.Run("ListEvents pagination", func(t *testing.T) {
		rec, c := wireServer(t, `{"events":[]}`)
		if _, err := c.ListEvents(context.Background(), "org-1", 50, 123); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/orgs/org-1/events?before_id=123&limit=50", "")
	})
	t.Run("ListEvents no params", func(t *testing.T) {
		rec, c := wireServer(t, `{"events":[]}`)
		if _, err := c.ListEvents(context.Background(), "org-1", 0, 0); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/orgs/org-1/events", "")
	})
	t.Run("ListSecrets", func(t *testing.T) {
		rec, c := wireServer(t, `{"secrets":[]}`)
		if _, err := c.ListSecrets(context.Background(), "e1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/envs/e1/secrets", "")
	})
	t.Run("DeleteSecret escapes the key", func(t *testing.T) {
		rec, c := wireServer(t, `{}`)
		if err := c.DeleteSecret(context.Background(), "e1", "odd/key"); err != nil {
			t.Fatal(err)
		}
		// A key with a slash must be escaped, or it never reaches the route.
		assertWire(t, rec, "DELETE", "/v1/envs/e1/secrets/odd%2Fkey", "")
	})
}

func TestLogsWire(t *testing.T) {
	t.Run("OpenLogs", func(t *testing.T) {
		rec, c := wireServer(t, `{"id":"lr1"}`)
		if _, err := c.OpenLogs(context.Background(), "e1", "api", 200, true); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "POST", "/v1/envs/e1/logs", `"service":"api"`)
	})
	t.Run("ReadLogs cursor", func(t *testing.T) {
		rec, c := wireServer(t, `{"chunks":[],"cursor":0}`)
		if _, err := c.ReadLogs(context.Background(), "lr1", 42); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "GET", "/v1/logs/lr1?cursor=42", "")
	})
	t.Run("CloseLogs", func(t *testing.T) {
		rec, c := wireServer(t, `{}`)
		if err := c.CloseLogs(context.Background(), "lr1"); err != nil {
			t.Fatal(err)
		}
		assertWire(t, rec, "DELETE", "/v1/logs/lr1", "")
	})
}

// Validate posts the compose file raw — no JSON envelope — because that is
// the same shape the stack-version push uses.
func TestValidateWire(t *testing.T) {
	rec, c := wireServer(t, `{"valid":true}`)
	if _, err := c.Validate(context.Background(), []byte("services: {}")); err != nil {
		t.Fatal(err)
	}
	assertWire(t, rec, "POST", "/v1/validate", "")
	if rec.Body != "services: {}" {
		t.Fatalf("validate must send the compose bytes verbatim, got %q", rec.Body)
	}
}
