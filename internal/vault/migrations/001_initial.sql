-- Timestamps are RFC 3339 UTC text. Columns ending in _encrypted hold
-- base64(iv || tag || ciphertext) blobs sealed with AAD naming their row and column.

CREATE TABLE oauth_clients (
    provider                TEXT PRIMARY KEY,
    client_id               TEXT NOT NULL,
    client_secret_encrypted TEXT NOT NULL,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

-- AUTOINCREMENT: an id is never reused, so a sealed blob cannot open under a later account.
CREATE TABLE accounts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    provider   TEXT NOT NULL,
    alias      TEXT NOT NULL,
    identity   TEXT NOT NULL DEFAULT '',
    scopes     TEXT NOT NULL DEFAULT '[]',
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (provider, alias)
);

CREATE TABLE credentials (
    account_id              INTEGER PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
    access_token_encrypted  TEXT NOT NULL,
    refresh_token_encrypted TEXT,
    token_type              TEXT NOT NULL DEFAULT 'Bearer',
    expires_at              TEXT,
    updated_at              TEXT NOT NULL
);

-- account_id is a plain, never-reused id, so history survives account removal; no alias is kept.
CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY,
    event      TEXT NOT NULL,
    account_id INTEGER,
    provider   TEXT,
    details    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX audit_log_by_account ON audit_log (account_id, created_at);
