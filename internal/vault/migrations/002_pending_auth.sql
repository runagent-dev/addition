-- An authorization started by `addn connect --no-browser`, taken once by `--complete`.
-- expires_at is fixed-width RFC 3339 UTC, so expired rows can be pruned by text comparison.
CREATE TABLE pending_auth (
    state                   TEXT PRIMARY KEY,
    provider                TEXT NOT NULL,
    alias                   TEXT NOT NULL,
    scopes                  TEXT NOT NULL DEFAULT '[]',
    redirect_uri            TEXT NOT NULL,
    code_verifier_encrypted TEXT,
    created_at              TEXT NOT NULL,
    expires_at              TEXT NOT NULL
);
