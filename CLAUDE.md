# CLAUDE.md — Addition

This file is loaded automatically by Claude Code when working in this repo. It captures the project's shape, ground rules, and the exact workflow we use to ship milestones. Treat these instructions as authoritative — they override defaults.

---

## 1. What Addition is

**Addition** (`addn`) is a **local-first, single-binary CLI that gives any coding agent authenticated access to the user's accounts**: several Gmail inboxes, several Drives, calendars, and later GitHub, Slack, Discord. Every credential stays on the user's machine in one encrypted SQLite file.

```bash
addn connect google --as work --services gmail,drive,calendar
addn gmail search-messages -a work --query "has:attachment newer_than:7d"
addn call -a work GET /calendar/v3/calendars/primary/events
```

It is a tool, not a platform: no server of ours, no account with us, no telemetry. Agents need only a shell. MCP and language wrappers are projections layered on top later.

**Plan and direction:** `docs/cli-plan.md` — authoritative for design, phases, milestone order, and the decisions already taken.
**SDK-era history:** the earlier TypeScript + Python embeddable SDK (Postgres, multi-tenant) lives on branch `archive/sdk-v0.1`. Its behaviour is ported, not resurrected. Do not copy its Postgres, multi-tenancy, or framework-handler code back into `main`.

---

## 2. Architecture principles (don't violate these)

1. **Local only.** No hosted component, no phoning home, no telemetry.
2. **One static binary, one data dir.** Go, pure-Go SQLite, no cgo, no runtime. All state under `$ADDN_HOME` (default XDG data dir) in one `addition.db`. The only exception is the master key: OS keychain, env, or an opted-in `master.key`.
3. **Agent-first output.** Tool commands print JSON on stdout. Errors print `{"error":{"code","message"}}` on stderr with a stable exit code. Never prompt unless stdin is a TTY.
4. **Tokens never leave the vault in plaintext.** Not in stdout, stderr, logs, errors, `--debug`, or test snapshots. There is no command that prints a token; agents reach APIs through `addn call`.
5. **Provider config is data (YAML), provider quirks are code (adapters).** Keep `if provider == "slack"` out of the core engine.
6. **One account = one identity.** A Google account connects once and serves Gmail, Drive and Calendar; more services means re-consent for the union of granted and requested scopes, not a new account.
7. **Tools as curated data.** Toolkit YAML is the source for generated commands, JSON schemas and (later) MCP tools. Commands are generated at runtime, not code-generated.

---

## 3. Repo layout

```
addition/
├── CLAUDE.md
├── README.md
├── docs/cli-plan.md        # direction + phases + milestones (authoritative)
├── go.mod                  # module github.com/runagent-dev/addition
├── cmd/addn/               # main package; thin
├── internal/               # all implementation packages (cli, catalog, vault, oauth, proxy, …)
└── shared/                 # language-agnostic data, embedded into the binary
    ├── providers/          # provider config YAML
    ├── toolkits/           # tool definitions YAML
    └── test-fixtures/      # behavioural contract carried over from the SDK era
```

---

## 4. How we work: TDD + one-PR-per-milestone

**This is a strict workflow. Do not deviate.**

### 4.1 Red → Green → Refactor, per milestone

Each milestone (M0, M1, … as listed in `docs/cli-plan.md §3`) follows the same loop:

1. **Red.** Write the failing tests for that milestone. Run them — they MUST fail for the right reason (missing package, missing function, wrong output). No implementation code yet.
2. **Green.** Write the minimum implementation needed to make them pass.
3. **Refactor.** Clean up while tests stay green. No behaviour change.

Do not mix milestones in one session. Do not start M_n+1 before M_n is merged.

### 4.2 One PR per milestone

```
M_n → tests green locally
    → commit on branch feat/m<N>-<slug> (e.g. feat/m2-vault)
    → push + open PR titled "M<N>: <milestone name>"
    → a SEPARATE reviewer reviews the PR
    → apply review fixes on the SAME branch/PR
    → reviewer approves → merge to main
    → only then start M_{n+1} on a fresh branch off main
```

**Hard rules:**
- **Stop after the PR is opened.** Wait for the user to signal "go / next / merged".
- **One milestone per branch, one branch per PR.** No stacked milestones.
- **Fixes go on the original PR branch.** Never open a follow-up PR for the same milestone.
- **main is always green.** If CI is red, stop everything and fix it.

### 4.3 Commit message style

- `test(m2): add failing vault tests` (Red)
- `feat(m2): implement sqlite vault` (Green)
- `refactor(m2): extract key resolution` (optional)

Co-authored-by Claude trailer as configured.

### 4.4 Scope discipline

Every milestone has a tight scope in `docs/cli-plan.md`. If you find something out of scope, note it in the PR description for its own milestone — do not fix it in this PR.

---

## 5. Tests

| Concern | Tool |
|---|---|
| Runner | `go test ./...` (standard `testing`; table tests) |
| HTTP / provider fakes | `net/http/httptest` — a fake provider API and a fake OAuth server |
| Storage | real SQLite in `t.TempDir()`, starting empty and running the real bootstrap every time |
| CLI | run the cobra root in-process with captured stdout/stderr; assert JSON, not prose |
| Time | inject a clock; never `time.Sleep` in tests |

- **Mock only at the network edge.** Fake the HTTP server, never internal packages against each other.
- **Fixtures in `shared/test-fixtures/` are the contract** that the Go port behaves like the SDK. A Go conformance driver consumes them.
- **No-leak tests:** any command touching credentials gets a test asserting no token string appears in stdout, stderr or debug output.

---

## 6. Core invariants (must never silently break)

1. **Encryption wire format:** `base64(iv[12] || tag[16] || ciphertext)`, AES-256-GCM, 32-byte key parsed from 64 hex chars. `crypto_interop.yaml` pins it.
2. **Master key never silently downgrades.** Resolution: `ADDN_MASTER_KEY` → OS keychain → key file only if opted in at `addn init --key-file`. Otherwise fail loudly.
3. **`extends` merge semantics:** scalars override, arrays **replace**, `id` and `name` come from the child.
4. **Path interpolation:** params named in `{}` are **consumed** — stripped from body/query.
5. **401 retry:** exactly once, and only when a refresh token exists. Never loop.
6. **Refresh under lock:** concurrent `addn` processes refreshing one account hit the token endpoint once (SQLite `BEGIN IMMEDIATE`, re-read before refresh). A rotation lost to a crash before commit ends in `needs-reauth`, never a retry loop.
7. **OAuth:** PKCE S256, `state` validated and discarded after use; loopback redirect on `127.0.0.1`, with a paste-back path for headless machines.
8. **Slack non-standard errors:** HTTP 200 with `ok: false` is a failure, detected by the Slack adapter, not core.
9. **Scopes delimiter:** read from YAML (space for most, comma for Slack). Never hardcode.
10. **Token extraction is data:** `oauth2.token_response_mapping` dot-paths, else the standard OAuth2 layout. Adapters may reject error shapes but never extract tokens.
11. **Audit log:** connect / refresh / revoke / tool-call events write a row; never token values.
12. **Output contract:** stdout is JSON for tool commands; errors are the JSON envelope on stderr with exit codes `0` ok · `1` output · `2` usage · `3` needs (re)connect · `4` upstream 4xx · `5` upstream 5xx/network · `6` policy denied.

---

## 7. Dev commands

```bash
go test ./...                 # all tests (older local Go fetches the pinned toolchain)
go vet ./...
"$(go env GOROOT)/bin/gofmt" -l .   # must print nothing
go run ./cmd/addn version
go build -o addn ./cmd/addn
```

---

## 8. Security checklist (per PR)

- [ ] No plaintext tokens or client secrets in output, logs, errors, or test snapshots.
- [ ] Encryption roundtrip tested for code touching the vault.
- [ ] OAuth `state` validated and deleted after use (for code touching OAuth).
- [ ] Audit row written for each sensitive event the PR touches.
- [ ] No new network destination other than the provider APIs the user connected.

---

## 9. Non-goals (until their phase in `docs/cli-plan.md`)

- **Phase 1 excludes:** MCP, policies, non-Google providers, API-key auth, sync, language wrappers, OpenAPI import.
- **Never:** a hosted service, telemetry, a shared OAuth client we operate (revisit only by explicit decision).

If a milestone surfaces one of these as "nice to have", note it in the PR description as follow-up — do not implement.

---

## 10. References

- `docs/cli-plan.md` — direction, architecture, phases, decisions.
- `shared/test-fixtures/` — behavioural contract.
- `archive/sdk-v0.1` branch — the SDK implementation the Go port is based on.

When user instructions and the plan disagree: **user instructions win**. Flag the disagreement before coding.
