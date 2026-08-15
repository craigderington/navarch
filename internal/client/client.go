package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func (c *Client) DrainNode(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/nodes/"+id+"/drain", map[string]any{}, &map[string]string{})
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
