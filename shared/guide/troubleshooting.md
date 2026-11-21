Google Cloud setup, expiring tokens, and the usual install, vault and key problems.

# Troubleshooting

Run `addition doctor` first: it is the first diagnostic command and reports the vault, the master-key source, the accounts and `PATH` in one JSON document. Then use this page.

## Google Cloud setup (once per user)

addition uses the user's own OAuth client; there is no shared one.

1. In the Google Cloud console create or pick a project.
2. Enable the API of each app you will connect (Gmail, Google Drive, Google Calendar, Google Chat, Google Docs). A disabled API fails with HTTP 403 and `SERVICE_DISABLED` in the body.
3. Configure the OAuth consent screen (External, or Internal for a Workspace org), add the user as a test user and the scopes listed in `addition guide <app>`.
4. Create credentials: OAuth client ID of type **Desktop app** and download the JSON.
5. `addition client add google --file client_secret.json`. A "Web application" client is refused (`web-client`).

While the consent screen is in **Testing**, Google expires refresh tokens after 7 days: each account turns `needs-reauth` and calls exit 3. Reconnect, or move the consent screen to **In production** (an unverified app shows a warning screen but its tokens no longer expire after 7 days).

## Symptom to fix

| Symptom | Fix |
|---|---|
| `not-initialized` | `addition init` |
| `needs-reauth`, `scopes-not-granted` (exit 3) | `addition connect <app> --as <alias>`; see `addition guide connect` |
| `legacy-account` (exit 3) | Remove the old `google:<alias>` account, connect each app separately |
| `host-not-allowed`, `path-not-allowed` | The path is outside that app's API; use the right app (`addition guide apps`) |
| `vault-busy` (exit 1) | Another addition process holds the vault; retry in a moment |
| HTTP 403 `SERVICE_DISABLED` | Enable that API in the Cloud project; retry after a minute |
| HTTP 403 `insufficientPermissions` | Missing scope: `addition accounts show <app>:<alias>`, reconnect |

## Key problems

The master key lives in the OS keychain, in `$ADDITION_HOME/master.key` (`addition init --key-file`), or in `ADDITION_MASTER_KEY` (64 hex characters; when set it always wins). The vault never falls back to another source.

- `keychain-unavailable` (SSH, containers): `addition init --key-file`, or set `ADDITION_MASTER_KEY`.
- `no-master-key`: restore the recorded key source, or set `ADDITION_MASTER_KEY`.
- `wrong-master-key`: use the key this vault was created with. A lost key cannot be recovered: remove the data dir and run `addition init` again.
- `invalid-master-key`: `ADDITION_MASTER_KEY` is not 64 hex characters.
- `insecure-key-file`: `chmod 600 $ADDITION_HOME/master.key`. `insecure-home`: the data dir is writable by others; `chmod go-w` it or move `ADDITION_HOME`.
