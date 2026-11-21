Google Cloud setup, expiring tokens, and the usual install, vault and key problems.

# Troubleshooting

`addition doctor` reports the vault, key source, OAuth client and accounts, and names the next command to run (`--live` also probes each app once). Then use this page.

## Google Cloud setup (once per user)

addition uses the user's own OAuth client; there is no shared one.

1. Create or pick a Google Cloud project.
2. Enable the API of every app you will connect (Gmail, Google Drive, Google Calendar, Google Chat, Google Docs); a disabled API fails with HTTP 403 `SERVICE_DISABLED`.
3. Configure the OAuth consent screen (External, or Internal for a Workspace org), add the user as a test user and the scopes listed in `addition guide <app>`.
4. Create credentials: OAuth client ID of type **Desktop app** and download the JSON.
5. `addition client add google --file client_secret.json`. A "Web application" client is refused (`web-client`).

While the consent screen is in **Testing**, refresh tokens expire after 7 days: accounts turn `needs-reauth` and calls exit 3. Reconnect, or move the consent screen to **In production** (unverified apps show a warning screen).

## Common failures

- `needs-reauth`, `scopes-not-granted` (exit 3), `legacy-account` (exit 3): reconnect with `addition connect <app> --as <alias>`; for a legacy `google:<alias>` account, remove it and connect each app separately.
- HTTP 403 `SERVICE_DISABLED`: enable that API in the Cloud project, retry after a minute. HTTP 403 `insufficientPermissions`: a scope is missing; `addition accounts show <app>:<alias>`, reconnect.

## Key problems

The master key is in the OS keychain, in `$ADDITION_HOME/master.key` (`addition init --key-file`) or in `ADDITION_MASTER_KEY` (64 hex characters; it always wins when set). There is no fallback between sources.

- `keychain-unavailable` (SSH, containers): `addition init --key-file` (not on Windows), or set `ADDITION_MASTER_KEY`.
- `no-master-key`: restore the recorded key source, or set `ADDITION_MASTER_KEY`.
- `wrong-master-key`: use the key the vault was created with; a lost key cannot be recovered (remove the data dir, `addition init` again).
- `invalid-master-key`: not 64 hex characters. `insecure-key-file`: `chmod 600 $ADDITION_HOME/master.key`. `insecure-home`: `chmod go-w` the data dir or move `ADDITION_HOME`.
