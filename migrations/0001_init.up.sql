-- composectl initial schema
-- Design notes:
--   * deployments is APPEND-ONLY. Rollback = promote an older revision.
--   * spec_json holds the normalized DeploymentSpec, not the raw compose file.
--     raw_compose is kept for audit/debug only.
--   * service_instances is agent-owned reconciliation state.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ---------------------------------------------------------------- orgs/apps

CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE applications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug        TEXT NOT NULL,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

-- ------------------------------------------------------------------- nodes

CREATE TYPE node_state AS ENUM ('pending', 'ready', 'draining', 'unreachable', 'retired');

CREATE TABLE nodes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    hostname        TEXT NOT NULL,
    advertise_addr  INET NOT NULL,          -- WireGuard mesh address
    state           node_state NOT NULL DEFAULT 'pending',
    -- capacity reported by agent heartbeat
    cpu_millis      INTEGER NOT NULL DEFAULT 0,
    memory_bytes    BIGINT  NOT NULL DEFAULT 0,
    -- live usage, updated each heartbeat
    alloc_cpu_millis   INTEGER NOT NULL DEFAULT 0,
    alloc_memory_bytes BIGINT  NOT NULL DEFAULT 0,
    labels          JSONB NOT NULL DEFAULT '{}'::jsonb,
    agent_version   TEXT,
    last_heartbeat  TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, hostname)
);

CREATE INDEX nodes_heartbeat_idx ON nodes (last_heartbeat)
    WHERE state IN ('ready', 'draining');

-- ------------------------------------------------------------------ stacks

-- A stack is the versioned compose definition. Editing it creates a new
-- version row rather than mutating in place.
CREATE TABLE stacks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id      UUID NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    slug        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (app_id, slug)
);

CREATE TABLE stack_versions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stack_id     UUID NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,
    raw_compose  TEXT NOT NULL,             -- audit only
    spec_json    JSONB NOT NULL,            -- normalized DeploymentSpec
    spec_digest  TEXT NOT NULL,             -- sha256 of canonical spec_json
    created_by   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (stack_id, version)
);

-- ------------------------------------------------------------ environments

CREATE TYPE rollout_strategy AS ENUM ('blue_green', 'rolling', 'recreate');

CREATE TABLE environments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stack_id      UUID NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
    slug          TEXT NOT NULL,            -- prod | staging | pr-142
    strategy      rollout_strategy NOT NULL DEFAULT 'blue_green',
    hostname      TEXT,                     -- ingress hostname
    config        JSONB NOT NULL DEFAULT '{}'::jsonb,  -- non-secret env overlay
    -- current live deployment; NULL until first successful rollout
    live_deployment_id UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (stack_id, slug)
);

-- ------------------------------------------------------------- deployments

CREATE TYPE deployment_state AS ENUM (
    'pending',      -- accepted, not yet scheduled
    'scheduling',   -- placement decided, agents notified
    'starting',     -- containers coming up
    'healthy',      -- all healthchecks green, not yet live
    'live',         -- receiving traffic
    'superseded',   -- was live, replaced by newer revision
    'failed',
    'stopped'
);

-- APPEND-ONLY. Never UPDATE anything but `state` and timestamps.
CREATE TABLE deployments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    environment_id    UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    stack_version_id  UUID NOT NULL REFERENCES stack_versions(id),
    revision          INTEGER NOT NULL,
    -- the color slot this revision occupies; alternates per rollout
    slot              TEXT NOT NULL CHECK (slot IN ('blue', 'green')),
    -- docker compose project name; also the network name prefix
    project_name      TEXT NOT NULL UNIQUE,
    state             deployment_state NOT NULL DEFAULT 'pending',
    -- resolved spec: stack spec + env config overlay + secret refs
    resolved_spec     JSONB NOT NULL,
    -- rollout bookkeeping
    promoted_at       TIMESTAMPTZ,
    superseded_at     TIMESTAMPTZ,
    failure_reason    TEXT,
    created_by        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, revision)
);

ALTER TABLE environments
    ADD CONSTRAINT environments_live_deployment_fk
    FOREIGN KEY (live_deployment_id) REFERENCES deployments(id)
    DEFERRABLE INITIALLY DEFERRED;

-- Only one non-terminal deployment per environment at a time.
CREATE UNIQUE INDEX deployments_one_active_idx
    ON deployments (environment_id)
    WHERE state IN ('pending', 'scheduling', 'starting', 'healthy');

CREATE INDEX deployments_env_revision_idx
    ON deployments (environment_id, revision DESC);

-- -------------------------------------------------------- service instances

CREATE TYPE instance_state AS ENUM (
    'pending', 'pulling', 'starting', 'running',
    'unhealthy', 'stopped', 'failed'
);

-- One row per (deployment, service). Agent-owned.
CREATE TABLE service_instances (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id  UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    node_id        UUID NOT NULL REFERENCES nodes(id),
    service_name   TEXT NOT NULL,
    -- false for services with named volumes; those are pinned & shared
    swappable      BOOLEAN NOT NULL DEFAULT true,
    container_id   TEXT,
    image_ref      TEXT NOT NULL,
    state          instance_state NOT NULL DEFAULT 'pending',
    health_status  TEXT,
    restart_count  INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    started_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (deployment_id, service_name)
);

CREATE INDEX service_instances_node_idx ON service_instances (node_id);
CREATE INDEX service_instances_deployment_idx ON service_instances (deployment_id);

-- --------------------------------------------------------------- volumes

-- Named volumes survive deployments. Pinned to a node until explicitly moved.
CREATE TABLE volumes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    environment_id  UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,          -- compose volume name
    node_id         UUID REFERENCES nodes(id),
    driver          TEXT NOT NULL DEFAULT 'local',
    -- EBS volume id when backed by cloud block storage
    external_ref    TEXT,
    size_bytes      BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, name)
);

-- --------------------------------------------------------------- secrets

-- Ciphertext only. Control plane never stores plaintext; agent decrypts.
CREATE TABLE secrets (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    environment_id  UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    ciphertext      BYTEA NOT NULL,
    key_id          TEXT NOT NULL,          -- age recipient / KMS key id
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (environment_id, key, version)
);

-- ---------------------------------------------------------------- events

-- Append-only audit + rollout timeline. Drives the TUI activity feed.
CREATE TABLE events (
    id             BIGSERIAL PRIMARY KEY,
    org_id         UUID REFERENCES organizations(id) ON DELETE CASCADE,
    deployment_id  UUID REFERENCES deployments(id) ON DELETE CASCADE,
    node_id        UUID REFERENCES nodes(id) ON DELETE SET NULL,
    kind           TEXT NOT NULL,
    message        TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_deployment_idx ON events (deployment_id, id DESC);
CREATE INDEX events_org_time_idx ON events (org_id, created_at DESC);

-- ------------------------------------------------- agent wakeup via NOTIFY

CREATE OR REPLACE FUNCTION notify_node_desired_state() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('node_' || REPLACE(NEW.node_id::text, '-', ''), '');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER service_instances_notify
    AFTER INSERT OR UPDATE ON service_instances
    FOR EACH ROW EXECUTE FUNCTION notify_node_desired_state();

-- updated_at maintenance
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deployments_touch
    BEFORE UPDATE ON deployments
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
