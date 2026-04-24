-- Addition: Embeddable Integration Runtime
-- Migration 001: Initial schema
--
-- Idempotent. Safe to re-run after `bootstrap()` has already created the schema.

CREATE SCHEMA IF NOT EXISTS _addition;

-- Schema version tracking. Single row keyed by the string 'schema_version'.
CREATE TABLE IF NOT EXISTS _addition.meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Provider configurations (developer's OAuth app credentials).
CREATE TABLE IF NOT EXISTS _addition.provider_configs (
    provider_id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL,
    client_secret_encrypted TEXT NOT NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    extra_config JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-user connections.
CREATE TABLE IF NOT EXISTS _addition.connections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    provider_id TEXT NOT NULL REFERENCES _addition.provider_configs(provider_id),
    access_token_encrypted TEXT NOT NULL,
    refresh_token_encrypted TEXT,
    token_expires_at TIMESTAMPTZ,
    token_type TEXT NOT NULL DEFAULT 'Bearer',
    raw_token_response_encrypted TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    status_detail TEXT,
    scopes_granted TEXT[] NOT NULL DEFAULT '{}',
    provider_user_id TEXT,
    provider_user_email TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_connections_user_provider
    ON _addition.connections (user_id, provider_id);

CREATE INDEX IF NOT EXISTS idx_connections_expiring
    ON _addition.connections (token_expires_at)
    WHERE status = 'active' AND refresh_token_encrypted IS NOT NULL;

-- OAuth state (short-lived, for in-flight auth flows).
CREATE TABLE IF NOT EXISTS _addition.oauth_state (
    state TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    code_verifier TEXT,
    redirect_uri TEXT NOT NULL,
    scopes_requested TEXT[] NOT NULL DEFAULT '{}',
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '15 minutes')
);

CREATE INDEX IF NOT EXISTS idx_oauth_state_expires
    ON _addition.oauth_state (expires_at);

-- Audit log.
CREATE TABLE IF NOT EXISTS _addition.audit_log (
    id BIGSERIAL PRIMARY KEY,
    event_type TEXT NOT NULL,
    user_id TEXT,
    provider_id TEXT,
    connection_id UUID,
    detail JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_log_user
    ON _addition.audit_log (user_id, created_at DESC);

-- Record this migration as applied. Must be the LAST statement.
INSERT INTO _addition.meta (key, value) VALUES ('schema_version', '001')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();
