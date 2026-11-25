# Addition UI — Direction & Build Plan

> Status: **accepted 2026-11-25.** Extends `docs/cli-plan.md`; where the two differ on the UI, the call log or
> `addition log`, this file wins. Remote access beyond what §7 lists is not decided yet.

---

## 1. What we're adding

`addition ui` starts a local web UI over the same vault the CLI uses. People who would rather click than type get
every tool; everyone gets a page showing what their agents did.

- **One binary, two faces.** The UI is embedded in the `addition` binary. No second install, no runtime, no cgo.
- **One database.** CLI and UI both work on `addition.db`. A connection made in the UI is there in the CLI, and the
  other way round. The CLI never needs the UI to be running.
- **Distribution like T3 Code.** `npx @addition/cli` runs a small shim that fetches the released binary and verifies its
  checksum (§9). With no arguments it opens the UI. `npm i -g @addition/cli` puts `addition` on `PATH` for agents.
- **Everything agents did is visible to the user and hidden from the agents** (§5, §6).

Still true, and unchanged: no hosted component, no telemetry, no account with us, tokens never leave the vault in
plaintext.

## 2. Decisions

| # | Decision |
|---|---|
| 1 | The UI is a React + Vite + TypeScript single-page app over a versioned JSON API (`/api/v1`). Few dependencies, a pinned lockfile, npm only at build time; the binary embeds static files. |
| 2 | Every `addition call` (and every curated tool, which goes through it) is recorded in full for audit: request and response, headers and bodies, success or failure. Tokens are never recorded. |
| 3 | A call is recorded **before** it is sent. If the record cannot be written, the call is not sent. |
| 4 | The record can be read only by a human: `addition log` and `addition ui` need a terminal on stdin. |
| 5 | Retention has a default (90 days) and is configurable from the UI and the CLI, by a human only. |
| 6 | Agent attribution has two columns: the name the caller claims (`ADDITION_AGENT`) and what addition observed (working directory, parent process). |
| 7 | The UI is responsive from the start, installable as a PWA, and reachable remotely through SSH forwarding or Tailscale. |
| 8 | Desktop: browser first. A native shell (Tauri) is a later, optional milestone; it loads the same local URL. |
| 9 | `npx @addition/cli` with no arguments opens the UI. |

## 3. Architecture

```
npx @addition/cli ─► shim (.mjs) ─► downloads + checks sha256 ─► addition binary (Go)
                                                                   │
                   ┌───────────────────────────────────────────────┴───────────┐
             CLI (agents, scripts)                                    addition ui (people)
             separate process per command                             127.0.0.1 server + embedded SPA
                   └──────────────────► addition.db (WAL) ◄────────────────────┘
```

- **Service layer first (U0).** Connect, call, accounts, doctor, search and the rest currently live in `internal/cli`.
  They move behind a service package that returns typed results; cobra commands and the HTTP handlers are both thin
  callers. No behaviour change, all existing tests stay green. This is the milestone that makes everything after it
  small.
- **JSON API is the contract.** `/api/v1` is what the SPA calls, and what a desktop shell or later clients can call.
  Handlers have `httptest` tests like the rest of the repo; the SPA is tested against a fake API.
- **Live updates.** The server watches `PRAGMA data_version` and pushes events over SSE, so a call made by an agent
  shows up on the Activity page while it is open, and a change in the UI shows up in the next CLI command.
- **No token endpoint.** The API has no route that returns a token. The "Try it" console uses the same proxy engine
  as `addition call`, so the host allow-list, path rules and echo guard apply unchanged.
- **Layout:** `ui/` holds the SPA source; its build output is embedded from `internal/uiserver`. The Go packages
  keep building and testing without Node when the embedded assets are absent (a stub page), so `go test ./...` needs
  no JS toolchain.

## 4. The UI server

- Binds `127.0.0.1` on a random port (`--port` to fix it). Nothing else listens by default.
- Each launch makes a fresh session secret. The first request carries a single-use code that expires in a few
  minutes; it is exchanged for an `HttpOnly`, `SameSite=Strict` cookie (`Secure` over HTTPS).
- Every request checks `Host` against an allow-list and `Origin` on state-changing requests (DNS rebinding, CSRF).
  No CORS headers.
- Idle timeout ends the session; stopping `addition ui` stops everything.
- A second `addition ui` finds the first through a lock in the data dir and opens its page instead of starting
  another server.
- The server takes a session secret from its launcher; it does not decide who is human. That check lives in the
  `addition ui` command (§5), so a desktop shell can launch the server later with its own secret and nothing in
  the server changes.
- The first-run wizard runs the same steps as `addition init` through the service layer.

## 5. Human-only access

`addition log` (every subcommand) and `addition ui` refuse to run unless **stdin is a terminal**. Otherwise they exit
`6` with `{"error":{"code":"human-only","message":"…"}}`.

- Shells that agent harnesses give their tools usually have no terminal on stdin, so an agent that runs
  `addition log` or `addition ui` is refused. A person at a real terminal (including a phone SSH app, which
  allocates a pty) is not, and `addition log | jq` works because stdin is still the terminal.
- `addition ui` prints its link to the terminal itself (`/dev/tty`), never to stdout, with a QR code when the
  terminal can show one. There is no `--no-open` mode that prints a URL for a script to read.
- Changing retention, `purge` and `export` sit behind the same gate.

**Limit.** This is a guard against mistakes and prompt injection, not against an agent that is willing to defeat
it (allocating a pty, or reading `$ADDITION_HOME` and the keychain directly as the same OS user). Stopping that needs
OS-level separation: the agent runs as another user or in a sandbox without the data directory and keychain.
`addition doctor` will say so. A stricter mode that asks for a vault passphrase to read the log is a possible later
milestone; it does not exist in the current master-key model.

## 6. The call log

Today `audit_log` keeps, per call, the method, status, account id and provider. That cannot answer "what did the
agent do". The call log can.

### 6.1 What is stored

Plain columns, so filtering needs no decryption: call id, event kind, account id, provider, operation id (when it
maps to one), method, host, status, duration, request and response byte counts, truncation flags, claimed agent
name, timestamp, and the hash chain fields.

One sealed blob per event (AES-256-GCM, AAD binding vault id, table, row id and column, as for every vault secret):
path, query, request and response headers and bodies, working directory, parent process name and pid, and the error
text for a failure.

- **Never stored:** the `Authorization` header or any token. The request is recorded as the caller sent it, before
  addition attaches a token. A token string in a response is removed the same way the echo guard removes it.
  Token-endpoint exchanges (refresh, code exchange) are not recorded with bodies.
- **Binary and large bodies:** a body written to disk with `-o` is not copied into the log: size and sha256 only.
  Text bodies are capped (default 1 MiB per body) and marked truncated.

### 6.2 Every attempt is recorded, in order

The log is append-only. A call writes `started` before anything leaves the machine, then one closing event:

| What happened | Events |
|---|---|
| Response received (any status) | `started`, `finished` with the status |
| Connection failed or timed out | `started`, `failed` with the reason |
| Refused by addition (host not allowed, path rule, policy) | `refused` with the reason; nothing was sent |
| Process died or the output pipe closed first | `started` only; shown as incomplete |

If `started` cannot be written (disk full, database locked past its deadline), `addition call` sends nothing and
fails with code `log-unavailable`, exit `1`. Cancellation still never drops a refresh-token rotation (invariant 6);
the call log never blocks a refresh, only the API request that follows it.

### 6.3 Tamper evidence and retention

- Each row stores `prev_hash` and `row_hash` over its plain columns and sealed bytes, so `addition log verify`
  detects a changed, removed or reordered row without decrypting anything.
- Retention defaults to 90 days. `addition log retention set <days>`, `addition log purge --before <date>` and the
  Settings page change it; all are human-only. A purge writes a `purged` row holding the hash of the last row it
  removed, so the chain still verifies from there.

### 6.4 Who called

`ADDITION_AGENT=claude-code` (or any name) is stored as the **claimed** name. The log shows it as claimed because the
caller can set anything. Working directory and the parent process name are recorded as **observed**, best effort
(not available on every platform). Nothing leaves the machine.

### 6.5 `addition log`

JSON on stdout, like every tool command, behind §5's gate:

```
addition log                         # newest first; --app --account --status --since --until --agent --limit
addition log --follow                # live tail
addition log show <call-id>          # the whole request and response
addition log verify                  # walks the hash chain
addition log retention [set <days>]
addition log purge --before <date>
addition log export -o file.jsonl
```

The `export` file is written with mode `0600` and contains the sealed content in plaintext; it is the user's file,
and the command says so.

## 7. Remote access

The UI is for your own machine, reached by your own route. Addition runs no relay, tunnel or hosted service.

- **SSH forwarding:** `ssh -L 8787:127.0.0.1:8787 host`, then `http://localhost:8787` in a browser on the other
  device. A phone SSH app that supports port forwarding works. The UI never listens on the network.
- **Tailscale:** preferred is `tailscale serve`, which keeps the UI on loopback and adds HTTPS and tailnet-only
  access. `--host <tailscale-ip>` works too and needs `--allow-host <name>` for the `Host` check.
- **Login** is the single-use link of §4, printed to the terminal where the vault lives. A phone SSH session has a
  pty, so `addition ui` runs there.
- **Phone:** the layout is responsive from the first page. Over HTTPS or `localhost` the UI installs as a PWA.

Not planned in this phase: a public endpoint, a third-party tunnel, a relay, a native phone app, or syncing the vault
between machines (Phase 4). A relay or any hosted component conflicts with `CLAUDE.md` §2 and §9 and would need an
explicit decision to change them.

## 8. Pages

| Page | Content |
|---|---|
| Overview | the `addition doctor` report: vault, master-key source, clients, account health |
| Accounts | add an OAuth client (upload `client_secret.json`), connect per app (the UI hosts the OAuth callback), token connections, check, remove |
| Activity | the call log (§6): filters, live tail, one call's full request and response, incomplete and refused calls marked |
| Tools | `search` / `describe`, and "Try it" forms generated from the operation schemas |
| Guide | the embedded recipes |
| Settings | retention, allowed hosts, idle timeout |

## 9. Packaging

- **npm shim (M7c).** `@addition/cli` is a zero-dependency Node script: it picks the archive for the platform from
  the release, checks it against `checksums.txt`, unpacks it beside a completion marker, runs `addition --version`
  to validate it, publishes it with an atomic rename, then runs it with the caller's arguments and stdio. With no
  arguments it runs `addition ui`. Installs are cached under `$ADDITION_HOME/runtime/versions/<version>`. This is
  the same shape T3 Code's `t3` package uses.
- **Desktop.** Level 1: the browser. Level 2: a browser app window, where the OS offers one (a courtesy, not a
  milestone). Level 3: an installable app with a tray icon, autostart and updates, built as a Tauri shell that starts
  the Go binary and loads its local URL.
- **Cost.** Levels 1 and 2 cost nothing. At level 3, Linux packages are free; Windows can ship unsigned (a
  SmartScreen warning) or use a free open-source signing programme if we qualify; macOS has no free notarization
  (the Apple Developer Program is about $99 a year), and an unsigned app asks the user to right-click → Open. The
  CLI binary and the npm shim need none of this.

## 10. Milestones

Each milestone keeps the usual loop (Red → Green → Refactor, one PR, stop after it opens). The npm shim comes first
because it needs no UI and gives the `npx` entry point from day one.

| # | Milestone | Ships |
|---|---|---|
| M7c | npm shim | `@addition/cli` as in §9 (PyPI and Homebrew follow as already planned) |
| U0 | Service layer | extract the logic out of `internal/cli`; no behaviour change |
| U1 | Call log + `addition log` | migration, write-ahead recording in the proxy path, redaction, caps, hash chain, `verify`, retention, human-only gate, `ADDITION_AGENT`; `CLAUDE.md` invariant 11 rewritten; guide topic |
| U2 | `addition ui` | server, session and Host/Origin checks, embedded SPA shell, SSE, Overview page, responsive layout, PWA manifest, `--host` / `--allow-host`, single-instance lock |
| U3 | Accounts | clients, connect, token connections, remove, first-run wizard |
| U4 | Activity | the call-log page and live tail |
| U5 | Tools and Guide | explorer, "Try it", recipes |
| U6 | Desktop shell (optional) | Tauri shell, tray, autostart, installers, signing as §9 allows |

## 11. Changes to the repo's rules

These land with the milestone that makes them true, not before:

- **U1:** `CLAUDE.md` invariant 11 becomes: *audit_log holds token-free metadata (connect, refresh, revoke);
  `call_log` holds every call in full, sealed, append-only, hash-chained, never a token, readable only by a human at a
  terminal.* The "no token in logs" part of invariant 4 is unchanged and gets a test over `call_log`.
- **U1:** the output contract's exit `6` also covers `human-only`.
- **U2:** repo layout gains `ui/` and `internal/uiserver/`; the security checklist gains the UI-server items of §4.

## 12. Tests

- **No-leak:** no token string in `call_log` rows (plain or sealed, after decryption), UI responses, SSE events,
  `addition log` output or `export` files.
- **Every attempt:** success, 4xx, 5xx, network error, timeout, refusal and a killed process each leave the events §6.2
  lists; a failing log write sends no request (fake provider sees none).
- **Tamper:** editing, deleting or reordering a row makes `verify` fail; a purge keeps it passing.
- **Human-only:** `addition log` and `addition ui` exit 6 without a terminal on stdin and work with one (a pty in the
  test).
- **Server:** Host and Origin checks, single-use link, expired code, cookie flags, idle timeout, second instance.
- **Encryption:** the sealed blob roundtrips and is bound to its row (moving it to another row fails to open).
- **Live:** a call from another process appears through SSE.

## 13. Open questions

- Remote access beyond §7 (the user has a different plan to share).
- Stricter log reading (vault passphrase), and OS-level separation guidance for agents.
- Whether level 3 is built at all, decided after the browser UI is in use.
