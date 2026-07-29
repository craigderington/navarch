package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

// DefaultPreviewDomain works on a dev box with no DNS at all: Traefik routes on
// the Host header, so `curl -H "Host: pr-1-hello.preview.localhost"` reaches it.
const DefaultPreviewDomain = "preview.localhost"

const (
	defaultPreviewTTLHours = 24
	maxPreviewTTLHours     = 168 // one week
	maxDNSLabel            = 63
	// env8Len is the number of UUID hex characters folded into generated
	// names — the same prefix length store.shortID uses for project names,
	// container labels and volume names.
	env8Len = 8
)

type createPreviewRequest struct {
	Slug           string `json:"slug"`
	StackVersionID string `json:"stack_version_id,omitempty"`
	// InheritSecretsFrom names an environment by slug. Inheritance must be
	// explicit: a preview that silently picked up production secrets would be
	// a credential leak with a convenient API.
	InheritSecretsFrom string `json:"inherit_secrets_from,omitempty"`
	TTLHours           int    `json:"ttl_hours,omitempty"`
	CreatedBy          string `json:"created_by,omitempty"`
}

type createPreviewResponse struct {
	EnvironmentID uuid.UUID  `json:"environment_id"`
	Hostname      string     `json:"hostname"`
	DeploymentID  uuid.UUID  `json:"deployment_id"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

// previewHostname folds env8 into the name because {slug}-{stack} is not
// unique. stacks.slug is only UNIQUE (app_id, slug) and environments.hostname
// carries no unique constraint, so two applications — in one org or in two —
// that each own a stack "main" with a preview "pr-1" would generate the same
// hostname. ListLiveRoutes would return both, Traefik would get two routers
// with the same Host rule, and which one wins is arbitrary: a cross-tenant
// misroute into a preview running someone else's branch with its inherited
// secrets. env8 is the environment's own id, so it is unique by construction.
func previewHostname(slug, stackSlug, env8, domain string) string {
	return fmt.Sprintf("%s-%s-%s.%s", slug, stackSlug, env8, domain)
}

// validatePreviewLabel measures the label as it will actually be emitted,
// env8 suffix included. Measuring only slug+stack would be wrong in the
// lenient direction — it would admit names resolvers silently truncate,
// which is the failure the limit exists to prevent.
func validatePreviewLabel(slug, stackSlug string) error {
	if n := len(slug) + 1 + len(stackSlug) + 1 + env8Len; n > maxDNSLabel {
		return fmt.Errorf("generated hostname label is %d characters; DNS allows %d", n, maxDNSLabel)
	}
	return nil
}

// handleCreatePreview creates an ephemeral environment, inherits secrets, and
// deploys — one call, one URL back. The point of previews is that CI can create
// one without orchestrating three requests.
func (s *Server) handleCreatePreview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	stackID, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	var req createPreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	ttlHours := req.TTLHours
	if ttlHours == 0 {
		ttlHours = defaultPreviewTTLHours
	}
	// Reject rather than clamp: silently storing a different TTL than the one
	// asked for makes the API lie about what it did.
	if ttlHours < 0 || ttlHours > maxPreviewTTLHours {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("ttl_hours must be between 1 and %d", maxPreviewTTLHours), nil)
		return
	}

	stack, err := s.st.GetStack(ctx, stackID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := validatePreviewLabel(req.Slug, stack.Slug); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// Resolve which stack version to deploy — same rule as handleCreateDeployment.
	var svID uuid.UUID
	if req.StackVersionID != "" {
		if svID, err = uuid.Parse(req.StackVersionID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid stack_version_id", nil)
			return
		}
	} else {
		latest, err := s.st.LatestStackVersion(ctx, stackID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		svID = latest.ID
	}
	sv, err := s.st.GetStackVersion(ctx, svID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	var srcID *uuid.UUID
	var srcKeys []store.SecretMeta
	if req.InheritSecretsFrom != "" {
		src, err := s.st.GetEnvironmentBySlug(ctx, stackID, req.InheritSecretsFrom)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		srcID = &src.ID
		if srcKeys, err = s.st.SecretKeysForEnv(ctx, src.ID); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}

	// A preview starts with an empty config overlay: only secrets are inherited.
	resolved := applyEnvConfig(sv.Spec, nil)

	// Fail fast against the *source* environment's keys, before anything is
	// created. Checking after creation would leave a half-built preview behind
	// for the reaper to collect and the user to wonder about.
	if required := resolved.RequiredSecrets(); len(required) > 0 {
		set := map[string]bool{}
		for _, m := range srcKeys {
			set[m.Key] = true
		}
		var missing []string
		for _, k := range required {
			if !set[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{
				Error: "preview would be missing required secrets", Details: missing})
			return
		}
	}

	// The environment id is generated here rather than by the column default:
	// the hostname carries env8, so the id has to exist before the row does.
	// Generating it in the handler keeps hostname construction next to the
	// length validation above and leaves CreatePreview a straight-line
	// transaction — a presentation concern staying on this side of the store
	// boundary, unlike the slug rules, which are business rules and live in
	// the store.
	envID := uuid.New()
	hostname := previewHostname(req.Slug, stack.Slug, envID.String()[:env8Len], s.previewDomain)
	env, dep, err := s.st.CreatePreview(ctx, store.CreatePreviewParams{
		EnvironmentID: envID,
		StackID:       stackID, Slug: req.Slug, Hostname: hostname,
		TTL: time.Duration(ttlHours) * time.Hour, InheritSecretsFrom: srcID,
		StackVersionID: svID, ResolvedSpec: resolved, CreatedBy: req.CreatedBy,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, createPreviewResponse{
		EnvironmentID: env.ID, Hostname: env.Hostname,
		DeploymentID: dep.ID, ExpiresAt: env.ExpiresAt,
	})
}
