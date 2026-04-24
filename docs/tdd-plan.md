# Addition — TDD-Driven Build Plan

> Companion to `docs/spec.md`. The spec defines **what** to build; this plan defines **the order in which to write failing tests, then code**, so every module lands with green tests before the next one starts.

---

## 0. TDD Ground Rules for This Project

1. **Red → Green → Refactor, per module.** No module graduates without its own test file green.
2. **Shared fixtures are the contract.** `shared/test-fixtures/*.yaml` drive conformance in both languages. A scenario added there MUST pass in TS and Python before the sprint closes.
3. **Test pyramid:**
   - **Unit** — pure functions, single file under test (crypto, providers, toolkits, transforms, adapters, tool-engine validation/interpolation).
   - **Integration** — hits a real ephemeral Postgres + a mocked HTTP layer (schema, oauth, tokens, proxy, background).
   - **Conformance** — reads `shared/test-fixtures/*.yaml`, runs the same scenario against TS and Python builds. Single source of truth for behavioral parity.
4. **Mock only at the network edge.** Don't mock `providers.ts` from `oauth.ts` — use the real registry. Do mock `fetch` / `httpx.AsyncClient` so tests are hermetic.
5. **Ephemeral Postgres for integration.** Testcontainers (TS: `@testcontainers/postgresql`, Python: `testcontainers[postgres]`). Every integration test starts with a fresh `_addition` schema bootstrap — exercises the bootstrap path itself every run.
6. **No snapshot tests on encrypted output.** Encryption has random IVs; assert *roundtrip* and *format*, never byte-level.
7. **Stop at the first red.** If a module's tests aren't green, don't start the next one — re-work the design.
8. **One commit per Red/Green pair** (optional but recommended). Makes review and bisection trivial.

### Per-language test stacks

| Concern | TypeScript | Python |
|---|---|---|
| Runner | `vitest` | `pytest` + `pytest-asyncio` |
| HTTP mocking | `msw` (node) or `vitest`'s `vi.fn()` on `globalThis.fetch` | `respx` (for `httpx`) |
| Postgres | `@testcontainers/postgresql` | `testcontainers[postgres]` |
| Time/timers | `vi.useFakeTimers()` | `freezegun` |
| Coverage | `vitest --coverage` | `pytest --cov` |

---

## 1. Shared Foundation — no tests, just data (pre-requisite)

These files are checked-in data, not code. They block all tests below, so build them first.

- [ ] Monorepo directory layout per spec §5
- [ ] `shared/providers/*.yaml` — `_template`, `google`, `gmail`, `google-calendar`, `github`, `slack`
- [ ] `shared/toolkits/*.yaml` — `gmail`, `google-calendar`, `github`, `slack`
- [ ] `shared/schema/001_initial.sql`
- [ ] `shared/spec.yaml`
- [ ] `shared/test-fixtures/{oauth_flows,token_refresh,proxy_requests,tool_execution,schema_bootstrap}.yaml`

**Exit criteria:** `yamllint` clean; `psql -f 001_initial.sql` runs green against an empty DB.

---

## 2. TypeScript Track — ordered TDD milestones

Each milestone: **write the listed tests first → watch them fail → implement → green → refactor**.

### M1 — Crypto (pure, no deps)
**Tests (`test/unit/crypto.test.ts`)**
- Encrypts then decrypts a known plaintext, gets same bytes back.
- Rejects decryption with a different 32-byte key (throws `EncryptionError`).
- Rejects tampered ciphertext (auth tag mismatch → throws).
- Output format is `base64(iv || tag || ciphertext)`; IV is 12 bytes, tag is 16.
- Two encryptions of the same plaintext produce different ciphertexts (fresh IV).
- Rejects a key that is not 64 hex chars.

**Impl:** `src/crypto.ts` using Node built-in `crypto` — `aes-256-gcm`.

### M2 — Provider Registry
**Tests (`test/unit/providers.test.ts`)**
- Loads all 5 YAMLs from `shared/providers/`.
- `getProvider('google')` returns parsed config; unknown id throws `ProviderNotFoundError`.
- `extends` resolution: `gmail` inherits `google`'s `authorize_url`, `token_url`.
- Array override semantics: `gmail.default_scopes` **replaces** (not merges with) `google.default_scopes`.
- Child wins for scalars: `gmail.api.base_url` overrides `google.api.base_url`.
- `id` and `name` always taken from the child.
- Topological sort handles `extends` regardless of file read order.
- Cycle in `extends` throws a clear error.

**Impl:** `src/providers.ts`.

### M3 — Toolkit Loader
**Tests (`test/unit/toolkits.test.ts`)**
- Loads all 4 toolkit YAMLs.
- `getToolDefinitions('gmail')` returns 5 tools; `getToolDefinition('gmail', 'send_message')` returns one.
- Unknown toolkit/tool throws `ToolNotFoundError`.
- Tool definition shape matches `ToolDefinition` type (id, name, description, params with type/required/description).
- Validates every `provider` field in a toolkit YAML matches a real provider id (fails fast on typo).

**Impl:** `src/toolkits.ts`, `src/types.ts` (tool types), `src/errors.ts`.

### M4 — Transforms (pure functions)
**Tests (`test/unit/transforms.test.ts`)**
- `gmail:rfc2822`: given `{to, subject, body}` produces `{ body: { raw } }`; base64url-decodes to a valid RFC 2822 message containing `To:`, `Subject:`, `MIME-Version`, blank line, body.
- Optional `cc`/`bcc` appear only when provided.
- Base64url uses URL-safe alphabet, no padding.
- `google-calendar:create_event_body`: maps `startDateTime`/`endDateTime`/`timeZone` into `start.dateTime`/`end.dateTime`/`timeZone`; maps `attendees` string[] into `[{email}]`.
- `google-calendar:update_event_body`: only includes fields that were passed (partial updates don't clobber).
- `getTransform('nonexistent:foo')` returns `null`.

**Impl:** `src/transforms/{gmail,google-calendar,index}.ts`.

### M5 — Adapters
**Tests (`test/unit/adapters.test.ts`)**
- `DefaultAdapter.parseTokenResponse` with `{access_token, refresh_token, expires_in}` returns standard shape; with status 400 throws `OAuthError`.
- `DefaultAdapter.isErrorResponse` is true for status 500, false for 200.
- `GitHubAdapter.beforeTokenExchange` adds `Accept: application/json`.
- `GitHubAdapter.parseTokenResponse` with `{error, error_description}` throws; with success returns `refreshToken: null, expiresIn: null`.
- `SlackAdapter.parseTokenResponse` with `{ok:false, error:"invalid_auth"}` throws; with nested `authed_user.access_token` extracts it.
- `SlackAdapter.isErrorResponse` is true for `{ok: false}` even on HTTP 200.
- `getAdapter('google')` returns a `DefaultAdapter`; `getAdapter('github')` returns `GitHubAdapter`.

**Impl:** `src/adapters/{base,github,slack,index}.ts`.

### M6 — Schema bootstrap (first integration tests)
**Tests (`test/integration/schema.test.ts`, uses testcontainers)**
- On empty DB: `bootstrap()` creates `_addition` schema + 4 tables + 3 indexes; `meta.schema_version = '001'`.
- Idempotent: calling `bootstrap()` twice is a no-op (no errors, no dup rows).
- Version check: if `meta.schema_version` is already '001', skips migration 001.
- Future-proof: if a `002_*.sql` file is added, picks it up and runs it; bumps version.
- Uses the `_addition` schema namespace, not `public`.

**Impl:** `src/schema.ts`.

### M7 — Tool engine (pure validation + orchestration with mocked proxy)
**Tests (`test/integration/tools.test.ts`)**
- Uses a stub proxy (`proxy: jest.fn()`) — no real HTTP, no DB.
- **Param validation:**
  - Missing required param → `ToolValidationError` listing the missing names.
  - Wrong type (passes `"abc"` where `integer` expected) → error.
  - Unknown enum value → error.
  - Defaults applied for omitted optional params.
- **Path interpolation:** `create_issue` with `{owner:"o", repo:"r", title:"t"}` → calls proxy with `path:/repos/o/r/issues`, body `{title:"t"}`.  `owner`/`repo` must not leak into body/query.
- **No transform:** GET → params become query; POST → params become JSON body.
- **With transform:** `send_message` result has `body.raw` base64url string.
- **Tool not found:** `executeTool('gmail', 'bogus', ...)` → `ToolNotFoundError`.
- Drives `shared/test-fixtures/tool_execution.yaml` scenarios.

**Impl:** `src/tools.ts`.

### M8 — OAuth engine
**Tests (`test/integration/oauth.test.ts`)**
- **`getAuthUrl`:**
  - Produces a URL on the provider's `authorize_url` with `client_id`, `redirect_uri`, `state`, `scope`, `response_type=code`.
  - When provider's `pkce_supported: true`, includes `code_challenge` + `code_challenge_method=S256` and stores `code_verifier` in `oauth_state`.
  - Scopes joined with the provider's `scopes_delimiter` (space vs comma for Slack).
  - Applies `authorize_params` from YAML (Google's `access_type=offline`).
  - Stores `state`, `user_id`, `provider_id`, `code_verifier`, `redirect_uri` in `_addition.oauth_state`.
  - Custom scopes override default_scopes.
- **`handleCallback`:**
  - Valid state: exchanges code at `token_url`, stores encrypted tokens, deletes state row, returns `{success: true}`.
  - Unknown/expired state → returns `{success:false, error:"invalid_state"}`.
  - Provider error code in query → returns `{success:false, error: code}`.
  - GitHub adapter path: sends `Accept: application/json` header on token exchange.
  - Slack adapter path: detects `ok:false` in 200 response and fails appropriately.
  - Writes `audit_log` row on success.
- Drives `shared/test-fixtures/oauth_flows.yaml`.

**Impl:** `src/oauth.ts`.

### M9 — Token manager
**Tests (`test/integration/tokens.test.ts`)**
- Store: encrypts access+refresh tokens before INSERT; DB row has only ciphertext.
- Retrieve: returns decrypted plaintext tokens in memory.
- Refresh: calls `token_url` with `grant_type=refresh_token`, updates row with new ciphertext + new `expires_at`.
- Refresh failure: sets `status='refresh_failed'`, emits `token.refresh_failed` event, does not clobber existing tokens.
- No-refresh-token providers (GitHub): refresh attempt is a no-op, not an error.
- Drives `shared/test-fixtures/token_refresh.yaml`.

**Impl:** `src/tokens.ts`.

### M10 — Proxy
**Tests (`test/integration/proxy.test.ts`, mocked fetch)**
- Injects `Authorization: Bearer <decrypted>` header.
- On 401 + refresh token present: refreshes once, retries once, succeeds; assert both calls made.
- On 401 + no refresh token: fails with `ConnectionExpiredError`, no retry.
- On 429: reads `Retry-After`, waits (fake timers), retries up to 3 times.
- On 200 but `isErrorResponse` returns true (Slack `ok:false`): raises, doesn't silently succeed.
- On network error: retries once after 1s.
- Connection not found: raises `ConnectionNotFoundError` immediately (no HTTP call).
- Drives `shared/test-fixtures/proxy_requests.yaml`.

**Impl:** `src/proxy.ts`.

### M11 — Background task runner
**Tests (`test/integration/background.test.ts`, fake timers)**
- `startBackgroundTasks()` schedules an interval at `tokenRefreshIntervalMs`.
- On tick: selects connections expiring within `tokenRefreshBufferMs`, refreshes each.
- Refresh failure in one connection doesn't stop the others.
- `stopBackgroundTasks()` clears the interval; no further ticks fire.
- `shutdown()` stops tasks and closes the PG pool.

**Impl:** `src/background.ts`.

### M12 — `Addition` main class (wiring + smoke tests)
**Tests (`test/integration/addition.test.ts`)**
- Constructor with invalid `encryptionKey` (not 64 hex) throws before any DB call.
- First instantiation bootstraps schema automatically.
- Event emitter: subscribers to `connection.created` / `token.refreshed` / `tool.executed` fire at the right moments.
- `configureProvider` is idempotent (calling twice with same inputs doesn't duplicate).
- `listConfiguredProviders` only returns configured ones (not every YAML).

**Impl:** `src/addition.ts`, `src/index.ts`.

### M13 — Conformance
**Tests (`test/conformance/conformance.test.ts`)**
- Loads every scenario from `shared/test-fixtures/`, runs against the built `Addition` class.
- Must pass 100% before tagging a release.

---

## 3. Python Track — mirrors TS track

Re-run the exact same milestones (M1–M13) against the Python implementation. The **shared fixtures** and **spec.yaml** are the contract — a scenario passing in TS MUST pass in Python.

Order: `crypto.py` → `providers.py` → `toolkits.py` → `transforms/` → `adapters/` → `schema.py` → `tools.py` → `oauth.py` → `tokens.py` → `proxy.py` → `background.py` → `addition.py` → conformance.

Python-specific notes:
- All public methods `async def`. Background loop via `asyncio.create_task`.
- `asyncpg` uses `$1`-style placeholders — don't port SQL verbatim from TS.
- `httpx.AsyncClient` is the HTTP client under test — use `respx` to mock.
- Encryption: `cryptography.hazmat.primitives.ciphers.aead.AESGCM` — produces the same wire format as the TS version so a token encrypted by TS can be decrypted by Python (and vice versa). Add a cross-language roundtrip test in `shared/test-fixtures/crypto_interop.yaml`.

---

## 4. Cross-Cutting Verification

Before tagging v0.1.0:

- [ ] **Conformance green in both languages** against the full `shared/test-fixtures/` suite.
- [ ] **CI workflows** (`ci-ts.yml`, `ci-python.yml`, `conformance.yml`) all green.
- [ ] **Versions match** across `shared/spec.yaml`, `ts/package.json`, `python/pyproject.toml`.
- [ ] **Example apps boot** (`examples/nextjs-app`, `examples/fastapi-app`) and complete one OAuth + one tool call end-to-end against a sandbox Google/GitHub/Slack account — treat this as a smoke-only manual gate, not an automated test.
- [ ] **Security checks:** encryption roundtrip test across languages; audit log populated for all 5 sensitive events; no plaintext tokens appear in any log output (grep the test logs).

---

## 5. Tricky Spots — Write These Tests First, Implementation Second

Five places the spec is subtle enough that TDD earns its keep:

1. **`extends` provider merging** — arrays *replace*, scalars *override*, id/name come from child. Easy to get wrong; the tests pin it down.
2. **Path interpolation** — consumed params must be stripped from the remaining payload. Write a test that asserts `owner`/`repo` do **not** appear in the request body before writing the interpolation code.
3. **Slack nested token response** — `authed_user.access_token` vs top-level `access_token`. The adapter test + a fixture make this unambiguous.
4. **401 retry with refresh** — only once, and only when a refresh token exists. Test both "refresh succeeds, original request retried" and "no refresh token, immediate failure".
5. **Schema bootstrap idempotency** — running twice must not error, must not re-run migrations. Test this explicitly rather than relying on `IF NOT EXISTS` everywhere.

---

## 6. What We Don't Test (Yet)

Per spec §20, v0.1.0 explicitly excludes SQLite, MCP export, webhooks, UI components, rate-limit persistence, and connection health checks. **Do not write tests** for these — they'll bit-rot before v0.2.0 lands. Add them in their own milestone when the feature ships.
