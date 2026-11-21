Every stable error code addition emits, with its exit code and the fix.

# Errors

A failure prints one JSON line on stderr and exits non-zero:

Example: `{"error":{"code":"needs-reauth","message":"gmail:work must be reconnected; …"}}`.

Branch on `code` (stable), not `message`. Exit: 1 internal, 2 usage, 3 needs (re)connect, 4 upstream 4xx, 5 upstream 5xx or network; "4 or 5" follows the HTTP status.

| Code | Exit | Fix |
|---|---|---|
| `usage`, `app-required`, `unknown-topic` | 2 | Wrong arguments: `addition <command> --help`; `addition guide` lists topics |
| `unknown-provider`, `not-an-app`, `not-identity-root`, `not-oauth` | 2 | Use an app from `addition guide apps`; for `client add` use `google` |
| `no-oauth-client`, `bad-client-file`, `web-client` | 2 | `addition client add google --file client_secret.json` with a "Desktop app" client |
| `bad-redirect`, `state-mismatch`, `redirect-mismatch` | 2 | Paste the whole callback URL of the newest link |
| `unknown-state`, `authorization-expired`, `authorization-in-progress`, `stale-authorization` | 2 | Link used, expired or superseded: run `connect --no-browser` again |
| `authorization-timeout`, `loopback` | 2, 1 | Use `connect --no-browser` |
| `identity-mismatch` | 2 | The alias belongs to another account: use another alias |
| `identity-not-allowed` | 2 | Identity URL off the allowed hosts; nothing sent |
| `revoke-affects-other-apps` | 2 | Add `--and-siblings`, or drop `--revoke` |
| `account-required` | 2 | Pass `-a <alias>` or run `accounts default <app>:<alias>` |
| `account-changed` | 2 | Repeat |
| `bad-method`, `bad-path`, `bad-query`, `bad-header`, `forbidden-header`, `bad-data`, `body-too-large`, `bad-provider`, `bad-request` | 2 | Fix the call per `addition guide call` |
| `host-not-allowed`, `path-not-allowed` | 2 | Only the app's own https hosts and paths |
| `bad-output`, `file-exists`, `target-is-symlink`, `binary-response` | 2 | Fix the `-o` target; `--force` replaces; binary needs `-o` |
| `not-connected`, `needs-reauth`, `legacy-account`, `scopes-not-granted` | 3 | `addition connect <app> --as <alias>`, ticking every permission |
| `needs-action` | 3 | `addition doctor` found something for you to do; run its `next` command |
| `access-denied`, `authorization-error` | 4 | Cancelled or refused; ask, then retry `connect` |
| `token-exchange-rejected`, `identity-rejected`, `revoke-rejected`, `refresh-rejected` | 4 or 5 | Provider refused; check the OAuth client, reconnect |
| `upstream-error`, `unsupported-encoding` | 4 or 5 | Read the body on stdout; a compressed response is withheld |
| `network`, `credential-echo` | 5 | Retry later |
| `interrupted`, `output`, `internal`, `catalog`, `guide`, `vault` | 1 | A `call` was cancelled, stdout was unwritable, or a bug: reinstall; `addition doctor` shows what is wrong |
| `vault-busy` | 1 | Retry |
| `not-initialized` | 2 | `addition init` |
| `keychain-unavailable`, `no-master-key`, `wrong-master-key`, `invalid-master-key`, `key-file-unsupported` | 2 | Key problems: `addition guide troubleshooting` |
| `insecure-key-file`, `insecure-home`, `relative-path`, `schema-too-new`, `invalid-account` | 2 | Fix permissions; absolute `ADDITION_HOME`; upgrade; `<app>:<alias>` |
