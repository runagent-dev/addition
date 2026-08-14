# Addition — MCP + Collections Build Plan (v0.2.0 track)

> Companion to `docs/spec.md` and `docs/tdd-plan.md`. Those cover v0.1.0 (manual
> `executeTool`). **This plan covers the next arc**: turning the existing tool
> catalog into an *on-platform Composio/Nango* for the host app (aihubspot) —
> one declarative source of truth that, at boot, projects into **REST endpoints**
> and an **MCP server**, sliceable into **collections**, in both SDKs.
>
> Same rules as `tdd-plan.md`: Red → Green → Refactor, one PR per milestone,
> TS track then Python track, shared fixtures are the cross-language contract.

---

## 0. Why this exists / scope note

`docs/spec.md §20` lists MCP export as a **v0.3.0 non-goal** and says nothing about
REST auto-mounting or collections. This plan **supersedes that** on the user's
direction (per `CLAUDE.md §10`, user instructions beat the spec). Before M14 code
lands, `spec.md §20` and `CLAUDE.md §9` must be amended to move "MCP export",
"auto-mounted endpoints", and "collections" out of the non-goals list and into
the v0.2.0 scope. Track that as the M14 PR's first commit.

The `one` CLI is **reference only** — study its catalog/UX as a model. Addition
owns its own catalog and execution; no runtime dependency on `one`.

## 1. The thesis (one source → N projections)

```
shared/toolkits/*.yaml  ──(loaded once at boot, both SDKs)──▶  ToolkitRegistry
        │
        │   paramsToJsonSchema()   ← single pure mapper, shared by all projections
        │
        ├─▶ executeTool()   ✅ v0.1.0  (manual / programmatic)
        ├─▶ REST endpoints   ▶ M16     (one route per tool → executeTool)
        └─▶ MCP tools         ▶ M15     (tool → inputSchema → executeTool)
                                            scoped by ▶ M17 Collections
```

**Invariant for this arc:** every projection is a *pure mapping over the registry
plus a delegation to the existing `executeTool`*. No projection re-implements
validation, path interpolation, transforms, auth, or dispatch. If a projection
needs behaviour `executeTool` doesn't have, fix it in the core, not the projection.

---

## 2. Milestones

### M14 — Tool-schema projection (pure, no IO) ← **current**

The foundation both later projections consume. Turns a `ToolDefinition` into a
JSON-Schema input contract and an MCP tool descriptor. Pure functions; a golden
fixture pins the exact output so TS and Python emit **byte-identical** schemas.

**Source of truth:** `shared/test-fixtures/tool_schema_export.yaml` — expected
MCP tool descriptors for a representative spread of tools.

**Tests (`test/unit/toolschema.test.ts`)**
- `paramsToJsonSchema([])` → `{ type:'object', properties:{}, required:[], additionalProperties:false }`.
- Each `ToolParamType` maps correctly: `string→{type:'string'}`, `integer→{type:'integer'}`,
  `boolean→{type:'boolean'}`, `string[]→{type:'array',items:{type:'string'}}`, `object→{type:'object'}`.
- `description`, `enum`, and `default` are copied onto the property schema when present; absent otherwise.
- `required` params (and only those) are listed in the top-level `required` array, in declaration order.
- **Path params still appear in the input schema** — `get_message.messageId` is `{}`-interpolated at
  execution but the caller must supply it, so it is a required property here. (Pins the "input schema
  describes the *caller's* contract, not the wire placement" rule.)
- `toMcpTool('gmail', sendMessageDef)` → `{ name:'gmail__send_message', description, inputSchema }`;
  name is `${provider}__${toolId}` (double-underscore separator, reversible).
- `toolCatalog(registry)` returns one normalized entry per tool across all providers:
  `{ provider, toolId, mcpName, description, inputSchema, method, path }` (method/path parsed from `operation`).
- **Golden:** every descriptor in `tool_schema_export.yaml` deep-equals the generated output.

**Impl:** `ts/src/toolschema.ts`; export from `src/index.ts`. No changes to `executeTool`.

**Python side (M14-py):** `python/runagent_addition/toolschema.py` with `params_to_json_schema`,
`to_mcp_tool`, `tool_catalog`; `tests/unit/test_toolschema.py` drives the **same** golden fixture.
Add `tool_schema_export.yaml` to the Python conformance suite's `handled` set (its
`test_no_unhandled_fixtures` guard fails otherwise). The TS conformance loader already
skips non-scenario fixtures (post the crypto_interop fix).

### M15 — MCP server generation

Wrap M14's catalog in a real MCP server.

**Tests (`test/integration/mcp.test.ts`)**
- `listTools` returns every catalog tool as an MCP tool (name + inputSchema from M14).
- `callTool('gmail__send_message', args)` → routes to `executeTool`, returns its result mapped to MCP content.
- Unknown tool name → MCP error, not a throw that kills the session.
- Invalid args → the same `ToolValidationError` surface as `executeTool` (no second validator).
- **Tenancy:** `userId` is resolved via a host-supplied `userIdResolver(sessionCtx)`; assert the
  resolved id reaches `executeTool`. (Design decision to confirm: one multiplexed server + resolver
  vs. one server per user — default to resolver.)
- Optional `collection` filter (forward-ref to M17) narrows `listTools`.

**Impl:** `ts/src/mcp.ts` (TS: `@modelcontextprotocol/sdk`). Python: `mcp` package.

### M16 — REST endpoint generation

**Tests (`test/integration/router.test.ts`)**
- `buildRoutes(addition)` returns one descriptor per tool: `{ method, path:'/connectors/:provider/:tool', inputSchema, handler }`.
- Handler validates body against the M14 schema and calls `executeTool`; success → 200 + result, `ToolValidationError` → 400, `ConnectionNotFoundError` → 404/409.
- `userId` comes from a host-injected context extractor, never the request body.
- Framework adapters: `toExpress(routes)`, a Next.js route handler, and (Python) a FastAPI `APIRouter`.
- Optional `collection` filter mounts only that collection's tools.

**Impl:** `ts/src/router.ts` + `src/adapters-http/{express,next}.ts`. Library exposes handlers to mount; no server of its own (spec §3.1).

### M17 — Collections domain model

**Tests (`test/integration/collections.test.ts`, ephemeral Postgres)**
- Schema migration `shared/schema/002_collections.sql`: `_addition.collections` + `_addition.collection_items`, under the `_addition` namespace; bootstrap picks up `002` and bumps `schema_version` (exercises the existing forward-migration path — see `tdd-plan.md M6`).
- `createCollection`, `addToCollection(provider, toolId)`, `listCollections`, `getCollectionTools` — all scoped by `user_id`/owner (multi-tenant, spec §3.5).
- Adding a tool that isn't in any toolkit → error. Duplicate add → idempotent.
- Audit-log row written for collection mutations (invariant §6.8).

**Impl:** `ts/src/collections.ts`; `shared/schema/002_collections.sql`.

### M18 — Collection-scoped projections + conformance

**Tests**
- `createMcpServer(addition, { collection })` exposes exactly that collection's tools; likewise `buildRoutes(addition, { collection })`.
- **Conformance:** a new driver in both suites consumes `tool_schema_export.yaml` (already added in M14) and, additionally, a `collection_projection.yaml` fixture asserting that a named collection yields the same tool set and schemas in TS and Python.
- Version bump: `shared/spec.yaml`, `ts/package.json`, `python/pyproject.toml` → `0.2.0` together (invariant §6.9).

---

## 3. Cross-cutting

- **Parity is the point.** Any tool exposed via REST, MCP, or `executeTool` is the *same* definition and the *same* execution path. The golden fixtures (`tool_schema_export.yaml`, `collection_projection.yaml`) are the contract; a scenario passing in TS must pass in Python before the milestone closes.
- **No new non-goals leak in.** Still out for v0.2.0: OpenAI/Anthropic function-calling export (schema mapper makes it trivial later, but it is not a milestone here), webhooks, UI components, a hosted directory service.
- **Security:** MCP `callTool` and REST handlers run the full auth/token/proxy path unchanged — no bypass of encryption, refresh, or `user_id` scoping. `userId` is never taken from client-supplied request bodies.

---

## 4. Open decisions (resolve before the relevant milestone)

1. **MCP tenancy (M15):** host-supplied `userIdResolver` on one multiplexed server (recommended) vs. one MCP server minted per user/collection.
2. **Route shape (M16):** generic `POST /connectors/:provider/:tool` vs. explicitly registered per-tool routes with individual OpenAPI operations (recommended for a real API surface).
3. **Collection ownership (M17):** per-end-user collections vs. per-tenant/workspace collections vs. both. Drives the `collections` table's owner columns.
