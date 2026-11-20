-- An authorization started by `addn connect --no-browser` and finished once by `--complete`.
-- account_scopes is what the alias held at the start; claimed_at marks a completion in flight.
-- Timestamps here are fixed-width RFC 3339 UTC, so expired rows can be pruned by text comparison.
CREATE TABLE pending_auth (
    state                   TEXT PRIMARY KEY,
    provider                TEXT NOT NULL,
    alias                   TEXT NOT NULL,
    scopes                  TEXT NOT NULL DEFAULT '[]',
    account_scopes          TEXT NOT NULL DEFAULT '[]',
    redirect_uri            TEXT NOT NULL,
    code_verifier_encrypted TEXT,
    claimed_at              TEXT,
    created_at              TEXT NOT NULL,
    expires_at              TEXT NOT NULL
);

-- The provider's stable account id; identity stays a display name such as an email.
ALTER TABLE accounts ADD COLUMN subject TEXT NOT NULL DEFAULT '';
