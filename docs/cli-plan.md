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
addn connect google --as work --services gmail,drive,calendar
addn connect google --as personal --services gmail,drive

addn gmail search-messages -a work --query "from:billing has:attachment newer_than:7d"
addn gmail get-attachment  -a work --message 18c2… --attachment 0.1 -o ./invoice.pdf
addn drive upload          -a personal --file ./invoice.pdf --folder "Taxes/2026"
addn call -a work GET /calendar/v3/calendars/primary/events --query maxResults=5
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
| Core engine closed or paywalled (Composio, Arcade, Nango's actions/syncs/MCP) | Whole engine MIT. No hosted tier holding features hostage. |
| MCP-first; files and pipes are second-class (Executor) | CLI-first. Files in, files out, `--json` everywhere. MCP is a projection on top (Phase 2). |
| OAuth / token handling is the #1 bug source everywhere (Executor: ~50 of 400 recent issues) | We already have a TDD'd OAuth/refresh/retry engine + fixtures; port it, keep the fixtures as the contract. |
| Heavy stacks, fragile native deps (Bun + napi keyring + workerd) | One static Go binary. Pure-Go SQLite. No runtime. |
| State split across files → silent expiries (Executor #2564) | One data dir, one SQLite file. |
| Telemetry on by default (authsome) | No telemetry. Ever. |
| Single-vendor CLIs (`gog`, `gws`) | One grammar across providers, provider config as data. |

### 1.3 Principles

1. **Local only.** No server of ours, no account with us, nothing phones home.
2. **One binary, one data dir.** `~/.local/share/addition/` (XDG; macOS: `~/Library/Application Support/addition`), overridable with `ADDN_HOME`.
3. **Agent-first output.** Tool commands print JSON on stdout; errors print a JSON envelope on stderr with a stable exit code. Never interactive unless a human is at a TTY.
4. **Tokens never leave the vault in plaintext.** Not in stdout, logs, errors, or `--debug` output. No `addn token print` command. Agents get API access through `addn call`, not raw tokens.
5. **Provider config is data, quirks are code.** Kept from the SDK era: provider + toolkit YAML, adapters for quirks.
6. **One account = one identity.** A Google account connects once and serves Gmail, Drive and Calendar; adding a service later re-consents the same account for the union of its granted and requested scopes, not a new account.
7. **Every user-visible phase ships a usable release.**

---

## 2. Architecture

```
            ┌──────────────── addn (Go binary) ────────────────┐
 agent ──▶  │ CLI layer (cobra)  ── commands generated from    │
 shell      │                       embedded toolkit YAML      │
            │        │                                          │
            │ Tool engine: validate → interpolate → transform  │
            │        │                                          │
            │ Proxy: inject token · 401→refresh→retry once ·   │
            │        429 Retry-After · binary streaming        │
            │        │                                          │
            │ Token manager ◀──▶ OAuth engine (loopback+PKCE)  │
            │        │                                          │
            │ Vault: SQLite (addition.db) · AES-256-GCM blobs  │
            │        master key ← OS keychain | key file | env │
            └───────────────────────────────────────────────────┘
                     │ HTTPS
                     ▼
              Google / GitHub / Slack / …
```

### 2.1 Data model (SQLite, `addition.db`)

| Table | Holds |
|---|---|
| `meta` | `schema_version` (self-bootstrapping migrations, as before) |
| `oauth_clients` | per provider: client_id, **encrypted** client_secret (user brings their own) |
| `accounts` | `alias` (`work`), provider (`google`), identity (`me@work.com`), the provider's stable subject id, granted scopes, status |
| `credentials` | encrypted access/refresh token, expiry — one row per account |
| `pending_auth` | a `--no-browser` authorization awaiting `--complete`: state, sealed PKCE verifier, alias, scopes, the alias's scopes at the start, redirect URI, claim marker, expiry |
| `audit_log` | connect / refresh / revoke / tool-call events by account id and provider id; never token values or aliases |

The `user_id` multi-tenancy column from the SDK becomes the account alias.

### 2.2 Encryption & master key

- Wire format unchanged: `base64(iv[12] || tag[16] || ciphertext)`, AES-256-GCM.
  The existing `crypto_interop.yaml` fixture still applies.
- Master key resolution: a set `ADDN_MASTER_KEY` always wins (set but empty is
  an error, not "absent"). Otherwise the vault unlocks only from the source
  `addn init` recorded in `meta`: OS keychain (`go-keyring`) by default, or the
  key file `$ADDN_HOME/master.key` (0600) **only if the user opted in at
  `addn init --key-file`**. There is no keychain → file search, so a first run
  and its rerun always agree. Headless Linux without a secret service is a
  first-class case (it's our own dev setup), so the key-file path must be good,
  not a fallback hack.
- If the keychain is unavailable and no opt-in exists: fail loudly. Never
  silently downgrade to plaintext.
- The keychain entry is named by a random `vault_id` kept in `meta`, not by the
  home path, so a vault moved to a new `ADDN_HOME` still finds its key.
- Vault secrets are sealed with versioned AAD naming the vault ID, table, row
  and column, so a blob copied to another account or column fails to decrypt.
  The nil-AAD helpers stay for SDK interop and the fixture.
- Safe path (Unix): the data dir must be absolute (relative `ADDN_HOME`,
  `XDG_DATA_HOME` or `HOME` is a usage error) and is resolved to a
  symlink-free path before use. Every ancestor from `/` must be owned by root
  or the user and not group- or world-writable, except a root-owned sticky
  dir such as `/tmp` (OpenSSH StrictModes). A user-owned ancestor may be
  group-writable (never world-writable) when its group is the user's private
  group: their primary gid, named after them, with no other member in
  `/etc/group` or `/etc/passwd`; if that cannot be read or verified (e.g.
  NSS-only accounts), addn refuses. The dir itself must be the user's
  and 0700; the DB, any `-wal`/`-shm`/`-journal` and the key file must be
  regular, the user's and 0600 or stricter. Otherwise addn refuses with
  `insecure-home`. SQLite opens the resolved path; key-file reads and writes
  go through one directory fd, and the key is published with an atomic
  no-replace rename (link fallback) under the SQLite writer lock, which is
  also held while leftover `.master.key-*` names are removed.
  On Windows the profile check runs on the nearest existing ancestor's
  resolved path before any directory is created.
- Windows (v0.1): the key lives in Credential Manager or `ADDN_MASTER_KEY`;
  `--key-file` and file-backed vaults are refused. The data dir must be
  inside `%USERPROFILE%` and relies on the profile's inherited ACLs. Private
  DACLs, and with them key files, are a follow-up.

### 2.3 OAuth on any machine

- **Default:** RFC 8252 loopback redirect on `127.0.0.1:<random port>`, PKCE S256,
  `state` validated and discarded. The listener answers 400 to any callback that is
  not for its state, or that carries both or neither of `code` and `error`, and
  accepts exactly one verified one (a code or a denial), then closes before the code
  is exchanged.
- **Headless / SSH, in two steps:** `addn connect … --no-browser` stores a pending
  authorization in the vault (state, sealed PKCE verifier, alias, scopes, redirect
  URI; 10-minute expiry) and prints `{authorize_url, expires_at, next}`. The user
  opens the URL on any device, approves, and runs `addn connect --complete '<url>'`
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
- **Stale re-consent:** a completion is refused (`stale-authorization`) if the alias
  gained scopes after the authorization started that it did not ask for.
- **Back channel:** token, userinfo and revoke requests go straight to the HTTP
  transport, so no redirect is followed, and need a 2xx; any 3xx is a refusal.
  `addn` discards the standard library's logger, so `net/http` never writes to stderr. Error messages are addn's own: the operation, the HTTP status and at
  most an RFC 6749/6750 error identifier, never the provider's text.
- **Exit codes for connect:** `2` for a bad or mismatched paste, a state mismatch,
  an unknown, used, expired, in-progress or stale authorization, or a timeout; `3`
  when the account was saved but some requested scopes were not granted
  (`scopes-not-granted`); `4` for a provider refusal (`access_denied`, a 4xx or 3xx)
  or an unusable 2xx token or identity response; `5` for the network or a 5xx.
- **Bring your own OAuth client** (v0.1): `addn client add google --file client_secret.json`.
  `addn doctor google` walks through the Google Cloud setup and warns about the
  7-day refresh-token expiry for apps left in "Testing".

### 2.4 Concurrency

Several agents may run `addn` in parallel against the same account. Token
refresh runs inside a SQLite `BEGIN IMMEDIATE` transaction that re-reads the
credential before refreshing, so two processes whose refreshes commit never
burn the same refresh token.

The guarantee covers refreshes that commit. If the process dies after the
provider rotated the refresh token but before the commit, the stored token is
stale: the next refresh gets `invalid_grant`, and the account is marked
`needs-reauth` (exit 3). It never retries in a loop.

### 2.5 Command grammar

```
addn <provider> <tool> [-a <alias>] [--param value …] [-o file] [--all]
addn call -a <alias> <METHOD> <path> [--query k=v] [--data @file.json] [-o file]
addn connect <provider> --as <alias> [--services …] [--no-browser]
addn accounts [list|show|remove|default]      addn auth check
addn tools [--provider p] [--json]            addn skill install [--harness …]
```

- Tool commands are **generated at runtime** from embedded toolkit YAML;
  `snake_case` ids become `kebab-case` commands, params become flags.
- `-a` is optional when a provider has exactly one account or a default is set.
- Exit codes: `0` ok · `1` internal (addn itself failed: stdout not writable, broken embedded data) · `2` usage/validation ·
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
events. `addn call` covers every other Google API on day one.

**First usable checkpoint is M4.** From M4 on, every merged PR is something you
can use the same day.

| # | Milestone | Ships | Usable after merge? |
|---|---|---|---|
| M0 | Repo reshape | TS/Python removed from `main`, this plan adopted, CLAUDE.md + README rewritten, Go module + cobra root with `addn version` and the JSON error envelope, CI | — |
| M1 | Catalog | YAML loader (`extends` merge: scalars override, arrays replace), embedded via `go:embed`; identity root of each provider; `effect:` tag and CLI `command:` name per toolkit; Google Drive provider + metadata toolkit; `addn providers`, `addn tools` | browse only |
| M2 | Vault | data dir, SQLite bootstrap + migrations, AES-GCM (+ `crypto_interop` conformance), master-key resolution, store API for clients / accounts / credentials / audit, `addn init` | — |
| M3 | Connect | `addn client add/list` (Desktop-app client JSON), loopback + two-step `--no-browser` / `--complete` OAuth with a pending authorization in the vault, PKCE, one-use state, identity discovery from `oauth2.identity`, adding Google services by re-consent for the scope union (persist the scopes actually granted), `addn connect / accounts / auth check` (local status only) | accounts connected |
| **M4** | **Raw call** | `addn call`: token injection, refresh under lock, 401 → retry once, 429 backoff, `-o` binary streaming | **✅ agents can hit any Google API** |
| M5 | Curated tools | tool engine (validate, path-param consumption, pagination `--all`), toolkits: gmail, drive, calendar | ✅ friendly commands |
| M6 | Files | Gmail attachment download, send-with-attachment (MIME build), Drive upload (resumable), download, Docs/Sheets export | ✅ the Gmail→Drive flow |
| M7 | Agent surface + release | `addn skill install` (SKILL.md for Claude Code / Codex / generic), `addn tools --json` schemas, goreleaser binaries (linux/mac/windows, amd64/arm64), install script, README | ✅ **v0.1.0** |

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
  instructions; a human approves with `addn approve <id>` or in a TTY prompt),
  `deny <tool>`. Every write tool in the YAML is tagged `effect: write|destructive`.
- **Providers:** GitHub, Slack (comma scopes, `ok:false` adapter — already in
  YAML), Discord (bot token), Notion.
- **API-key / bot-token auth type** next to OAuth2.
- `addn mcp` (stdio): the same catalog projected as MCP tools (the M14 schema
  mapper, ported), for harnesses that prefer MCP.
- Audit viewer: `addn log`.

### Phase 3 — "Every API" → `v0.3.0`

- **Generated tools** from OpenAPI and Google Discovery documents
  (`addn import openapi <url> --as provider`), so coverage stops depending on
  hand-written YAML. Curated YAML stays as the high-quality layer on top.
- Community provider registry: a directory of YAML files in the repo and PRs to
  add more. No hosted registry.
- **Wrappers:** `pip install addition` / `npm i @runagent/addition` ship the
  binary and expose `run(provider, tool, params) -> dict` over the JSON contract.

### Phase 4 — "All my machines" → `v0.4.0`

- `addn vault export/import` (encrypted bundle, age-style passphrase).
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

1. **Binary name:** `addn`. Go module `github.com/runagent-dev/addition`.
2. **Grammar:** gh-style. `addn gmail search-messages -a work`.
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
