package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultTimeout = 15 * time.Second

// Client talks to a composectl control plane.
type Client struct {
	base  string
	token string
	http  *http.Client
}

type Option func(*Client)

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

func New(baseURL, token string, opts ...Option) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("control plane URL is required")
	}
	if _, err := url.ParseRequestURI(base); err != nil {
		return nil, fmt.Errorf("invalid control plane URL %q: %w", baseURL, err)
	}
	c := &Client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

type APIError struct {
	Status  int
	Message string
	Details any
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	if err := c.do(ctx, http.MethodGet, "/healthz", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (c *Client) Validate(ctx context.Context, compose []byte) (*ValidateResult, error) {
	var out ValidateResult
	if err := c.doRaw(ctx, http.MethodPost, "/v1/validate", compose, "application/x-yaml", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateOrg(ctx context.Context, slug, name string) (*Organization, error) {
	var out Organization
	if err := c.doJSON(ctx, http.MethodPost, "/v1/orgs", map[string]string{"slug": slug, "name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListOrgs(ctx context.Context) ([]Organization, error) {
	var out struct {
		Organizations []Organization `json:"organizations"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/orgs", nil, &out); err != nil {
		return nil, err
	}
	return out.Organizations, nil
}

func (c *Client) CreateApp(ctx context.Context, orgID, slug, name string) (*Application, error) {
	var out Application
	if err := c.doJSON(ctx, http.MethodPost, "/v1/orgs/"+orgID+"/apps", map[string]string{"slug": slug, "name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListApps(ctx context.Context, orgID string) ([]Application, error) {
	var out struct {
		Apps []Application `json:"applications"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/orgs/"+orgID+"/apps", nil, &out); err != nil {
		return nil, err
	}
	return out.Apps, nil
}

func (c *Client) CreateStack(ctx context.Context, appID, slug string) (*Stack, error) {
	var out Stack
	if err := c.doJSON(ctx, http.MethodPost, "/v1/apps/"+appID+"/stacks", map[string]string{"slug": slug}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListStacks(ctx context.Context, appID string) ([]Stack, error) {
	var out struct {
		Stacks []Stack `json:"stacks"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/apps/"+appID+"/stacks", nil, &out); err != nil {
		return nil, err
	}
	return out.Stacks, nil
}

func (c *Client) GetStack(ctx context.Context, id string) (*Stack, error) {
	var out Stack
	if err := c.do(ctx, http.MethodGet, "/v1/stacks/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) PushStack(ctx context.Context, stackID string, compose []byte, createdBy string) (*StackVersion, error) {
	path := "/v1/stacks/" + stackID + "/versions"
	if createdBy != "" {
		path += "?created_by=" + url.QueryEscape(createdBy)
	}
	var out StackVersion
	if err := c.doRaw(ctx, http.MethodPost, path, compose, "application/x-yaml", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListStackVersions(ctx context.Context, stackID string) ([]StackVersion, error) {
	var out struct {
		Versions []StackVersion `json:"versions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/stacks/"+stackID+"/versions", nil, &out); err != nil {
		return nil, err
	}
	return out.Versions, nil
}

type CreateEnvInput struct {
	Slug     string
	Hostname string
	Strategy string
	Config   map[string]string
}

func (c *Client) CreateEnv(ctx context.Context, stackID string, in CreateEnvInput) (*Environment, error) {
	body := map[string]any{"slug": in.Slug}
	if in.Hostname != "" {
		body["hostname"] = in.Hostname
	}
	if in.Strategy != "" {
		body["strategy"] = in.Strategy
	}
	if len(in.Config) > 0 {
		body["config"] = in.Config
	}
	var out Environment
	if err := c.doJSON(ctx, http.MethodPost, "/v1/stacks/"+stackID+"/envs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListEnvs(ctx context.Context, stackID string) ([]Environment, error) {
	var out struct {
		Envs []Environment `json:"environments"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/stacks/"+stackID+"/envs", nil, &out); err != nil {
		return nil, err
	}
	return out.Envs, nil
}

// ListOrgEnvironments returns every environment in an organization in one
// request. Prefer it to walking apps → stacks → environments: that walk costs a
// request per app and per stack, which grows with the catalog rather than with
// what the caller is actually showing.
func (c *Client) ListOrgEnvironments(ctx context.Context, orgID string) ([]OrgEnvironment, error) {
	var out struct {
		Envs []OrgEnvironment `json:"environments"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/orgs/"+orgID+"/environments", nil, &out); err != nil {
		return nil, err
	}
	return out.Envs, nil
}

func (c *Client) GetEnv(ctx context.Context, id string) (*Environment, error) {
	var out Environment
	if err := c.do(ctx, http.MethodGet, "/v1/envs/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type CreatePreviewInput struct {
	Slug               string
	StackVersionID     string
	InheritSecretsFrom string
	TTLHours           int
	CreatedBy          string
}

func (c *Client) CreatePreview(ctx context.Context, stackID string, in CreatePreviewInput) (*Preview, error) {
	body := map[string]any{"slug": in.Slug}
	if in.StackVersionID != "" {
		body["stack_version_id"] = in.StackVersionID
	}
	if in.InheritSecretsFrom != "" {
		body["inherit_secrets_from"] = in.InheritSecretsFrom
	}
	if in.TTLHours != 0 {
		body["ttl_hours"] = in.TTLHours
	}
	if in.CreatedBy != "" {
		body["created_by"] = in.CreatedBy
	}
	var out Preview
	if err := c.doJSON(ctx, http.MethodPost, "/v1/stacks/"+stackID+"/previews", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Deploy(ctx context.Context, envID, versionID, createdBy string) (*Deployment, error) {
	body := map[string]string{}
	if versionID != "" {
		body["stack_version_id"] = versionID
	}
	if createdBy != "" {
		body["created_by"] = createdBy
	}
	var out Deployment
	if err := c.doJSON(ctx, http.MethodPost, "/v1/envs/"+envID+"/deployments", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListDeployments(ctx context.Context, envID string) ([]Deployment, error) {
	var out struct {
		Deployments []Deployment `json:"deployments"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/envs/"+envID+"/deployments", nil, &out); err != nil {
		return nil, err
	}
	return out.Deployments, nil
}

func (c *Client) GetDeployment(ctx context.Context, id string) (*Deployment, error) {
	var out Deployment
	if err := c.do(ctx, http.MethodGet, "/v1/deployments/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Promote(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/v1/deployments/"+id+"/promote", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Rollback(ctx context.Context, envID string, toRevision int) (*Deployment, error) {
	body := map[string]any{}
	if toRevision > 0 {
		body["to_revision"] = toRevision
	}
	var out Deployment
	if err := c.doJSON(ctx, http.MethodPost, "/v1/envs/"+envID+"/rollback", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SetSecret(ctx context.Context, envID, key, value string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/envs/"+envID+"/secrets", map[string]string{"key": key, "value": value}, &map[string]string{})
}

func (c *Client) ListSecrets(ctx context.Context, envID string) ([]SecretMeta, error) {
	var out struct {
		Secrets []SecretMeta `json:"secrets"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/envs/"+envID+"/secrets", nil, &out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}

func (c *Client) DeleteSecret(ctx context.Context, envID, key string) error {
	return c.do(ctx, http.MethodDelete, "/v1/envs/"+envID+"/secrets/"+url.PathEscape(key), nil, nil)
}

func (c *Client) ListNodes(ctx context.Context, orgID string) ([]Node, error) {
	var out struct {
		Nodes []Node `json:"nodes"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/nodes?org="+url.QueryEscape(orgID), nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

func (c *Client) GetNode(ctx context.Context, id string) (*Node, error) {
	var out Node
	if err := c.do(ctx, http.MethodGet, "/v1/nodes/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DrainNode cordons a node and evacuates what it safely can, returning what
// moved and what did not. A node with stranded environments still drained — the
// cordon is the part that always works — so this returns a manifest rather than
// an error for the stranded case.
func (c *Client) DrainNode(ctx context.Context, id string) (*DrainResult, error) {
	var out DrainResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/nodes/"+id+"/drain", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UncordonNode lifts a drain and returns the state the node landed in, which
// the control plane derives from its last heartbeat rather than assuming.
func (c *Client) UncordonNode(ctx context.Context, id string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/nodes/"+id+"/uncordon", map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// RotateNodeRecipient promotes the age key a node has advertised. The operator
// approves what the node is already offering; there is no key to send, because
// the control plane only ever sees public halves.
func (c *Client) RotateNodeRecipient(ctx context.Context, id string) (*Node, error) {
	var out Node
	if err := c.doJSON(ctx, http.MethodPost, "/v1/nodes/"+id+"/rotate-recipient", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListEvents(ctx context.Context, orgID string, limit int, beforeID int64) ([]Event, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if beforeID > 0 {
		q.Set("before_id", fmt.Sprintf("%d", beforeID))
	}
	path := "/v1/orgs/" + orgID + "/events"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out struct {
		Events []Event `json:"events"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	return c.roundTrip(ctx, method, path, rdr, "application/json", out)
}

func (c *Client) doRaw(ctx context.Context, method, path string, body []byte, contentType string, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	return c.roundTrip(ctx, method, path, rdr, contentType, out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	return c.roundTrip(ctx, method, path, body, "", out)
}

func (c *Client) roundTrip(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "composectl")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var wrap struct {
			Error   string `json:"error"`
			Details any    `json:"details"`
		}
		_ = json.Unmarshal(raw, &wrap)
		msg := wrap.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: msg, Details: wrap.Details}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// OpenLogs asks the control plane to fetch a service's container output. It
// returns an instruction, not output: the node has to be polled before anything
// exists to read, so the caller then reads with LogPage.
func (c *Client) OpenLogs(ctx context.Context, envID, service string, tail int, follow bool) (*LogRequest, error) {
	body := map[string]any{"service": service, "follow": follow}
	if tail > 0 {
		body["tail"] = tail
	}
	var out LogRequest
	if err := c.doJSON(ctx, http.MethodPost, "/v1/envs/"+envID+"/logs", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReadLogs returns whatever has arrived for a request since cursor.
func (c *Client) ReadLogs(ctx context.Context, requestID string, cursor int64) (*LogPage, error) {
	path := "/v1/logs/" + requestID
	if cursor > 0 {
		path += "?cursor=" + strconv.FormatInt(cursor, 10)
	}
	var out LogPage
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseLogs ends a tail. Worth calling even on the way out of a failure: a
// followed request left open keeps its node reading Docker every tick for output
// that nothing will collect.
func (c *Client) CloseLogs(ctx context.Context, requestID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/logs/"+requestID, nil, nil)
}

// ---------------------------------------------------------- operator identity

type Operator struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type OrgMember struct {
	OrgID      string `json:"org_id"`
	OperatorID string `json:"operator_id"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Role       string `json:"role"`
}

type Whoami struct {
	Operator *Operator      `json:"operator"`
	Orgs     []Organization `json:"organizations"`
}

func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	var out Whoami
	if err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListMembers(ctx context.Context, orgID string) ([]OrgMember, error) {
	var out struct {
		Members []OrgMember `json:"members"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/orgs/"+orgID+"/members", nil, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

// AddMemberResult carries the one-time token issued when this call created the
// operator. It is empty for an operator who already existed — the server does
// not mint a second credential for someone joining a second org.
type AddMemberResult struct {
	Member OrgMember `json:"member"`
	Token  string    `json:"token,omitempty"`
}

func (c *Client) AddMember(ctx context.Context, orgID, email, name, role string) (*AddMemberResult, error) {
	var out AddMemberResult
	body := map[string]string{"email": email, "name": name, "role": role}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/orgs/"+orgID+"/members", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RemoveMember(ctx context.Context, orgID, operatorID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/orgs/"+orgID+"/members/"+operatorID, nil, nil)
}
