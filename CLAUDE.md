# CLAUDE.md — Addition

This file is loaded automatically by Claude Code when working in this repo. It captures the project's shape, ground rules, and the exact workflow we use to ship milestones. Treat these instructions as authoritative — they override defaults.

---

## 1. What Addition is

**Addition** is an *embeddable integration runtime* — a library (npm / PyPI), not a platform. A developer installs it into their own app (Next.js, FastAPI, Express, etc.), gives it a database connection string, and it handles:

- OAuth flows (authorize URL, callback, PKCE, state)
- Encrypted token storage + automatic refresh
- Authenticated API proxying (credential injection, 401/429 retry)
- Curated, typed "tools" for each provider (e.g. Gmail `send_message`, GitHub `create_issue`)
- Multi-tenant isolation by `user_id`

**No separate server. No Redis. No workers.** The developer's application process and Postgres are the entire stack. Addition bootstraps its own `_addition` schema in the app's existing database.

**v0.1.0 provider set:** Google (base) → Gmail, Google Calendar; GitHub; Slack.

Two consumption modes:
- **Manual:** `addition.executeTool({ userId, providerId, toolId, params })` — the first-class mode in v0.1.0.
- **Agentic (future):** same tool definitions exposed as MCP / OpenAI function-calling schemas. Deferred to v0.3.0.

**Full architectural spec:** `docs/spec.md` — authoritative.
**TDD build plan:** `docs/tdd-plan.md` — authoritative for the order of work and what tests to write.

---

## 2. Architecture principles (from spec §3 — don't violate these)

1. **Library, not a service.** No HTTP server of our own; we expose a callback *handler* the developer mounts in their framework.
2. **Database is the only infra.** Postgres (v0.1.0) under schema `_addition`.
3. **Self-bootstrapping schema.** On first instantiation, `Addition` creates tables and records `schema_version` in `_addition.meta`. Developers never run a migration command.
4. **Provider config is data (YAML), provider quirks are code (adapters).** Keep branches (`if provider === 'slack'`) out of the core engine — isolate them in adapter classes.
5. **Multi-tenancy via `user_id`.** Every connection, every query, every proxy call is scoped by a caller-supplied `user_id` string.
6. **Tools as curated data.** We do NOT expose every endpoint; we expose 5–10 high-value actions per provider with friendly params.
7. **Format-agnostic tool definitions.** Metadata is rich enough to structurally convert to MCP / OpenAI / Anthropic tool schemas later.

---

## 3. Repo layout

```
addition/
├── CLAUDE.md                 # this file
├── README.md
├── docs/
│   ├── spec.md               # architectural spec (authoritative)
│   └── tdd-plan.md           # TDD milestone plan (authoritative for work order)
├── shared/                   # language-agnostic source of truth
│   ├── providers/            # provider config YAML (google, gmail, github, slack, ...)
│   ├── toolkits/             # tool definitions YAML per provider
│   ├── schema/               # raw SQL migrations (001_initial.sql, ...)
│   ├── test-fixtures/        # cross-language scenarios (oauth, tokens, proxy, tools)
│   └── spec.yaml             # public API contract for both languages
├── ts/                       # TypeScript implementation (@runagent/addition)
├── python/                   # Python implementation (runagent-addition)
└── examples/                 # Next.js + FastAPI reference apps
```

Nothing in `shared/` is language-specific. Both `ts/` and `python/` read it at runtime/test time. Changes to `shared/` must be matched in both language tracks.

---

## 4. How we work: TDD + one-PR-per-milestone

**This is a strict workflow. Do not deviate.**

### 4.1 Red → Green → Refactor, per milestone

Each milestone (M1, M2, ... as listed in `docs/tdd-plan.md §2`) follows the same loop:

1. **Red.** Write the failing tests named in the plan for that milestone. Run them — they MUST fail for the right reason (missing module, missing export). Do not write implementation code yet.
2. **Green.** Write the minimum implementation needed to make the tests pass. Nothing more. No features the tests don't demand.
3. **Refactor.** Clean up while tests stay green. No behavior change.

Do not mix milestones in one session. Do not start M_n+1 before M_n is merged.

### 4.2 One PR per milestone

```
M_n → tests green locally
    → commit on branch feat/m<N>-<slug> (e.g. feat/m1-crypto-ts)
    → push + open PR titled "M<N>: <milestone name>"
    → a SEPARATE reviewer reviews the PR
    → apply review fixes on the SAME branch/PR (do not open a new one)
    → reviewer approves → merge to main
    → only then start M_{n+1} on a fresh branch off main
```

**Hard rules:**
- **Stop after the PR is opened.** Don't speculatively start the next milestone. Wait for the user to signal "go / next / merged".
- **One milestone per branch, one branch per PR.** No stacked milestones.
- **Fixes go on the original PR branch.** Never open M1-v2 or a followup PR for the same milestone.
- **The main branch is always mergeable and green.** If CI red, stop everything and fix.

### 4.3 Commit message style

Small, Red/Green scoped commits beat one giant commit:
- `test(m1): add failing crypto unit tests` (Red)
- `feat(m1): implement aes-256-gcm crypto` (Green)
- `refactor(m1): extract iv/tag constants` (Refactor — optional)

Co-authored-by Claude trailer as configured.

### 4.4 Scope discipline

Every milestone has a tight scope listed in `docs/tdd-plan.md`. If you discover something out-of-scope during implementation (e.g. you're building M1-crypto and notice a gap in providers.ts), **file it as a note for its own milestone — do NOT fix it in this PR**. Scope creep breaks the reviewer loop.

---

## 5. Test stacks

| Concern | TypeScript (`ts/`) | Python (`python/`) |
|---|---|---|
| Runner | `vitest` | `pytest` + `pytest-asyncio` |
| HTTP mocking | `msw` or `vi.fn()` on `globalThis.fetch` | `respx` (for `httpx`) |
| Postgres | `@testcontainers/postgresql` | `testcontainers[postgres]` |
| Time / timers | `vi.useFakeTimers()` | `freezegun` |
| Coverage | `vitest --coverage` | `pytest --cov` |

**Test directories:**
- `ts/test/unit/` — pure, no IO
- `ts/test/integration/` — ephemeral Postgres + mocked HTTP
- `ts/test/conformance/` — drives `shared/test-fixtures/*.yaml`
- Python mirrors this under `python/tests/{unit,integration,conformance}/`.

**Mocking rule:** mock only at the network edge (`fetch` / `httpx`). Do not mock internal modules against each other — that's how interface drift hides.

**Ephemeral Postgres:** every integration test starts from an empty DB and runs the real schema bootstrap. That way the bootstrap code path is exercised on every test run — never stub it.

---

## 6. Core invariants (things that must never silently break)

These are the things to check first when something feels off, and the things that should have direct tests pinning them down:

1. **Encryption wire format:** `base64(iv[12] || tag[16] || ciphertext)`, AES-256-GCM, 32-byte key parsed from 64 hex chars. TS-encrypted blob must decrypt in Python and vice versa — there is a cross-language interop test for this.
2. **Schema namespace:** everything lives under `_addition.*`. Never `public.*`.
3. **`extends` merge semantics (providers):** scalars override, arrays **replace** (do not merge), `id` and `name` always come from the child.
4. **Path interpolation:** params named in `{}` in a tool operation are **consumed** — stripped from the request body/query. Write the assertion that they do NOT appear in the body before writing the interpolation code.
5. **401 retry:** exactly once, and only when a refresh token exists. Never loop.
6. **Slack non-standard errors:** HTTP 200 with `ok: false` is a failure; detected by `SlackAdapter.isErrorResponse`, not core.
7. **Scopes delimiter:** space for most providers, **comma for Slack**. Read from the YAML, never hardcode.
8. **Audit log:** every OAuth / token / connection / tool event writes a row to `_addition.audit_log`. Tokens never appear in logs in plaintext.
9. **Spec version:** `shared/spec.yaml` version, `ts/package.json` version, `python/pyproject.toml` version are always equal. Bump together.

---

## 7. Dev commands

### TypeScript (`ts/`)
```bash
cd ts
npm install
npm test                       # vitest, watch mode
npm run test:ci                # single run with coverage
npm run build                  # tsc → dist/
```

### Python (`python/`)
```bash
cd python
uv sync
uv run pytest
uv run pytest tests/unit/      # just unit tests
uv run pytest -k crypto        # single suite
```

### Shared validation
```bash
# YAML lint
yamllint shared/

# Re-run conformance in both languages
(cd ts && npm run test:conformance) && (cd python && uv run pytest tests/conformance/)
```

---

## 8. Security checklist (per PR)

Before marking any PR ready for review:

- [ ] No plaintext access/refresh tokens in logs, errors, or test snapshots.
- [ ] Encryption roundtrip tested (for any code that touches `crypto.ts`).
- [ ] OAuth `state` is validated and deleted after use (for any code touching `oauth.ts`).
- [ ] No `client_secret` appears unencrypted anywhere except in-memory during OAuth exchange.
- [ ] Audit log row written for each sensitive event the PR touches.

---

## 9. What NOT to build (v0.1.0 non-goals — per spec §20)

Do not add code or tests for these until they have their own milestone:

- SQLite support (v0.2.0)
- MCP / OpenAI function-calling export (v0.3.0)
- Webhooks / triggers
- UI components (`<ConnectButton>`, etc.)
- Rate-limit persistence across restarts
- Connection health checks

If a milestone surfaces one of these as "nice to have", note it in the PR description as follow-up — do not implement.

---

## 10. References

- `docs/spec.md` — full architectural spec. When in doubt, this wins.
- `docs/tdd-plan.md` — ordered milestones with the exact test cases to write per milestone.
- `shared/spec.yaml` — the public API contract both languages must satisfy.
- `shared/test-fixtures/` — the cross-language conformance contract.

When user instructions, spec, and plan disagree: **user instructions win**, then spec, then plan. Flag the disagreement before coding.
