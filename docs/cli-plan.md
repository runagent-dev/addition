# Addition CLI — Direction & Build Plan

> Status: **accepted 2026-11-20.** Authoritative for direction and work order.
> Replaces the SDK-era `docs/spec.md`, `docs/tdd-plan.md` and
> `docs/mcp-collections-plan.md`, which are preserved, with the TS + Python SDK
> (v0.1 + M14), on branch `archive/sdk-v0.1`.

---

## 1. What we're building

A **local-first, single-binary CLI that gives any coding agent authenticated
access to your accounts** — several Gmail inboxes, several Drives, calendars,
GitHub, Slack, Discord — with every credential stored on your own machine.

Think *"rclone / `gh`, but for every SaaS API, built for agents"*:

```bash
addition client add google --file client_secret.json     # once; every Google app shares it
addition connect gmail --as work
addition connect google-drive --as personal
addition connect google-calendar --as work

addition gmail search-messages -a work --query "from:billing has:attachment newer_than:7d"
addition gmail get-attachment  -a work --message 18c2… --attachment 0.1 -o ./invoice.pdf
addition drive upload          -a personal --file ./invoice.pdf --folder "Taxes/2026"
addition call google-calendar -a work GET /calendar/v3/calendars/primary/events --query maxResults=5
```

The agent needs nothing but a shell. Claude Code, Codex, OpenCode, Cursor,
a cron job: all of them can run a binary and read JSON.

### 1.1 Who it's for

1. **Primary:** a developer whose coding agents need to touch their real accounts
   (mail, files, calendar, chat) from any harness, on any machine, including
   headless SSH boxes.
2. **Secondary (later):** programs that embed it through thin language wrappers
   (Python / TS packages that ship the binary and parse its JSON).

### 1.2 Why this and not the existing tools

From the market research (Nov 2026):

| Gap in existing tools | Our answer |
|---|---|
| Core engine closed or paywalled (Composio, Arcade, Nango's actions/syncs/MCP) | Whole engine Apache-2.0. No hosted tier holding features hostage. |
| MCP-first; files and pipes are second-class (Executor) | CLI-first. Files in, files out, `--json` everywhere. MCP is a projection on top (Phase 2). |
| OAuth / token handling is the #1 bug source everywhere (Executor: ~50 of 400 recent issues) | We already have a TDD'd OAuth/refresh/retry engine + fixtures; port it, keep the fixtures as the contract. |
| Heavy stacks, fragile native deps (Bun + napi keyring + workerd) | One static Go binary. Pure-Go SQLite. No runtime. |
| State split across files → silent expiries (Executor #2564) | One data dir, one SQLite file. |
| Telemetry on by default (authsome) | No telemetry. Ever. |
| Single-vendor CLIs (`gog`, `gws`) | One grammar across providers, provider config as data. |

### 1.3 Principles

1. **Local only.** No server of ours, no account with us, nothing phones home.
2. **One binary, one data dir.** `~/.local/share/addition/` (XDG; macOS: `~/Library/Application Support/addition`), overridable with `ADDITION_HOME`.
3. **Agent-first output.** Tool commands print JSON on stdout; errors print a JSON envelope on stderr with a stable exit code. Never interactive unless a human is at a TTY.
4. **Tokens never leave the vault in plaintext.** Not in stdout, logs, errors, or `--debug` output. No `addition token print` command. Agents get API access through `addition call`, not raw tokens.
5. **Provider config is data, quirks are code.** Kept from the SDK era: provider + toolkit YAML, adapters for quirks.
6. **One connection = one app + alias.** Each app (Gmail, Google Drive, Google Calendar, Google Chat, Google Docs, later Microsoft Teams, GitHub, Slack…) is its own provider with its own scopes and hosts, so a frontend can show one "Connect" card per app. The OAuth client is shared per identity root (`addition client add google` once serves every Google app).
7. **Every user-visible phase ships a usable release.**

---

## 2. Architecture

```
            ┌────────────── addition (Go binary) ──────────────┐
 agent ──▶  │ CLI layer (cobra)  ── commands generated from    │
 shell      │                       embedded toolkit YAML      │
            │        │                                         │
            │ Tool engine: validate → interpolate → transform  │
            │        │                                         │
            │ Proxy: inject token · 401→refresh→retry once ·   │
            │        429 Retry-After · binary streaming        │
            │        │                                         │
            │ Token manager ◀──▶ OAuth engine (loopback+PKCE)  │
            │        │                                         │
            │ Vault: SQLite (addition.db) · AES-256-GCM blobs  │
            │        master key ← OS keychain | key file | env │
            └──────────────────────────────────────────────────┘
                     │ HTTPS
                     ▼
              Google / GitHub / Slack / …
```

### 2.1 Data model (SQLite, `addition.db`)

| Table | Holds |
|---|---|
| `meta` | `schema_version` (self-bootstrapping migrations, as before) |
| `oauth_clients` | per identity root (`google`): client_id, **encrypted** client_secret (user brings their own); every app under the root uses it |
| `accounts` | one row per (app provider, `alias`): app (`gmail`, `google-drive`…), identity (`me@work.com`), the stable subject id the alias was bound to in that app, the scopes granted to that app, status. The same alias in two apps is two independent accounts. A row whose provider is an identity root with apps (`google`) predates this model and is reported as `legacy-account` (reconnect each app) |
| `credentials` | encrypted access/refresh token, expiry — one row per account |
| `pending_auth` | a `--no-browser` authorization awaiting `--complete`: state, sealed PKCE verifier, alias, scopes, the alias's scopes at the start, redirect URI, claim marker, expiry |
| `audit_log` | connect / refresh / revoke / call events by account id and provider id; never token values, aliases, paths or queries |

The `user_id` multi-tenancy column from the SDK becomes the account alias.

### 2.2 Encryption & master key

- Wire format unchanged: `base64(iv[12] || tag[16] || ciphertext)`, AES-256-GCM.
  The existing `crypto_interop.yaml` fixture still applies.
- Master key resolution: a set `ADDITION_MASTER_KEY` always wins (set but empty is
  an error, not "absent"). Otherwise the vault unlocks only from the source
  `addition init` recorded in `meta`: OS keychain (`go-keyring`) by default, or the
  key file `$ADDITION_HOME/master.key` (0600) **only if the user opted in at
  `addition init --key-file`**. There is no keychain → file search, so a first run
  and its rerun always agree. Headless Linux without a secret service is a
  first-class case (it's our own dev setup), so the key-file path must be good,
  not a fallback hack.
- If the keychain is unavailable and no opt-in exists: fail loudly. Never
  silently downgrade to plaintext.
- The keychain entry is named by a random `vault_id` kept in `meta`, not by the
  home path, so a vault moved to a new `ADDITION_HOME` still finds its key.
- Vault secrets are sealed with versioned AAD naming the vault ID, table, row
  and column, so a blob copied to another account or column fails to decrypt.
  The nil-AAD helpers stay for SDK interop and the fixture.
- Safe path (Unix): the data dir must be absolute (relative `ADDITION_HOME`,
  `XDG_DATA_HOME` or `HOME` is a usage error) and is resolved to a
  symlink-free path before use. Every ancestor from `/` must be owned by root
  or the user and not group- or world-writable, except a root-owned sticky
  dir such as `/tmp` (OpenSSH StrictModes). A user-owned ancestor may be
  group-writable (never world-writable) when its group is the user's private
  group: their primary gid, named after them, with no other member in
  `/etc/group` or `/etc/passwd`; if that cannot be read or verified (e.g.
  NSS-only accounts), addition refuses. The dir itself must be the user's
  and 0700; the DB, any `-wal`/`-shm`/`-journal` and the key file must be
  regular, the user's and 0600 or stricter. Otherwise addition refuses with
  `insecure-home`. SQLite opens the resolved path; key-file reads and writes
  go through one directory fd, and the key is published with an atomic
  no-replace rename (link fallback) under the SQLite writer lock, which is
  also held while leftover `.master.key-*` names are removed.
  On Windows the profile check runs on the nearest existing ancestor's
  resolved path before any directory is created.
- Windows (v0.1): the key lives in Credential Manager or `ADDITION_MASTER_KEY`;
  `--key-file` and file-backed vaults are refused. The data dir must be
  inside `%USERPROFILE%` and relies on the profile's inherited ACLs. Private
  DACLs, and with them key files, are a follow-up.

### 2.3 OAuth on any machine

- **Default:** RFC 8252 loopback redirect on `127.0.0.1:<random port>`, PKCE S256,
  `state` validated and discarded. The listener answers 400 to any callback that is
  not for its state, or that carries both or neither of `code` and `error`, and
  accepts exactly one verified one (a code or a denial), then closes before the code
  is exchanged.
- **Headless / SSH, in two steps:** `addition connect … --no-browser` stores a pending
  authorization in the vault (state, sealed PKCE verifier, alias, scopes, redirect
  URI; 10-minute expiry) and prints `{authorize_url, expires_at, next}`. The user
  opens the URL on any device, approves, and runs `addition connect --complete '<url>'`
  with the URL the browser landed on (the localhost page fails to load; the address
  bar still carries `code` + `state`). `--complete` checks the pasted URL before
  touching the pending row, so a bad paste leaves it usable; it then claims the row
  exclusively, and deletes it in the transaction that stores the account. The claim
  is released for a retry only when a DNS, dial or TLS failure left the token request
  without a connection; any other failure deletes the row. A denial deletes the row
  only if no completion holds the claim (else `authorization-in-progress`). No port
  forwarding needed, and the agent never holds the flow open.
- **Identity is data:** `oauth2.identity: {url, field, subject_field}` in the provider
  YAML names the endpoint read with the new token, a display field and the stable
  subject (Google: OpenID `userinfo`, `email`, `sub`). An alias is bound to its
  subject, so an email rename re-consents fine and a different account is refused.
  An account stored without a subject is bound on re-consent and keeps only the new
  tokens.
- **Apps, not services:** `addition connect <app> --as <alias>` takes a provider id or toolkit command
  (`drive` = `google-drive`) and asks only for that app's `oauth2.default_scopes`; the authorize URL never
  carries `include_granted_scopes`, so Google cannot widen the token beyond the app. Connecting an identity
  root that has apps (`google`) is refused with `not-an-app` and the list of apps. Reconnecting the same
  (app, alias) re-consents for the union of its stored scopes and the app's defaults. Connecting, in both the
  browser flow and `--complete`, first checks that the identity URL is on the app's `allowed_hosts` (as
  `accounts check --live` does), else `identity-not-allowed` (exit `2`) before any authorization starts or code is redeemed.
- **Stale re-consent:** a completion is refused (`stale-authorization`) if the alias
  gained scopes after the authorization started that it did not ask for.
- **Back channel:** token, userinfo and revoke requests go straight to the HTTP
  transport, so no redirect is followed, and need a 2xx; any 3xx is a refusal.
  `addition` discards the standard library's logger, so `net/http` never writes to stderr. Error messages are addition's own: the operation, the HTTP status and at
  most an RFC 6749/6750 error identifier, never the provider's text.
- **Exit codes for connect:** `2` for a bad or mismatched paste, a state mismatch,
  an unknown, used, expired, in-progress or stale authorization, or a timeout; `3`
  when the account was saved but some requested scopes were not granted
  (`scopes-not-granted`); `4` for a provider refusal (`access_denied`, a 4xx or 3xx)
  or an unusable 2xx token or identity response; `5` for the network or a 5xx.
- **Bring your own OAuth client** (v0.1): `addition client add google --file client_secret.json`, once per identity
  root: it serves every Google app, and token refresh uses it with the app's inherited token endpoint. Another
  root, such as Microsoft (Teams, planned), will need its own client.
  `addition doctor google` walks through the Google Cloud setup and warns about the
  7-day refresh-token expiry for apps left in "Testing".

### 2.4 Concurrency

Several agents may run `addition` in parallel against the same account. Token
refresh runs inside a SQLite `BEGIN IMMEDIATE` transaction that re-reads the
credential before refreshing, so two processes whose refreshes commit never
burn the same refresh token. "Someone already refreshed" is decided by the
credential's revision (a counter every credential write bumps), not by its
expiry or token text: a short-lived grant, or a rotation that returns the same
access token, is still reused by processes that were waiting.

Waiting for the writer lock stops when the command is cancelled; a lock already
held is never released mid-refresh. A lock that stays busy past SQLite's 10 s
wait is the local error `vault-busy` (exit `1`), never the driver's text.

The guarantee covers refreshes that commit. If the process dies after the
provider rotated the refresh token but before the commit, the stored token is
stale: the next refresh gets `invalid_grant`, and the account is marked
`needs-reauth` (exit 3). It never retries in a loop.

### 2.5 `addition call`

`addition call <app> [-a <alias>] <METHOD> <path> [--query k=v]… [--data @file|-|<json>] [-H 'Name: value']… [-o file] [--force]`
sends one authenticated request to an app's API. It lives in `internal/proxy`; `internal/cli/call.go` is the
thin command.

**Account.** The app comes first, as a provider id or toolkit command (`google-chat`, `drive`); there is no
`--provider` flag and nothing is inferred. A call without an app is `app-required` (exit `2`, naming the connected
apps), an identity root such as `google` is `not-an-app`, and an unknown app is `unknown-provider`. `-a` names the
alias within that app; without it the app's default account, else its only account, is used. Several accounts in the
app without a default are `account-required` (exit `2`); an alias or app with no account is `not-connected` (exit
`3`). An account stored under an identity root is `legacy-account` (exit `3`).

**URL.** `<path>` is absolute from the API host (`/calendar/v3/calendars/primary/events`, resolved against the
host of the app's `api.base_url`) or an `https` URL whose host is listed in the app's own `api.allowed_hosts`
(default: the host of `api.base_url`). Each app lists only its own hosts, plus the identity host, so a Gmail
token cannot reach `chat.googleapis.com` and a Chat token cannot reach Gmail. A listed host matches case-insensitively, without a trailing dot, on port 443.
Any other host or scheme, userinfo, a `//host` path, or an opaque URL is `host-not-allowed` (exit `2`), and nothing is
sent: not the request, and not a token refresh either. The access token is only ever sent to a listed host over https.
Apps that share a host also list `api.allowed_path_prefixes` (arrays replace under `extends`): Drive and Calendar both
use `www.googleapis.com`, so Drive allows `/drive/`, `/upload/drive/` and `/download/drive/` and Calendar `/calendar/`.
When the list is not empty, the target's path, exactly as it will be sent, must start with one of the prefixes on
every allowed host of the app, else `path-not-allowed` (exit `2`) and nothing is sent. A path a server might
normalise into another route is refused the same way: dot segments, double slashes, backslashes, encoded slashes
(`%2f`, `%5c`), percent-encoded dots, `;` parameters on a dot segment, control characters and spaces.
`--query` pairs are appended after the path's own query. `-H` may not set `Authorization`, `Host`, `Cookie`,
`Proxy-*`, `Content-Length`, `Transfer-Encoding`, `Accept-Encoding`, `Upgrade`, `Connection`, `TE`, `Trailer`,
`Keep-Alive` or a method-override header (`forbidden-header`, exit `2`); raw control characters, tab aside, are `bad-header`.
The engine checks the destination again where it attaches the token (https only, a listed host, no userinfo, an allowed path prefix), and
catalog validation rejects any `allowed_hosts` or `base_url` port: the token goes to port 443 only.
`--data` is a literal body, `@file` or `-` for stdin, capped at 64 MiB; the body is buffered so it can be replayed,
and defaults to `Content-Type: application/json`. A SIGINT while stdin is being read ends the command at once.

**Token.** A token that expires within 60 s is refreshed first, under the vault writer lock: the credential is
re-read inside the transaction, so a refresh another process already committed (a newer credential revision) is
reused and the token endpoint is hit once per rotation. Every token read must find the account selected at the
start of the command: an alias removed and connected again meanwhile fails as `account-changed` (exit `2`) and
nothing is sent. A rotated refresh token is stored in the same transaction as the new access token, and a
`refresh` audit event records `ok`, `failed` or `needs-reauth`. Cancellation is checked before the token endpoint is
called; from then on the provider may have rotated the grant, so the exchange (bounded by its own 30 s timeout) and its
commit finish whatever the command's context says, the API request is not sent, and `interrupted` is reported only
afterwards. If the provider invalidates the grant but its reply stalls past that timeout, there is no new grant to
commit and the stored one stays, which ends in `needs-reauth` like a crash before commit. Cancelling while only waiting for the writer lock, or for vault open or migration to get it, returns at once
and never releases a lock another process holds; a migration that holds the lock runs to its commit.
`invalid_grant` marks the account `needs-reauth` and ends in exit `3`; it is never retried. Later calls fail with
exit `3` before any request.

**Retries.** A `401` renews the token once (re-reading what another process stored first) and resends; a second
`401` is an upstream error. With no refresh token, a `401` is exit `3`. A `429` is retried up to three times,
waiting for `Retry-After` (seconds or an HTTP date) or, without it, 1 s, 2 s, 4 s with jitter, never more than
30 s (an all-digit value, however large, saturates at the cap; a number with any other character is malformed and
backs off instead). Nothing else is retried: not `5xx`, not network errors, and the HTTP transport never replays a
request either. The call engine speaks HTTP/1.1 on a fresh connection per attempt (a copy of the client's transport
with keep-alives and HTTP/2 off, proxy and TLS settings kept), with a body the transport cannot resend, so neither a
reused-connection retry nor an HTTP/2 refused-stream retry exists: a connection lost after a mutation was sent ends in
`network` even with an `Idempotency-Key`. The token refresh keeps the client's own transport. A rejected
`401`/`429` response is closed unread. Requests go to the transport itself, so no `Location` is ever parsed or followed: a `3xx`, whatever its `Location`, is an upstream failure and its body is
not read.

**Output.** A `2xx` text, JSON or XML body is streamed to stdout verbatim, exit `0`. Any other `2xx` body is refused
as `binary-response` (exit `2`) unless `-o` is given. addition alone sets `Accept-Encoding`, always to `identity`, and
the caller's is refused. A response that carries any coding other than `identity` (`gzip`, `br`, `zstd`, in one or
several `Content-Encoding` fields) is withheld on every path, except a `HEAD` answer, which has no body,
stdout, an error body and `-o`, as `unsupported-encoding` (exit `5`, or `4`/`5` by status for a non-2xx answer) with
nothing written.
`-o` opens the target's directory once and creates a `0600` temp file in it, before anything is sent; the finished
body is published into that same directory, so renaming the path mid-call cannot strand a part file. Without
`--force` publication is an atomic no-replace (a hard link, then the temp name is removed); a directory that cannot
do that is `bad-output` before the request, and `--force` renames over a regular file. A symlink at the target is
always refused. The temp name is gone before the summary `{"status","content_type","bytes","path"}` is printed, so a
writer killed by a closed stdout leaves only the finished file; `path` is the `-o` argument exactly as given. `-o`
assumes the output directory is not writable by other users: another writer of that directory could swap the temp
name or race the `--force` check before the rename.
SIGINT and SIGTERM cancel the command (`interrupted`, exit `1`) after removing the temp file.

**Echo guard.** Every response-derived output (stdout body, the capped error body, the `-o` file and its summary's
`content_type`) is checked for each access token sent across all attempts, including a token renewed mid-call.
Bytes are held back across reads and at the 1 MiB cap so a split match is caught before any of it is emitted. A
match is the local error `credential-echo` (exit `5`), emits nothing more and discards the temp file. It matches
the token text as sent; a provider that transforms it (encodes it, splits it) is outside the guarantee, and a
compressed body never reaches the guard undecoded (see Output).
Every failure to read a response body, on every path, is `network` (exit `5`) or `interrupted` with addition's own fixed
message: no Go, parser or upstream text ever reaches stderr, so a malformed chunk or trailer cannot repeat a token.
A non-2xx answer writes no file; its body (at most 1 MiB) goes to stdout verbatim, stderr gets the envelope
`{"error":{"code":"upstream-error","message":"HTTP <status>"}}`, and the exit is `4` for 4xx/3xx and `5` for 5xx.
A network, DNS or TLS failure is `network` (exit `5`); error text never contains the URL, query, headers or tokens.

**Audit.** One `call` event per command, after the final response head arrives: account id, method and final
status (`0` when none arrived). It is written even if the command was cancelled after the API answered, on a short
independent context whose 5 s deadline also bounds the wait for a writer lock another process holds. A failed audit write is reported as a `{"notice":"audit-failed"}` line on stderr
and does not change the result.

`addition accounts check --live` (formerly `addition auth check`, which still works as a hidden, deprecated alias) runs the
same refresh-if-needed path for each account and asks the provider's identity endpoint whether the token works: each
row gains `live` and `live_reason` (`ok`, `needs-reauth`, `no-refresh-token`, `no-credential`, `no-oauth-client`,
`refresh-rejected`, `token-rejected`, `identity-rejected`, `network`, `no-identity-endpoint`, `identity-not-allowed`,
`legacy-account`), never provider text. The identity URL is a fixed catalog URL checked by host only, and must be on the app's own `allowed_hosts`, so every app lists
`openidconnect.googleapis.com`. Every account row also carries `scopes_ready` and `missing_scopes`, the app's default
scopes the account was not granted.

**Removing and revoking.** `accounts remove <app>:<alias>` deletes the account and its tokens locally and touches no
other account and no network. With `--revoke` it first asks the provider to revoke the token, best effort. Whether
that reaches other accounts is provider data: `oauth2.revoke_scope` is `token` (the default: only that token) or
`grant` (Google: every token the same user got through the same OAuth client). For a `grant` provider, other accounts
under the same identity root with the same identity subject (legacy `google:<alias>` rows included) would break, so
the command refuses with `revoke-affects-other-apps` (exit `2`), naming them as `<app>:<alias>` and offering
`--and-siblings`. With `--and-siblings` it revokes once and, in the same transaction as the removal, marks each
sibling `needs-reauth` (their next call is exit `3`); the result carries `revoked: true` and
`also_invalidated: ["google-drive:work", …]`. If the revoke fails, no sibling is marked.

### 2.6 Command grammar

```
addition <provider> <tool> [-a <alias>] [--param value …] [-o file] [--all]
addition call <app> [-a <alias>] <METHOD> <path> [--query k=v] [--data @file.json] [-o file]
addition connect <app> --as <alias> [--no-browser]
addition accounts [list|show|remove <app>:<alias> [--revoke [--and-siblings]]|default|check [--live]]
addition tools [--provider p] [--json]            addition skill install [--harness …]
```

- Tool commands are **generated at runtime** from embedded toolkit YAML;
  `snake_case` ids become `kebab-case` commands, params become flags.
- `-a` is optional when a provider has exactly one account or a default is set.
- Exit codes: `0` ok · `1` internal (addition itself failed or was interrupted: stdout not writable, broken embedded data, vault busy) · `2` usage/validation ·
  `3` not connected / needs reauth · `4` upstream 4xx · `5` upstream 5xx/network ·
  `6` policy denied (Phase 2).
- Help text is human-readable; JSON is the contract for tool and data commands.
- Stderr is JSON lines: a progress notice is one object per line, such as
  `{"notice":"open-browser-failed","authorize_url":"…"}`, and an error envelope, if
  any, is always the last line.
- A closed stdout pipe ends the process with SIGPIPE, like standard Unix tools.

---

## 3. Phases

Each phase ends in a tagged release someone can install and use. Inside a phase,
milestones keep the old workflow: Red → Green → Refactor, one PR per milestone,
stop after the PR is opened.

### Phase 1 — "Google for my agents" → `v0.1.0`

**The user value:** connect several Google accounts once; from then on any coding
agent can search and read mail, pull attachments to disk, send mail with
attachments, search/download/upload Drive files, and read/create calendar
events. `addition call` covers every other Google API on day one.

**First usable checkpoint is M4.** From M4 on, every merged PR is something you
can use the same day.

| # | Milestone | Ships | Usable after merge? |
|---|---|---|---|
| M0 | Repo reshape | TS/Python removed from `main`, this plan adopted, CLAUDE.md + README rewritten, Go module + cobra root with `addition version` and the JSON error envelope, CI | — |
| M1 | Catalog | YAML loader (`extends` merge: scalars override, arrays replace), embedded via `go:embed`; identity root of each provider; `effect:` tag and CLI `command:` name per toolkit; Google Drive provider + metadata toolkit; `addition providers`, `addition tools` | browse only |
| M2 | Vault | data dir, SQLite bootstrap + migrations, AES-GCM (+ `crypto_interop` conformance), master-key resolution, store API for clients / accounts / credentials / audit, `addition init` | — |
| M3 | Connect | `addition client add/list` (Desktop-app client JSON), loopback + two-step `--no-browser` / `--complete` OAuth with a pending authorization in the vault, PKCE, one-use state, identity discovery from `oauth2.identity`, re-consent for the union of stored and default scopes (persist the scopes actually granted), `addition connect / accounts / auth check` (local status only) | accounts connected |
| **M4** | **Raw call** | `addition call` (§2.5): host allow-list, token injection, refresh under lock, 401 → retry once, 429 backoff, `-o` binary streaming, `call` audit event; `auth check --live` | **✅ agents can hit any Google API** |
| **M4b** | **Apps are separate connections** | each app its own provider, scopes, hosts and account (`gmail`, `google-drive`, `google-calendar`, new `google-chat` and `google-docs`); `connect <app>`, no `--services`; `call <app>` (app required, no `--provider`); `accounts check` (`auth check` kept as a hidden alias) with `scopes_ready` / `missing_scopes`; one OAuth client per identity root; `legacy-account` for pre-M4b rows; `api.allowed_path_prefixes` keeps an app's token off other apps' routes on a shared host; `oauth2.revoke_scope` and `accounts remove --revoke [--and-siblings]` for Google's grant-wide revocation | ✅ one "Connect" card per app |
| M4c | Doctor | `addition doctor`: one JSON report on the vault, master-key source, accounts and `PATH`, so an agent can diagnose a broken install | ✅ |
| M5 | Curated tools | tool engine (validate, path-param consumption, pagination `--all`), toolkits: gmail, drive, calendar | ✅ friendly commands |
| M6 | Files | Gmail attachment download, send-with-attachment (MIME build), Drive upload (resumable), download, Docs/Sheets export | ✅ the Gmail→Drive flow |
| M7a | Release pipeline + installer | `addition version` build metadata, GoReleaser binaries (linux/mac/windows, amd64/arm64) with version-less archive names and `checksums.txt`, GitHub artifact attestation, `release.yml` on `v*` tags, a snapshot build in CI, `install.sh`, README install section. Built ahead of M5–M6 so every later milestone can be tried from a release | ✅ installable binaries |
| M7b | Self-update | `addition update` (explicit only, checksum-verified, atomic replace, same release assets as the installer) | ✅ |
| M7c | Package-manager shims | npm `@addition/cli`, PyPI `addition`, Homebrew `addition`; each ships or fetches the released binary and verifies its checksum | ✅ |
| M7d | Agent surface | `addition guide`, `addition skill install` (SKILL.md for Claude Code / Codex / generic), `llms.txt`, per-app recipes, `addition tools --json` schemas | ✅ **v0.1.0** |

**M1–M4 test focus (port, don't reinvent):**
- `crypto_interop.yaml`, `token_refresh.yaml`, `proxy_requests.yaml` and
  `tool_schema_export.yaml` get a Go conformance driver. They are the contract
  that the port behaves like the SDK.
- Provider endpoints are faked with `httptest.Server`; a fake OAuth server drives
  the full connect flow, including a state mismatch and a missing refresh token.
- SQLite runs in `t.TempDir()`, starting from an empty DB that runs the real
  bootstrap every time.
- Two goroutines refreshing the same account → exactly one token-endpoint hit.
- Golden tests: no token string ever appears in stdout, stderr or `--debug`
  output for any command.

**Phase 1 non-goals:** MCP, policies, non-Google providers, sync, wrappers,
API-key providers, OpenAPI import.

### Phase 2 — "Safe and broader" → `v0.2.0`

- **Policies per account:** `read-only`, `confirm-writes` (agent gets exit 6 plus
  instructions; a human approves with `addition approve <id>` or in a TTY prompt),
  `deny <tool>`. Every write tool in the YAML is tagged `effect: write|destructive`.
- **Providers:** GitHub, Slack (comma scopes, `ok:false` adapter — already in
  YAML), Discord (bot token), Notion.
- **API-key / bot-token auth type** next to OAuth2.
- `addition mcp` (stdio): the same catalog projected as MCP tools (the M14 schema
  mapper, ported), for harnesses that prefer MCP.
- Audit viewer: `addition log`.

### Phase 3 — "Every API" → `v0.3.0`

- **Generated tools** from OpenAPI and Google Discovery documents
  (`addition import openapi <url> --as provider`), so coverage stops depending on
  hand-written YAML. Curated YAML stays as the high-quality layer on top.
- Community provider registry: a directory of YAML files in the repo and PRs to
  add more. No hosted registry.
- **Wrappers:** the `pip install addition` / `npm i @addition/cli` shims from M7c gain
  language bindings that expose `run(provider, tool, params) -> dict` over the JSON contract.

### Phase 4 — "All my machines" → `v0.4.0`

- `addition vault export/import` (encrypted bundle, age-style passphrase).
- Sync design, decided at that point. Leading options:
  (a) encrypted vault file synced via git / Syncthing, with refresh-token
  rotation conflicts handled by re-reading and re-consenting, or
  (b) a "home" device that holds the tokens, with other devices calling it over
  Tailscale.
- WhatsApp research: Business Cloud API vs the unofficial (whatsmeow)
  route, and its ToS risk.

---

## 4. What carries over from `archive/sdk-v0.1`

| Keep as-is | Port to Go (behaviour pinned by fixtures) | Drop |
|---|---|---|
| `shared/providers/*.yaml`, `shared/toolkits/*.yaml` | crypto, provider registry + `extends`, toolkit loader, transforms, adapters, OAuth engine, token manager, proxy, tool engine, M14 schema projection | Postgres, `_addition` schema namespace, testcontainers, multi-tenant `user_id`, framework callback handlers, background task runner, REST auto-mount, collections, TS/Python parity machinery |
| `shared/test-fixtures/*.yaml` | Postgres schema → SQLite migrations (M2) | `shared/spec.yaml` (SDK API contract) |

New YAML needed in Phase 1: `providers/google-drive.yaml`, `toolkits/google-drive.yaml`,
plus `effect:` tags on tools (used from Phase 2, cheap to add now).

---

## 5. Decisions (accepted 2026-11-20)

1. **Binary name:** `addition`. Go module `github.com/runagent-dev/addition`.
2. **Grammar:** gh-style. `addition gmail search-messages -a work`.
3. **CLI framework:** cobra.
4. **Output:** tool commands always print JSON on stdout. Errors print (when
   stderr is writable; a closed stdout pipe is the SIGPIPE exception in §2.5) a
   JSON envelope on stderr: `{"error":{"code":"…","message":"…"}}`.
5. **Repo:** pivot `runagent-dev/addition` in place. SDK history stays on
   `archive/sdk-v0.1`.
6. **OAuth clients:** bring your own in v0.1. Revisit a verified shared Google
   client later, since it means a security assessment and a support burden.
7. **Toolchain:** Go 1.27 (`go.mod` pins it, so older local Go installs fetch
   it automatically via `GOTOOLCHAIN`).
8. **Apps are separate connections (2026-11-21, M4b):** every app is its own provider and its own connection, replacing the
   earlier "one account serves Gmail + Drive + Calendar" design. Each connection asks Google only for its app's scopes
   (least privilege), can be removed on its own, and sends its token only to its app's hosts and routes. The OAuth
   client stays shared per identity root. Revocation is the exception to "on its own": Google revokes the whole grant
   (every token the user got through that client), so `accounts remove --revoke` refuses while sibling accounts of the
   same identity exist (`oauth2.revoke_scope: grant` in the provider data) unless `--and-siblings` is given. Microsoft Teams is the planned example of a second identity root (own client, own apps).
   `addition call` names its app first, and `addition auth check` became `addition accounts check`.
9. **License (2026-11-21):** Apache-2.0, with a NOTICE file; both ship in every release archive.
10. **Package names (checked 2026-11-21):** `addition` is free on PyPI and Homebrew (formula and cask). On npm the unscoped
    `addition` is taken, so the package is `@addition/cli` (the `@addition` scope still has to be claimed); crates.io
    `addition` is taken too, so there is no crate.
11. **Release integrity (M7a):** one sha256 `checksums.txt` per release plus a GitHub artifact attestation
    (`gh attestation verify`) for every archive and the checksums file. No cosign: it adds a key or identity policy
    to manage for no gain over GitHub's own attestation. The installer checks the archive against the `checksums.txt`
    from the same release origin on every run and does not verify the attestation. Releases are built reproducibly
    (commit-derived dates and file times), attested, and only then published, by separate jobs with least privilege;
    a tag must be a release tag on a commit reachable from `main`.
12. **Install script:** non-interactive, no `sudo`, installs one file to `~/.local/bin` by default, and never edits shell
    rc files; when the directory is not on `PATH` it prints the line to add. Archive names carry no version, so
    `releases/latest/download/<asset>` is a stable URL. Windows users take the `.zip` for now.
13. **No automatic update checks (M7b):** `addition update` runs only when asked. The binary never contacts the network on
    its own account, in line with "no phoning home".
