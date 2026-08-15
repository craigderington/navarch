package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRejectsEmptyAndInvalidURL(t *testing.T) {
	if _, err := New("", "tok"); err == nil {
		t.Fatal("empty URL must be rejected")
	}
	if _, err := New("not a url", "tok"); err == nil {
		t.Fatal("invalid URL must be rejected")
	}
}

func TestHealthAndAuthHeader(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" {
		t.Fatalf("status=%q", h.Status)
	}
	if sawAuth != "Bearer secret-token" {
		t.Fatalf("auth header %q", sawAuth)
	}
}

func TestAPIErrorSurfacesMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "environment is missing required secrets",
			"details": []string{"db_password"},
		})
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, "t")
	_, err := c.Deploy(context.Background(), "env-1", "", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.Status != 422 || !strings.Contains(apiErr.Message, "missing required secrets") {
		t.Fatalf("error: %+v", apiErr)
	}
}

func TestPushStackSendsRawBodyAndCreatedBy(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("created_by")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ver-1", "stack_id": "st-1", "version": 2, "spec_digest": "abc",
		})
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, "t")
	sv, err := c.PushStack(context.Background(), "st-1", []byte("services:\n  a:\n    image: nginx\n"), "craig")
	if err != nil {
		t.Fatal(err)
	}
	if sv.Version != 2 || sv.SpecDigest != "abc" {
		t.Fatalf("version: %+v", sv)
	}
	if gotPath != "/v1/stacks/st-1/versions" || gotQuery != "craig" {
		t.Fatalf("path=%s query=%s", gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, "image: nginx") {
		t.Fatalf("body not raw compose: %q", gotBody)
	}
	if gotCT != "application/x-yaml" {
		t.Fatalf("content-type %q", gotCT)
	}
}

func TestListOrgsDecodesEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"organizations":[{"id":"o1","slug":"dev","name":"Development"}]}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, "t")
	orgs, err := c.ListOrgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].Slug != "dev" {
		t.Fatalf("orgs=%+v", orgs)
	}
}

func TestSetSecretDoesNotReturnValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["key"] != "db_password" || body["value"] != "s3cret" {
			t.Fatalf("body=%v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "db_password"})
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, "t")
	if err := c.SetSecret(context.Background(), "env-1", "db_password", "s3cret"); err != nil {
		t.Fatal(err)
	}
}
