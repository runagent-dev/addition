-- How the credential was obtained: 'oauth2' (access and refresh tokens from a consent flow) or
-- 'api_key' (a token the user created, never refreshed). Existing rows are OAuth credentials.
ALTER TABLE credentials ADD COLUMN kind TEXT NOT NULL DEFAULT 'oauth2';
