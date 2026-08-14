# Addition — Embeddable Integration Runtime

> **What is this document?** This is the complete architectural specification for Addition. It contains every design decision, data format, code structure, and implementation detail needed to build the project from scratch. A coding agent should be able to read this document alone — with no additional context — and produce a working implementation.

---

## Table of Contents

1. [Vision](#1-vision)
2. [Package Identity](#2-package-identity)
3. [Architecture Principles](#3-architecture-principles)
4. [Conceptual Model](#4-conceptual-model)
5. [Monorepo Structure](#5-monorepo-structure)
6. [Layer 1 — Provider Registry](#6-layer-1--provider-registry)
7. [Layer 2 — Provider Adapters](#7-layer-2--provider-adapters)
8. [Layer 3 — Toolkit Definitions (Tools)](#8-layer-3--toolkit-definitions-tools)
9. [Layer 4 — Transforms](#9-layer-4--transforms)
10. [Layer 5 — Tool Engine](#10-layer-5--tool-engine)
11. [Database Schema](#11-database-schema)
12. [API Contract (spec.yaml)](#12-api-contract-specyaml)
13. [Test Fixtures](#13-test-fixtures)
14. [TypeScript Implementation](#14-typescript-implementation)
15. [Python Implementation](#15-python-implementation)
16. [Security Design](#16-security-design)
17. [Developer Experience: End to End](#17-developer-experience-end-to-end)
18. [Scaling: Adding a New Provider](#18-scaling-adding-a-new-provider)
19. [Future: Exposing Tools as MCP / Function Calling](#19-future-exposing-tools-as-mcp--function-calling)
20. [Development Roadmap](#20-development-roadmap)
21. [Implementation Order](#21-implementation-order)
22. [Key Implementation Notes](#22-key-implementation-notes)
23. [Reference Codebases](#23-reference-codebases)

---

## 1. Vision

Addition is a **library, not a platform**. You `npm install` or `pip install` it, give it a database connection string, and it handles:

- **OAuth flows** — authorization URL generation, callback handling, PKCE
- **Token management** — encrypted storage, automatic refresh, expiry tracking
- **Authenticated API proxying** — credential injection, retry on 401/429, rate limiting
- **Curated tool definitions** — typed, validated, high-level actions for each provider (e.g., "send email", "create issue", "list events")
- **Multi-tenant isolation** — all operations scoped by a `user_id` you provide

No separate server. No Docker containers. No Redis. No message queues. Your application process and your database are the entire stack.

Addition is designed for developers building multi-tenant applications (SaaS products, AI agents, automation tools) who need their end-users to connect third-party accounts (Gmail, Slack, GitHub, Google Calendar, etc.) without depending on a third-party integration platform.

The library has two consumption modes:
- **Manual integration** — the developer calls specific tools programmatically (`addition.executeTool(...)`)
- **Agentic integration** (future layer on top) — tool definitions exported as MCP tools, OpenAI function-calling schemas, or any other format, so an LLM agent can discover and invoke them

This spec covers manual integration as the first-class mode. The tool definitions are designed in a format that makes future agentic exposure trivial (structural conversion, no semantic work).

---

## 2. Package Identity

| Item | Value |
|------|-------|
| Project name | Addition |
| GitHub repository | `runagent-addition` (or `runagent/addition`) |
| npm package | `@runagent/addition` |
| PyPI package | `runagent-addition` |
| TypeScript import | `import { Addition } from '@runagent/addition'` |
| Python import | `from runagent_addition import Addition` |
| Database schema namespace | `_addition` |
| Environment variable prefix | `ADDITION_` |
| License | MIT |

---

## 3. Architecture Principles

1. **Library, not a service.** Addition runs inside your application process. It does not start its own HTTP server. It does not require a separate deployment.

2. **Database is the only infrastructure.** All state — provider configs, user connections, encrypted tokens, OAuth flow state, migration version — lives in your existing PostgreSQL (production) or SQLite (development/testing) database under the `_addition` schema.

3. **Self-bootstrapping schema.** On first instantiation, Addition checks for the `_addition` schema. If absent, it creates all tables automatically. On subsequent starts, it checks a version record and runs any pending migrations. The developer never runs a migration command manually.

4. **One callback route, mounted in your framework.** OAuth requires a callback URL. Addition does not spin up a server for this. Instead, it exposes a handler function that you mount as a single route in your existing web framework (Next.js, Express, FastAPI, Django, etc.).

5. **In-process background tasks.** Token refresh and other periodic work run as lightweight background tasks (Node.js `setInterval` / Python `asyncio.create_task`) inside your application's existing event loop. No cron jobs, no worker processes, no Redis queues.

6. **Provider registry as data, quirks as code.** Each provider's configuration (URLs, scopes, rate limits) lives in a YAML file. Provider-specific behavioral quirks (non-standard token responses, special headers) live in a small adapter code file. Config is data; behavior is code.

7. **Multi-tenancy via user_id.** Every connection is scoped to a `user_id` string that you provide (your app's user UUID, org ID, or any identifier). All operations are scoped by this ID. Connections are fully isolated between user IDs.

8. **Tools as curated data.** Each provider exposes a set of curated, high-level tools defined in YAML. These are not raw OpenAPI endpoints — they are developer-friendly actions with friendly parameter names, descriptions, and optional transforms that translate friendly inputs into the provider's actual API format.

9. **Format-agnostic tool definitions.** Tool definitions contain enough metadata (name, description, parameter schemas, return types) to be structurally converted into any tool-calling format (MCP, OpenAI functions, Anthropic tools, JSON Schema) without semantic changes.

---

## 4. Conceptual Model

Addition has five layers, each building on the one below:

```
┌─────────────────────────────────────────────────────┐
│  Layer 5: Tool Engine                                │
│  Validates params → applies transforms → calls proxy │
│  → parses response → returns structured result       │
├─────────────────────────────────────────────────────┤
│  Layer 4: Transforms                                 │
│  Converts friendly params to API-specific formats    │
│  (e.g., {to, subject, body} → base64 RFC 2822 MIME) │
├─────────────────────────────────────────────────────┤
│  Layer 3: Toolkit Definitions                        │
│  Curated YAML: which actions each provider exposes,  │
│  with friendly names, param schemas, descriptions    │
├─────────────────────────────────────────────────────┤
│  Layer 2: Provider Adapters                          │
│  Small code files handling behavioral quirks per     │
│  provider (non-standard responses, special headers)  │
├─────────────────────────────────────────────────────┤
│  Layer 1: Provider Registry + Auth + Proxy           │
│  YAML config, OAuth engine, token manager,           │
│  encrypted storage, authenticated proxy              │
└─────────────────────────────────────────────────────┘
```

A developer can use any layer directly:
- **Layer 1 only**: Raw proxy — `addition.proxy({ userId, providerId, method, path, params })` — full control, you know the API
- **Layer 5**: Tools — `addition.executeTool({ userId, providerId, toolId, params })` — friendly, validated, transformed

Both modes share the same auth, token management, and proxy infrastructure underneath.

---

## 5. Monorepo Structure

```
runagent-addition/
│
├── README.md
├── LICENSE                             # MIT
├── .github/
│   └── workflows/
│       ├── ci-ts.yml
│       ├── ci-python.yml
│       └── conformance.yml             # Verifies both implementations match spec.yaml
│
├── shared/                             # ⭐ Language-agnostic source of truth
│   │
│   ├── providers/                      # Layer 1: Provider config (YAML)
│   │   ├── _template.yaml
│   │   ├── google.yaml                 # Base Google OAuth config
│   │   ├── gmail.yaml                  # Extends google
│   │   ├── google-calendar.yaml        # Extends google
│   │   ├── github.yaml
│   │   └── slack.yaml
│   │
│   ├── toolkits/                       # Layer 3: Tool definitions (YAML)
│   │   ├── _template.yaml
│   │   ├── gmail.yaml                  # send_message, list_messages, get_message, search_messages
│   │   ├── google-calendar.yaml        # list_events, create_event, update_event, delete_event
│   │   ├── github.yaml                 # list_repos, create_issue, list_issues, create_pr, list_prs
│   │   └── slack.yaml                  # send_message, list_channels, list_messages
│   │
│   ├── schema/                         # Database migrations (raw SQL)
│   │   ├── 001_initial.sql
│   │   └── README.md
│   │
│   ├── test-fixtures/                  # Shared test scenarios
│   │   ├── oauth_flows.yaml
│   │   ├── token_refresh.yaml
│   │   ├── proxy_requests.yaml
│   │   ├── tool_execution.yaml         # Tool engine test scenarios
│   │   └── schema_bootstrap.yaml
│   │
│   └── spec.yaml                       # Public API contract
│
├── ts/                                 # TypeScript implementation
│   ├── src/
│   │   ├── index.ts                    # Public exports
│   │   ├── addition.ts                 # Main class
│   │   ├── schema.ts                   # Schema bootstrapper
│   │   ├── oauth.ts                    # OAuth engine
│   │   ├── tokens.ts                   # Token manager
│   │   ├── proxy.ts                    # Authenticated proxy
│   │   ├── providers.ts                # Provider registry loader
│   │   ├── crypto.ts                   # AES-256-GCM encryption
│   │   ├── background.ts              # Background task runner
│   │   ├── tools.ts                    # Layer 5: Tool engine
│   │   ├── toolkits.ts                # Layer 3: Toolkit loader
│   │   ├── adapters/                   # Layer 2: Provider adapters
│   │   │   ├── index.ts               # Adapter registry
│   │   │   ├── base.ts                # Default adapter (standard OAuth)
│   │   │   ├── github.ts
│   │   │   └── slack.ts
│   │   ├── transforms/                 # Layer 4: Parameter transforms
│   │   │   ├── index.ts               # Transform registry
│   │   │   ├── gmail.ts               # RFC 2822 message construction
│   │   │   └── slack.ts               # Block Kit message formatting
│   │   ├── types.ts
│   │   ├── errors.ts
│   │   └── utils.ts
│   ├── test/
│   │   ├── unit/
│   │   │   ├── crypto.test.ts
│   │   │   ├── providers.test.ts
│   │   │   ├── toolkits.test.ts
│   │   │   ├── adapters.test.ts
│   │   │   └── transforms.test.ts
│   │   ├── integration/
│   │   │   ├── oauth.test.ts
│   │   │   ├── tokens.test.ts
│   │   │   ├── proxy.test.ts
│   │   │   ├── tools.test.ts
│   │   │   └── schema.test.ts
│   │   └── conformance/
│   │       └── conformance.test.ts     # Runs shared/test-fixtures
│   ├── package.json
│   ├── tsconfig.json
│   └── README.md
│
├── python/                             # Python implementation
│   ├── src/runagent_addition/
│   │   ├── __init__.py
│   │   ├── addition.py
│   │   ├── schema.py
│   │   ├── oauth.py
│   │   ├── tokens.py
│   │   ├── proxy.py
│   │   ├── providers.py
│   │   ├── crypto.py
│   │   ├── background.py
│   │   ├── tools.py                    # Tool engine
│   │   ├── toolkits.py                # Toolkit loader
│   │   ├── adapters/
│   │   │   ├── __init__.py
│   │   │   ├── base.py
│   │   │   ├── github.py
│   │   │   └── slack.py
│   │   ├── transforms/
│   │   │   ├── __init__.py
│   │   │   ├── gmail.py
│   │   │   └── slack.py
│   │   ├── types.py
│   │   └── errors.py
│   ├── tests/
│   │   ├── unit/
│   │   ├── integration/
│   │   └── conformance/
│   ├── pyproject.toml
│   └── README.md
│
├── examples/                           # Example applications
│   ├── nextjs-app/                     # Next.js App Router example
│   │   ├── package.json
│   │   ├── .env.example
│   │   ├── lib/addition.ts
│   │   ├── app/api/addition/callback/route.ts
│   │   ├── app/dashboard/page.tsx
│   │   └── ...
│   └── fastapi-app/                    # FastAPI example
│       ├── pyproject.toml
│       ├── .env.example
│       ├── main.py
│       └── ...
│
└── docs/
    ├── getting-started.md
    ├── architecture.md
    ├── adding-providers.md             # Complete guide to adding a new provider
    ├── tools-and-toolkits.md
    ├── security.md
    └── api-reference.md
```

---

## 6. Layer 1 — Provider Registry

### Provider YAML Format

Each provider is a YAML file in `shared/providers/`. Both libraries read these at build time and bundle them into the package.

```yaml
# shared/providers/_template.yaml

id: ""                              # Unique ID (lowercase, hyphenated)
name: ""                            # Human-readable display name
auth_type: "oauth2"                 # "oauth2" | "api_key" | "basic"

oauth2:
  authorize_url: ""
  token_url: ""
  revoke_url: ""                    # Optional
  refresh_supported: true
  pkce_supported: true
  pkce_required: false
  default_scopes: []
  scopes_delimiter: "space"         # "space" (standard) or "comma" (Slack)
  authorize_params: {}              # Extra params on authorize URL
  token_params: {}                  # Extra params on token exchange
  token_response_format: "json"     # "json" or "form"
  client_auth_method: "body"        # "body" or "header" (Basic auth)

  # For non-standard token response structures (like Slack)
  # Dot-paths to extract fields from the JSON response
  token_response_mapping:
    access_token: "access_token"
    refresh_token: "refresh_token"
    expires_in: "expires_in"

api:
  base_url: ""
  auth_header: "Authorization"
  auth_prefix: "Bearer"
  rate_limit:
    requests_per_second: 10
    retry_after_header: "Retry-After"

quirks: []                          # Human-readable notes for documentation
```

### Initial Provider Files

#### shared/providers/google.yaml

```yaml
id: "google"
name: "Google"
auth_type: "oauth2"

oauth2:
  authorize_url: "https://accounts.google.com/o/oauth2/v2/auth"
  token_url: "https://oauth2.googleapis.com/token"
  revoke_url: "https://oauth2.googleapis.com/revoke"
  refresh_supported: true
  pkce_supported: true
  pkce_required: false
  default_scopes:
    - "openid"
    - "https://www.googleapis.com/auth/userinfo.email"
    - "https://www.googleapis.com/auth/userinfo.profile"
  scopes_delimiter: "space"
  authorize_params:
    access_type: "offline"
    prompt: "consent"
  token_params: {}
  token_response_format: "json"
  client_auth_method: "body"

api:
  base_url: "https://www.googleapis.com"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
  rate_limit:
    requests_per_second: 10
    retry_after_header: "Retry-After"

quirks:
  - "Requires access_type=offline for refresh tokens"
  - "Requires prompt=consent to guarantee refresh token on first auth"
  - "Scopes are full URLs, not short names"
```

#### shared/providers/gmail.yaml

```yaml
id: "gmail"
name: "Gmail"
auth_type: "oauth2"
extends: "google"

oauth2:
  default_scopes:
    - "openid"
    - "https://www.googleapis.com/auth/userinfo.email"
    - "https://www.googleapis.com/auth/gmail.readonly"
    - "https://www.googleapis.com/auth/gmail.send"
    - "https://www.googleapis.com/auth/gmail.modify"

api:
  base_url: "https://gmail.googleapis.com"

quirks:
  - "Inherits Google OAuth; see google.yaml"
  - "Per-user rate limit: 250 quota units per user per second"
```

#### shared/providers/google-calendar.yaml

```yaml
id: "google-calendar"
name: "Google Calendar"
auth_type: "oauth2"
extends: "google"

oauth2:
  default_scopes:
    - "openid"
    - "https://www.googleapis.com/auth/userinfo.email"
    - "https://www.googleapis.com/auth/calendar"
    - "https://www.googleapis.com/auth/calendar.events"

api:
  base_url: "https://www.googleapis.com/calendar/v3"

quirks:
  - "Inherits Google OAuth; see google.yaml"
  - "Per-user rate limit: 500 requests per 100 seconds per user"
```

#### shared/providers/github.yaml

```yaml
id: "github"
name: "GitHub"
auth_type: "oauth2"

oauth2:
  authorize_url: "https://github.com/login/oauth/authorize"
  token_url: "https://github.com/login/oauth/access_token"
  revoke_url: null
  refresh_supported: false
  pkce_supported: false
  pkce_required: false
  default_scopes:
    - "repo"
    - "read:user"
    - "user:email"
  scopes_delimiter: "space"
  authorize_params: {}
  token_params: {}
  token_response_format: "json"
  client_auth_method: "body"

api:
  base_url: "https://api.github.com"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
  rate_limit:
    requests_per_second: 5
    retry_after_header: "Retry-After"

quirks:
  - "Token exchange requires Accept: application/json header"
  - "Returns HTTP 200 with error in body instead of 4xx on failure"
  - "Tokens do not expire by default; no refresh token flow"
  - "Rate limit info in X-RateLimit-Remaining and X-RateLimit-Reset headers"
```

#### shared/providers/slack.yaml

```yaml
id: "slack"
name: "Slack"
auth_type: "oauth2"

oauth2:
  authorize_url: "https://slack.com/oauth/v2/authorize"
  token_url: "https://slack.com/api/oauth.v2.access"
  revoke_url: "https://slack.com/api/auth.revoke"
  refresh_supported: true
  pkce_supported: false
  pkce_required: false
  default_scopes:
    - "channels:read"
    - "chat:write"
    - "users:read"
    - "users:read.email"
  scopes_delimiter: "comma"
  authorize_params: {}
  token_params: {}
  token_response_format: "json"
  client_auth_method: "body"
  token_response_mapping:
    access_token: "authed_user.access_token"
    refresh_token: "authed_user.refresh_token"
    expires_in: "authed_user.expires_in"

api:
  base_url: "https://slack.com/api"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
  rate_limit:
    requests_per_second: 1
    retry_after_header: "Retry-After"

quirks:
  - "Uses comma-separated scopes"
  - "Token response nests user tokens under authed_user object"
  - "API returns 200 for errors with ok: false in body"
  - "Rate limits vary by method tier; use Retry-After header"
```

### Provider `extends` Mechanism

When a provider YAML has `extends: "google"`, the registry loader must:
1. Load all YAML files
2. Topologically sort by `extends` dependency
3. For each extending provider, deep-merge the base config with the child config
4. Child values override base values where they conflict
5. `id` and `name` always come from the child
6. Arrays (like `default_scopes`) are replaced entirely, not merged

---

## 7. Layer 2 — Provider Adapters

### Why Adapters Exist

YAML handles configuration differences (different URLs, different scopes). But some providers have **behavioral differences** that require code:
- Slack returns `ok: false` on HTTP 200 instead of error status codes
- GitHub requires `Accept: application/json` on token exchange
- GitHub returns HTTP 200 with an `error` field in the body on token exchange failure
- Some providers send token responses as form-encoded instead of JSON

Without adapters, these quirks become `if (provider === 'slack')` branches scattered through the core engine. Adapters isolate each provider's weirdness.

### Adapter Interface

```typescript
// ts/src/adapters/base.ts

export interface ProviderAdapter {
  /**
   * Called before the token exchange HTTP request is sent.
   * Use to add special headers, modify the request body, etc.
   */
  beforeTokenExchange?(request: TokenExchangeRequest): TokenExchangeRequest;

  /**
   * Called after receiving the token exchange response.
   * Use to parse non-standard response formats.
   * Default: extract access_token, refresh_token, expires_in from top-level JSON.
   */
  parseTokenResponse?(status: number, body: any): StandardTokenResponse;

  /**
   * Called after receiving any API response via the proxy.
   * Use to detect errors that come as 200 OK with error in body.
   * Default: treats status >= 400 as error.
   */
  isErrorResponse?(status: number, body: any): boolean;

  /**
   * Called to extract rate limit info from API response headers.
   * Default: reads Retry-After header.
   */
  parseRateLimitHeaders?(headers: Record<string, string>): RateLimitInfo | null;

  /**
   * Called before the authorization URL is constructed.
   * Use to add provider-specific params or modify the URL.
   * Default: no-op.
   */
  beforeAuthorize?(params: AuthorizeParams): AuthorizeParams;
}
```

### Default Adapter

```typescript
// ts/src/adapters/base.ts

export class DefaultAdapter implements ProviderAdapter {
  // All methods have sensible defaults.
  // Providers that follow the OAuth2 spec exactly don't need a custom adapter.
  
  parseTokenResponse(status: number, body: any): StandardTokenResponse {
    if (status >= 400) throw new OAuthError('token_exchange_failed', body);
    return {
      accessToken: body.access_token,
      refreshToken: body.refresh_token || null,
      expiresIn: body.expires_in || null,
      tokenType: body.token_type || 'Bearer',
      scope: body.scope || null,
      raw: body,
    };
  }

  isErrorResponse(status: number, body: any): boolean {
    return status >= 400;
  }
}
```

### GitHub Adapter

```typescript
// ts/src/adapters/github.ts

export class GitHubAdapter extends DefaultAdapter {
  beforeTokenExchange(request: TokenExchangeRequest): TokenExchangeRequest {
    // GitHub requires Accept: application/json or it returns form-encoded
    return {
      ...request,
      headers: {
        ...request.headers,
        'Accept': 'application/json',
      },
    };
  }

  parseTokenResponse(status: number, body: any): StandardTokenResponse {
    // GitHub returns 200 with { error: "...", error_description: "..." } on failure
    if (body.error) {
      throw new OAuthError(body.error, body.error_description);
    }
    return {
      accessToken: body.access_token,
      refreshToken: null,  // GitHub doesn't issue refresh tokens
      expiresIn: null,      // GitHub tokens don't expire
      tokenType: body.token_type || 'Bearer',
      scope: body.scope || null,
      raw: body,
    };
  }
}
```

### Slack Adapter

```typescript
// ts/src/adapters/slack.ts

export class SlackAdapter extends DefaultAdapter {
  parseTokenResponse(status: number, body: any): StandardTokenResponse {
    // Slack returns { ok: false, error: "..." } on failure
    if (!body.ok) {
      throw new OAuthError(body.error, `Slack OAuth error: ${body.error}`);
    }
    // User tokens are nested under authed_user
    const userToken = body.authed_user;
    return {
      accessToken: userToken?.access_token || body.access_token,
      refreshToken: userToken?.refresh_token || null,
      expiresIn: userToken?.expires_in || null,
      tokenType: 'Bearer',
      scope: body.scope || null,
      raw: body,
    };
  }

  isErrorResponse(status: number, body: any): boolean {
    // Slack API returns 200 with ok: false for errors
    if (typeof body === 'object' && body.ok === false) return true;
    return status >= 400;
  }
}
```

### Adapter Registry

```typescript
// ts/src/adapters/index.ts

const adapters: Record<string, ProviderAdapter> = {
  'github': new GitHubAdapter(),
  'slack': new SlackAdapter(),
};

export function getAdapter(providerId: string): ProviderAdapter {
  // Check for exact match, then check the base provider (for extends)
  return adapters[providerId] || new DefaultAdapter();
}
```

Providers that follow the standard OAuth2 spec (Google, Gmail, Google Calendar) don't need a custom adapter — the `DefaultAdapter` handles them correctly. Only providers with behavioral quirks get an adapter file.

The Python implementation mirrors this structure exactly with the same class hierarchy.

---

## 8. Layer 3 — Toolkit Definitions (Tools)

### Why Toolkits, Not Raw APIs

Having an OAuth token for Gmail is useless if the developer still has to read Google's API docs to construct a base64url-encoded RFC 2822 MIME message to send an email. Toolkits bridge the gap between "I have auth" and "I can do useful things."

Toolkits are **curated** — we don't expose every endpoint from Gmail's API (there are hundreds). We expose the 5-10 actions that 95% of developers need, with friendly parameter names and descriptions. The raw proxy is still available for developers who need full API access.

### Toolkit YAML Format

```yaml
# shared/toolkits/_template.yaml

provider: ""                        # Must match a provider ID from shared/providers/
name: ""                            # Human-readable toolkit name
description: ""                     # What this toolkit does (1-2 sentences)

tools:
  - id: ""                          # Unique tool ID within this toolkit (snake_case)
    name: ""                        # Human-readable name
    description: ""                 # What this tool does (used by LLMs for tool selection)
    operation: ""                   # HTTP method + path, e.g., "GET /users/me/messages"

    params:                         # Input parameters the developer provides
      - name: ""
        type: ""                    # "string" | "integer" | "boolean" | "string[]" | "object"
        required: true/false
        default: null               # Optional default value
        enum: []                    # Optional: allowed values
        description: ""             # What this param does (used by LLMs)

    # Optional: a named transform to apply before making the API request.
    # If set, the transform function receives the friendly params and returns
    # the actual request body/params/headers for the API call.
    # If not set, params are passed directly as query params (GET) or JSON body (POST/PUT/PATCH).
    transform: null                 # e.g., "gmail:rfc2822" — "{provider}:{transform_name}"

    # Response mapping: extract and rename fields from the raw API response
    # to provide a cleaner return value. If not set, raw API response is returned.
    response:
      type: "object"
      fields:                       # Key-value: friendly name → description
        fieldName: "Description of this field"

    # Pagination support (optional)
    pagination:
      type: "cursor"               # "cursor" | "offset" | "page"
      cursor_param: "pageToken"     # Query param name for the cursor
      cursor_response_field: "nextPageToken"  # Field in response containing next cursor
```

### Toolkit: Gmail (shared/toolkits/gmail.yaml)

```yaml
provider: "gmail"
name: "Gmail"
description: "Read, send, and manage emails in Gmail"

tools:

  - id: list_messages
    name: "List messages"
    description: "List email messages in the user's mailbox. Returns message IDs and thread IDs. Use get_message to fetch full content."
    operation: "GET /gmail/v1/users/me/messages"
    params:
      - name: query
        type: string
        required: false
        description: "Gmail search query (e.g., 'is:unread', 'from:alice@example.com', 'subject:meeting')"
      - name: maxResults
        type: integer
        required: false
        default: 10
        description: "Maximum number of messages to return (1-500)"
      - name: labelIds
        type: string[]
        required: false
        description: "Only return messages with these label IDs (e.g., ['INBOX', 'UNREAD'])"
    pagination:
      type: cursor
      cursor_param: pageToken
      cursor_response_field: nextPageToken
    response:
      type: object
      fields:
        messages: "Array of {id, threadId} objects"
        nextPageToken: "Token for next page, null if no more results"
        resultSizeEstimate: "Estimated total number of results"

  - id: get_message
    name: "Get message"
    description: "Get the full content of a specific email message, including headers, body, and attachments metadata"
    operation: "GET /gmail/v1/users/me/messages/{messageId}"
    params:
      - name: messageId
        type: string
        required: true
        description: "The ID of the message to retrieve"
      - name: format
        type: string
        required: false
        default: "full"
        enum: ["minimal", "full", "raw", "metadata"]
        description: "The format to return the message in"
    response:
      type: object
      fields:
        id: "Message ID"
        threadId: "Thread ID"
        snippet: "Short preview text of the email"
        labelIds: "Labels applied to this message"
        payload: "Message content including headers, parts, and body"
        internalDate: "Timestamp when the message was received"

  - id: send_message
    name: "Send email"
    description: "Send a new email message. The library handles MIME formatting automatically."
    operation: "POST /gmail/v1/users/me/messages/send"
    transform: "gmail:rfc2822"
    params:
      - name: to
        type: string
        required: true
        description: "Recipient email address (e.g., 'alice@example.com')"
      - name: subject
        type: string
        required: true
        description: "Email subject line"
      - name: body
        type: string
        required: true
        description: "Email body (plain text)"
      - name: cc
        type: string
        required: false
        description: "CC recipient email address"
      - name: bcc
        type: string
        required: false
        description: "BCC recipient email address"
    response:
      type: object
      fields:
        id: "ID of the sent message"
        threadId: "Thread ID of the sent message"
        labelIds: "Labels applied (typically ['SENT'])"

  - id: search_messages
    name: "Search emails"
    description: "Search emails using Gmail's powerful search syntax. Returns matching message IDs."
    operation: "GET /gmail/v1/users/me/messages"
    params:
      - name: query
        type: string
        required: true
        description: "Gmail search query (e.g., 'from:alice subject:meeting after:2024/01/01 has:attachment')"
      - name: maxResults
        type: integer
        required: false
        default: 20
        description: "Maximum number of results"
    pagination:
      type: cursor
      cursor_param: pageToken
      cursor_response_field: nextPageToken
    response:
      type: object
      fields:
        messages: "Array of matching {id, threadId} objects"
        resultSizeEstimate: "Estimated total results"

  - id: modify_message
    name: "Modify message labels"
    description: "Add or remove labels from a message (e.g., mark as read, archive, star)"
    operation: "POST /gmail/v1/users/me/messages/{messageId}/modify"
    params:
      - name: messageId
        type: string
        required: true
        description: "The ID of the message to modify"
      - name: addLabelIds
        type: string[]
        required: false
        description: "Label IDs to add (e.g., ['STARRED', 'IMPORTANT'])"
      - name: removeLabelIds
        type: string[]
        required: false
        description: "Label IDs to remove (e.g., ['UNREAD', 'INBOX'] to mark read and archive)"
    response:
      type: object
      fields:
        id: "Message ID"
        threadId: "Thread ID"
        labelIds: "Updated label list"
```

### Toolkit: Google Calendar (shared/toolkits/google-calendar.yaml)

```yaml
provider: "google-calendar"
name: "Google Calendar"
description: "Create, read, and manage calendar events"

tools:

  - id: list_events
    name: "List events"
    description: "List upcoming events from the user's primary calendar"
    operation: "GET /calendars/primary/events"
    params:
      - name: timeMin
        type: string
        required: false
        description: "Start of time range (RFC 3339, e.g., '2024-01-01T00:00:00Z'). Defaults to now."
      - name: timeMax
        type: string
        required: false
        description: "End of time range (RFC 3339)"
      - name: maxResults
        type: integer
        required: false
        default: 10
        description: "Maximum number of events"
      - name: query
        type: string
        required: false
        description: "Free text search terms to find events"
      - name: singleEvents
        type: boolean
        required: false
        default: true
        description: "Whether to expand recurring events into individual instances"
      - name: orderBy
        type: string
        required: false
        default: "startTime"
        enum: ["startTime", "updated"]
        description: "Sort order (startTime requires singleEvents=true)"
    pagination:
      type: cursor
      cursor_param: pageToken
      cursor_response_field: nextPageToken
    response:
      type: object
      fields:
        items: "Array of event objects"
        nextPageToken: "Token for next page"
        summary: "Calendar title"

  - id: get_event
    name: "Get event"
    description: "Get details of a specific calendar event"
    operation: "GET /calendars/primary/events/{eventId}"
    params:
      - name: eventId
        type: string
        required: true
        description: "The event ID"
    response:
      type: object
      fields:
        id: "Event ID"
        summary: "Event title"
        description: "Event description"
        start: "Start time object"
        end: "End time object"
        attendees: "List of attendees"
        location: "Event location"

  - id: create_event
    name: "Create event"
    description: "Create a new calendar event"
    operation: "POST /calendars/primary/events"
    params:
      - name: summary
        type: string
        required: true
        description: "Event title"
      - name: description
        type: string
        required: false
        description: "Event description"
      - name: startDateTime
        type: string
        required: true
        description: "Start time (RFC 3339, e.g., '2024-06-15T09:00:00-07:00')"
      - name: endDateTime
        type: string
        required: true
        description: "End time (RFC 3339)"
      - name: timeZone
        type: string
        required: false
        default: "UTC"
        description: "Time zone (e.g., 'America/New_York')"
      - name: location
        type: string
        required: false
        description: "Event location"
      - name: attendees
        type: string[]
        required: false
        description: "Email addresses of attendees"
    transform: "google-calendar:create_event_body"
    response:
      type: object
      fields:
        id: "Created event ID"
        htmlLink: "URL to view the event in Google Calendar"
        summary: "Event title"
        start: "Start time"
        end: "End time"

  - id: update_event
    name: "Update event"
    description: "Update an existing calendar event"
    operation: "PATCH /calendars/primary/events/{eventId}"
    params:
      - name: eventId
        type: string
        required: true
        description: "The event ID to update"
      - name: summary
        type: string
        required: false
        description: "New event title"
      - name: description
        type: string
        required: false
        description: "New event description"
      - name: startDateTime
        type: string
        required: false
        description: "New start time (RFC 3339)"
      - name: endDateTime
        type: string
        required: false
        description: "New end time (RFC 3339)"
      - name: location
        type: string
        required: false
        description: "New event location"
    transform: "google-calendar:update_event_body"
    response:
      type: object
      fields:
        id: "Event ID"
        summary: "Updated title"
        updated: "Last modification time"

  - id: delete_event
    name: "Delete event"
    description: "Delete a calendar event"
    operation: "DELETE /calendars/primary/events/{eventId}"
    params:
      - name: eventId
        type: string
        required: true
        description: "The event ID to delete"
    response:
      type: object
      fields: {}
```

### Toolkit: GitHub (shared/toolkits/github.yaml)

```yaml
provider: "github"
name: "GitHub"
description: "Manage repositories, issues, and pull requests"

tools:

  - id: list_repos
    name: "List repositories"
    description: "List repositories for the authenticated user"
    operation: "GET /user/repos"
    params:
      - name: type
        type: string
        required: false
        default: "owner"
        enum: ["all", "owner", "public", "private", "member"]
        description: "Type of repositories to list"
      - name: sort
        type: string
        required: false
        default: "updated"
        enum: ["created", "updated", "pushed", "full_name"]
        description: "Sort field"
      - name: per_page
        type: integer
        required: false
        default: 30
        description: "Results per page (max 100)"
    pagination:
      type: page
      cursor_param: page
      cursor_response_field: null
    response:
      type: array
      fields:
        id: "Repository ID"
        name: "Repository name"
        full_name: "Full name (owner/repo)"
        description: "Repository description"
        html_url: "URL to view on GitHub"
        private: "Whether the repo is private"

  - id: list_issues
    name: "List issues"
    description: "List issues in a repository"
    operation: "GET /repos/{owner}/{repo}/issues"
    params:
      - name: owner
        type: string
        required: true
        description: "Repository owner (username or org)"
      - name: repo
        type: string
        required: true
        description: "Repository name"
      - name: state
        type: string
        required: false
        default: "open"
        enum: ["open", "closed", "all"]
        description: "Filter by state"
      - name: labels
        type: string
        required: false
        description: "Comma-separated label names"
      - name: per_page
        type: integer
        required: false
        default: 30
    response:
      type: array
      fields:
        id: "Issue ID"
        number: "Issue number"
        title: "Issue title"
        state: "open or closed"
        body: "Issue body (markdown)"
        user: "Author information"
        labels: "Applied labels"
        created_at: "Creation timestamp"

  - id: create_issue
    name: "Create issue"
    description: "Create a new issue in a repository"
    operation: "POST /repos/{owner}/{repo}/issues"
    params:
      - name: owner
        type: string
        required: true
        description: "Repository owner"
      - name: repo
        type: string
        required: true
        description: "Repository name"
      - name: title
        type: string
        required: true
        description: "Issue title"
      - name: body
        type: string
        required: false
        description: "Issue body (supports markdown)"
      - name: labels
        type: string[]
        required: false
        description: "Labels to apply"
      - name: assignees
        type: string[]
        required: false
        description: "GitHub usernames to assign"
    response:
      type: object
      fields:
        id: "Issue ID"
        number: "Issue number"
        html_url: "URL to view the issue"
        title: "Issue title"

  - id: list_pull_requests
    name: "List pull requests"
    description: "List pull requests in a repository"
    operation: "GET /repos/{owner}/{repo}/pulls"
    params:
      - name: owner
        type: string
        required: true
      - name: repo
        type: string
        required: true
      - name: state
        type: string
        required: false
        default: "open"
        enum: ["open", "closed", "all"]
      - name: per_page
        type: integer
        required: false
        default: 30
    response:
      type: array
      fields:
        id: "PR ID"
        number: "PR number"
        title: "PR title"
        state: "open or closed"
        html_url: "URL to view"
        user: "Author information"
        created_at: "Creation timestamp"
        head: "Source branch info"
        base: "Target branch info"

  - id: create_pull_request
    name: "Create pull request"
    description: "Create a new pull request"
    operation: "POST /repos/{owner}/{repo}/pulls"
    params:
      - name: owner
        type: string
        required: true
      - name: repo
        type: string
        required: true
      - name: title
        type: string
        required: true
        description: "PR title"
      - name: body
        type: string
        required: false
        description: "PR description (supports markdown)"
      - name: head
        type: string
        required: true
        description: "Branch containing changes (e.g., 'feature-branch')"
      - name: base
        type: string
        required: true
        description: "Branch to merge into (e.g., 'main')"
      - name: draft
        type: boolean
        required: false
        default: false
        description: "Create as draft PR"
    response:
      type: object
      fields:
        id: "PR ID"
        number: "PR number"
        html_url: "URL to view"
        title: "PR title"
```

### Toolkit: Slack (shared/toolkits/slack.yaml)

```yaml
provider: "slack"
name: "Slack"
description: "Send messages and interact with Slack workspaces"

tools:

  - id: send_message
    name: "Send message"
    description: "Send a message to a Slack channel"
    operation: "POST /chat.postMessage"
    params:
      - name: channel
        type: string
        required: true
        description: "Channel ID or name (e.g., '#general' or 'C1234567890')"
      - name: text
        type: string
        required: true
        description: "Message text (supports Slack markdown/mrkdwn)"
      - name: thread_ts
        type: string
        required: false
        description: "Timestamp of parent message to reply in a thread"
    response:
      type: object
      fields:
        ok: "Whether the message was sent successfully"
        ts: "Timestamp of the sent message (use as message ID)"
        channel: "Channel ID where message was posted"

  - id: list_channels
    name: "List channels"
    description: "List public channels in the workspace"
    operation: "GET /conversations.list"
    params:
      - name: types
        type: string
        required: false
        default: "public_channel"
        description: "Comma-separated channel types (public_channel, private_channel, mpim, im)"
      - name: limit
        type: integer
        required: false
        default: 100
        description: "Maximum results (max 1000)"
      - name: exclude_archived
        type: boolean
        required: false
        default: true
    pagination:
      type: cursor
      cursor_param: cursor
      cursor_response_field: response_metadata.next_cursor
    response:
      type: object
      fields:
        channels: "Array of channel objects with id, name, purpose, num_members"
        response_metadata: "Contains next_cursor for pagination"

  - id: list_messages
    name: "List messages"
    description: "List recent messages in a channel"
    operation: "GET /conversations.history"
    params:
      - name: channel
        type: string
        required: true
        description: "Channel ID"
      - name: limit
        type: integer
        required: false
        default: 20
        description: "Number of messages to return (max 1000)"
      - name: oldest
        type: string
        required: false
        description: "Only messages after this timestamp"
      - name: latest
        type: string
        required: false
        description: "Only messages before this timestamp"
    pagination:
      type: cursor
      cursor_param: cursor
      cursor_response_field: response_metadata.next_cursor
    response:
      type: object
      fields:
        messages: "Array of message objects with text, user, ts, type"
        has_more: "Whether more messages exist"
```

---

## 9. Layer 4 — Transforms

### Why Transforms Exist

Some tools need more than "forward this HTTP request." The Gmail `send_message` tool expects the developer to pass `{to, subject, body}`, but the Gmail API expects a base64url-encoded RFC 2822 MIME message in `{raw: "..."}`. The Google Calendar `create_event` tool expects `{summary, startDateTime, endDateTime}`, but the API expects `{summary, start: {dateTime: "..."}, end: {dateTime: "..."}}`.

Transforms bridge the gap between developer-friendly parameters and API-expected formats.

### Transform Registry

Each transform is a named function identified by `"{provider}:{transform_name}"`. The toolkit YAML references them in the `transform` field.

```typescript
// ts/src/transforms/index.ts

import { gmailTransforms } from './gmail';
import { googleCalendarTransforms } from './google-calendar';
import { slackTransforms } from './slack';

const registry: Record<string, TransformFunction> = {
  ...gmailTransforms,
  ...googleCalendarTransforms,
  ...slackTransforms,
};

export type TransformFunction = (params: Record<string, any>) => {
  body?: any;        // JSON body for POST/PUT/PATCH
  params?: Record<string, string>;  // Query params
  headers?: Record<string, string>; // Additional headers
};

export function getTransform(transformId: string): TransformFunction | null {
  return registry[transformId] || null;
}
```

### Gmail Transforms

```typescript
// ts/src/transforms/gmail.ts

export const gmailTransforms: Record<string, TransformFunction> = {
  'gmail:rfc2822': (params) => {
    const { to, subject, body, cc, bcc } = params;
    const lines = [
      `To: ${to}`,
      cc ? `Cc: ${cc}` : null,
      bcc ? `Bcc: ${bcc}` : null,
      `Subject: ${subject}`,
      'MIME-Version: 1.0',
      'Content-Type: text/plain; charset="UTF-8"',
      '',
      body,
    ].filter(Boolean).join('\r\n');

    // Gmail API expects base64url encoding (no padding, URL-safe)
    const raw = Buffer.from(lines).toString('base64url');
    return { body: { raw } };
  },
};
```

### Google Calendar Transforms

```typescript
// ts/src/transforms/google-calendar.ts

export const googleCalendarTransforms: Record<string, TransformFunction> = {
  'google-calendar:create_event_body': (params) => {
    const { summary, description, startDateTime, endDateTime, timeZone, location, attendees } = params;
    return {
      body: {
        summary,
        description: description || undefined,
        location: location || undefined,
        start: { dateTime: startDateTime, timeZone: timeZone || 'UTC' },
        end: { dateTime: endDateTime, timeZone: timeZone || 'UTC' },
        attendees: attendees ? attendees.map((email: string) => ({ email })) : undefined,
      },
    };
  },

  'google-calendar:update_event_body': (params) => {
    const { eventId, summary, description, startDateTime, endDateTime, location } = params;
    const body: Record<string, any> = {};
    if (summary !== undefined) body.summary = summary;
    if (description !== undefined) body.description = description;
    if (location !== undefined) body.location = location;
    if (startDateTime !== undefined) body.start = { dateTime: startDateTime };
    if (endDateTime !== undefined) body.end = { dateTime: endDateTime };
    return { body };
  },
};
```

### When No Transform Is Needed

Most tools don't need a transform. When `transform` is null/absent in the toolkit YAML:
- For **GET/DELETE**: params are sent as URL query parameters
- For **POST/PUT/PATCH**: params are sent as the JSON request body
- Path parameters (like `{messageId}`, `{owner}`, `{repo}`) are interpolated from params automatically

The tool engine handles path interpolation universally — any `{paramName}` in the operation path is replaced with the corresponding param value, and that param is removed from the query/body.

---

## 10. Layer 5 — Tool Engine

### How Tool Execution Works

When `addition.executeTool(...)` is called, this is the execution flow:

```
1. Load toolkit definition for the provider
2. Find the tool by toolId
3. Validate params against the tool's param schema
   - Check required params are present
   - Check types match
   - Check enum values if defined
   - Apply defaults for optional params
4. Interpolate path parameters
   - Replace {paramName} in operation path with param values
   - Remove those params from the remaining params object
5. Apply transform (if specified)
   - Call the transform function with the friendly params
   - Get back the actual body/params/headers for the API request
6. If no transform:
   - GET/DELETE: remaining params become query params
   - POST/PUT/PATCH: remaining params become JSON body
7. Call addition.proxy() with the resolved request
   - This handles auth, token refresh, retries, rate limits
8. Parse the response
9. Return structured result
```

### Tool Engine Interface

```typescript
// ts/src/tools.ts

export interface ExecuteToolParams {
  userId: string;
  providerId: string;
  toolId: string;
  params: Record<string, any>;
  connectionId?: string;              // If user has multiple connections
}

export interface ToolExecutionResult {
  success: boolean;
  data: any;                          // Parsed response body
  status: number;                     // HTTP status code
  error?: string;                     // Error message if success=false
}

export interface ToolDefinition {
  id: string;
  name: string;
  description: string;
  provider: string;
  params: ToolParamDefinition[];
  // This is the structure an LLM or MCP server needs
}

export interface ToolParamDefinition {
  name: string;
  type: string;
  required: boolean;
  default?: any;
  enum?: string[];
  description: string;
}
```

### Getting Tool Definitions (for future LLM/MCP use)

```typescript
// Get all tool definitions for a provider (metadata only, no execution)
const gmailTools: ToolDefinition[] = addition.getToolDefinitions('gmail');

// Get a single tool definition
const sendTool: ToolDefinition = addition.getToolDefinition('gmail', 'send_message');
```

This returns the tool metadata in Addition's native format. The conversion to MCP, OpenAI function-calling, or other formats is a future layer (see Section 19), but the data needed for that conversion is all here: name, description, parameter schemas with types and descriptions.

---

## 11. Database Schema

### shared/schema/001_initial.sql

```sql
-- Addition: Embeddable Integration Runtime
-- Migration 001: Initial schema

CREATE SCHEMA IF NOT EXISTS _addition;

-- Schema version tracking
CREATE TABLE IF NOT EXISTS _addition.meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO _addition.meta (key, value) VALUES ('schema_version', '001')
ON CONFLICT (key) DO UPDATE SET value = '001', updated_at = NOW();

-- Provider configurations (developer's OAuth app credentials)
CREATE TABLE IF NOT EXISTS _addition.provider_configs (
    provider_id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL,
    client_secret_encrypted TEXT NOT NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    extra_config JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-user connections
CREATE TABLE IF NOT EXISTS _addition.connections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    provider_id TEXT NOT NULL REFERENCES _addition.provider_configs(provider_id),
    access_token_encrypted TEXT NOT NULL,
    refresh_token_encrypted TEXT,
    token_expires_at TIMESTAMPTZ,
    token_type TEXT NOT NULL DEFAULT 'Bearer',
    raw_token_response_encrypted TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    status_detail TEXT,
    scopes_granted TEXT[] NOT NULL DEFAULT '{}',
    provider_user_id TEXT,
    provider_user_email TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_connections_user_provider
    ON _addition.connections (user_id, provider_id);

CREATE INDEX IF NOT EXISTS idx_connections_expiring
    ON _addition.connections (token_expires_at)
    WHERE status = 'active' AND refresh_token_encrypted IS NOT NULL;

-- OAuth state (short-lived, for in-flight auth flows)
CREATE TABLE IF NOT EXISTS _addition.oauth_state (
    state TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    code_verifier TEXT,
    redirect_uri TEXT NOT NULL,
    scopes_requested TEXT[] NOT NULL DEFAULT '{}',
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '15 minutes')
);

CREATE INDEX IF NOT EXISTS idx_oauth_state_expires
    ON _addition.oauth_state (expires_at);

-- Audit log
CREATE TABLE IF NOT EXISTS _addition.audit_log (
    id BIGSERIAL PRIMARY KEY,
    event_type TEXT NOT NULL,
    user_id TEXT,
    provider_id TEXT,
    connection_id UUID,
    detail JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_log_user
    ON _addition.audit_log (user_id, created_at DESC);
```

---

## 12. API Contract (spec.yaml)

This defines every public method. Both TypeScript and Python must implement these exactly.

```yaml
version: "0.1.0"

constructor:
  name: "Addition"
  params:
    databaseUrl: { type: string, required: true }
    encryptionKey: { type: string, required: true, description: "64 hex chars (32 bytes)" }
    callbackUrl: { type: string, required: true }
    tokenRefreshIntervalMs: { type: number, default: 60000 }
    tokenRefreshBufferMs: { type: number, default: 300000 }

methods:

  # --- Provider Configuration ---
  configureProvider:
    params:
      providerId: { type: string, required: true }
      clientId: { type: string, required: true }
      clientSecret: { type: string, required: true }
      scopes: { type: "string[]", required: false }
      extraConfig: { type: object, required: false }
    returns: void
    notes: "Idempotent. Encrypts client_secret before storing."

  listConfiguredProviders:
    params: {}
    returns: "ProviderConfig[]"

  # --- OAuth Flows ---
  getAuthUrl:
    params:
      userId: { type: string, required: true }
      providerId: { type: string, required: true }
      scopes: { type: "string[]", required: false }
      redirectAfter: { type: string, required: false }
      metadata: { type: object, required: false }
    returns: string

  handleCallback:
    params:
      query: { type: "Record<string, string>", required: true }
    returns: CallbackResult
    notes: "Exchanges code for tokens, stores encrypted, returns result."

  # --- Connection Management ---
  listConnections:
    params:
      userId: { type: string, required: true }
      providerId: { type: string, required: false }
      status: { type: string, required: false }
    returns: "Connection[]"

  getConnection:
    params:
      connectionId: { type: string, required: true }
    returns: "Connection | null"

  revokeConnection:
    params:
      connectionId: { type: string, required: true }
    returns: void

  # --- Raw Proxy ---
  proxy:
    params:
      userId: { type: string, required: true }
      providerId: { type: string, required: true }
      method: { type: string, required: true }
      path: { type: string, required: true }
      body: { type: any, required: false }
      params: { type: object, required: false }
      headers: { type: object, required: false }
      connectionId: { type: string, required: false }
    returns: ProxyResponse

  # --- Tools ---
  getToolDefinitions:
    params:
      providerId: { type: string, required: true }
    returns: "ToolDefinition[]"
    notes: "Returns all tool definitions for a provider. No auth needed — this is metadata."

  getToolDefinition:
    params:
      providerId: { type: string, required: true }
      toolId: { type: string, required: true }
    returns: "ToolDefinition | null"

  executeTool:
    params:
      userId: { type: string, required: true }
      providerId: { type: string, required: true }
      toolId: { type: string, required: true }
      params: { type: object, required: true }
      connectionId: { type: string, required: false }
    returns: ToolExecutionResult
    notes: "Validates params, applies transforms, calls proxy, returns parsed result."

  listToolkits:
    params: {}
    returns: "ToolkitSummary[]"
    notes: "Returns list of available toolkits with provider, name, description, tool count."

  # --- Lifecycle ---
  startBackgroundTasks:
    params: {}
    returns: void

  stopBackgroundTasks:
    params: {}
    returns: void

  shutdown:
    params: {}
    returns: void
    notes: "Stops background tasks and closes DB connection pool."

  # --- Events ---
  on:
    params:
      event: { type: string }
      handler: { type: function }
    returns: void
    notes: "Events: connection.created, connection.revoked, token.refreshed, token.refresh_failed, tool.executed, tool.error"
```

---

## 13. Test Fixtures

Test fixtures live in `shared/test-fixtures/`. Both TS and Python test suites read these and execute against their implementations. See the original spec for detailed `oauth_flows.yaml` scenarios. Here is the additional fixture file for tool execution:

### shared/test-fixtures/tool_execution.yaml

```yaml
scenarios:

  - name: "Gmail list_messages — basic"
    provider: "gmail"
    tool: "list_messages"
    setup:
      user_id: "user_001"
      connection_status: "active"
      access_token: "valid-token"
    params:
      maxResults: 5
    expect:
      proxy_called_with:
        method: "GET"
        path: "/gmail/v1/users/me/messages"
        query: { maxResults: "5" }
      success: true

  - name: "Gmail send_message — transform applied"
    provider: "gmail"
    tool: "send_message"
    setup:
      user_id: "user_001"
      connection_status: "active"
      access_token: "valid-token"
    params:
      to: "alice@example.com"
      subject: "Hello"
      body: "Hi Alice"
    expect:
      proxy_called_with:
        method: "POST"
        path: "/gmail/v1/users/me/messages/send"
        body_has_field: "raw"
        body_raw_decoded_contains: "To: alice@example.com"
        body_raw_decoded_contains: "Subject: Hello"
      success: true

  - name: "GitHub create_issue — path interpolation"
    provider: "github"
    tool: "create_issue"
    params:
      owner: "myorg"
      repo: "myrepo"
      title: "Bug: login fails"
      body: "Steps to reproduce..."
    expect:
      proxy_called_with:
        method: "POST"
        path: "/repos/myorg/myrepo/issues"
        body: { title: "Bug: login fails", body: "Steps to reproduce..." }
      # owner and repo are consumed by path interpolation, not in body

  - name: "Tool param validation — missing required param"
    provider: "github"
    tool: "create_issue"
    params:
      owner: "myorg"
      # missing: repo, title (both required)
    expect:
      success: false
      error_type: "validation_error"
      error_contains: "title"

  - name: "Tool execution — no active connection"
    provider: "gmail"
    tool: "list_messages"
    setup:
      user_id: "user_no_connection"
      connection_status: null
    params: {}
    expect:
      success: false
      error_type: "connection_not_found"
```

---

## 14. TypeScript Implementation

### Dependencies (ts/package.json)

```json
{
  "name": "@runagent/addition",
  "version": "0.1.0",
  "description": "Embeddable integration runtime — OAuth, tokens, proxy, and tools as a library",
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "license": "MIT",
  "dependencies": {
    "pg": "^8.13.0",
    "yaml": "^2.6.0"
  },
  "devDependencies": {
    "typescript": "^5.7.0",
    "vitest": "^2.0.0",
    "@types/pg": "^8.11.0"
  },
  "files": ["dist", "providers", "toolkits"]
}
```

**Key decisions:**
- **`pg`** for PostgreSQL — most widely used, no ORM overhead
- **`yaml`** for reading provider/toolkit YAML files
- **Node.js built-in `crypto`** for AES-256-GCM — no encryption dependency
- **Node.js built-in `fetch`** (Node 18+) for HTTP requests — no undici/axios dependency
- **No ORM** — raw SQL via `pg` (schema is small and stable)
- **No Redis, no Express** — zero platform dependencies

### Singleton Pattern for Next.js

```typescript
// lib/addition.ts — in a Next.js project
import { Addition } from '@runagent/addition';

const globalForAddition = globalThis as unknown as { addition: Addition };

export const addition = globalForAddition.addition ?? new Addition({
  databaseUrl: process.env.DATABASE_URL!,
  encryptionKey: process.env.ADDITION_SECRET_KEY!,
  callbackUrl: `${process.env.NEXT_PUBLIC_APP_URL}/api/addition/callback`,
});

if (process.env.NODE_ENV !== 'production') {
  globalForAddition.addition = addition;
}

addition.startBackgroundTasks();
```

---

## 15. Python Implementation

### Dependencies (python/pyproject.toml)

```toml
[project]
name = "runagent-addition"
version = "0.1.0"
description = "Embeddable integration runtime"
license = "MIT"
requires-python = ">=3.10"
dependencies = [
    "asyncpg>=0.29.0",
    "httpx>=0.27.0",
    "pyyaml>=6.0",
    "cryptography>=43.0",
]

[project.optional-dependencies]
dev = ["pytest>=8.0", "pytest-asyncio>=0.24"]
```

**Fully async** — all methods are `async def`. Integrates naturally with FastAPI's event loop. The background token refresh runs as an `asyncio.create_task` inside the existing loop — no separate process needed.

### FastAPI Integration (standalone, no other infrastructure)

```python
import os
from contextlib import asynccontextmanager
from fastapi import FastAPI, Request
from fastapi.responses import RedirectResponse
from runagent_addition import Addition

addition = Addition(
    database_url=os.environ["DATABASE_URL"],
    encryption_key=os.environ["ADDITION_SECRET_KEY"],
    callback_url=os.environ["APP_URL"] + "/api/addition/callback",
)

@asynccontextmanager
async def lifespan(app: FastAPI):
    await addition.configure_provider("gmail",
        client_id=os.environ["GOOGLE_CLIENT_ID"],
        client_secret=os.environ["GOOGLE_CLIENT_SECRET"],
    )
    addition.start_background_tasks()
    yield
    await addition.shutdown()

app = FastAPI(lifespan=lifespan)

@app.get("/api/addition/callback")
async def callback(request: Request):
    result = await addition.handle_callback(dict(request.query_params))
    return RedirectResponse(result.redirect_after or "/integrations")

@app.get("/api/emails")
async def list_emails(user_id: str):
    result = await addition.execute_tool(
        user_id=user_id, provider_id="gmail",
        tool_id="list_messages", params={"maxResults": 10},
    )
    return result.data
```

That's the entire integration. `pip install runagent-addition`, one Addition instance, one callback route, done. The library handles schema creation, token management, and background refresh inside the FastAPI process.

---

## 16. Security Design

### Token Encryption
- **Algorithm**: AES-256-GCM
- **Key**: From `ADDITION_SECRET_KEY` env var (64 hex characters = 32 bytes)
- **IV**: Random 12 bytes per encryption
- **Auth tag**: 16 bytes
- **Storage format**: `base64(iv || authTag || ciphertext)`
- Tokens decrypted in memory only when needed
- Encryption key never touches the database
- Encrypted fields: `client_secret_encrypted`, `access_token_encrypted`, `refresh_token_encrypted`, `raw_token_response_encrypted`

### OAuth Security
- **PKCE**: Used when provider supports it (code_challenge + code_verifier)
- **State parameter**: Cryptographically random, stored in DB, validated on callback, deleted after use
- **State expiry**: 15 minutes
- **Tokens**: Never sent to client-side code

### Audit Trail
All security-sensitive operations logged to `_addition.audit_log`:
`connection_created`, `connection_revoked`, `token_refreshed`, `token_refresh_failed`, `provider_configured`

---

## 17. Developer Experience: End to End

This is what a developer actually does, step by step, from installation to making API calls on behalf of their users.

### Step 1: Install

```bash
npm install @runagent/addition
# or
pip install runagent-addition
```

### Step 2: Register OAuth Apps with Providers

This is unavoidable — each provider (Google, GitHub, Slack) requires you to register an application in their developer console to get client credentials. Addition's documentation includes step-by-step guides for each provider. For example, for Google:

1. Go to Google Cloud Console → APIs & Services → Credentials
2. Create an OAuth 2.0 Client ID (Web Application)
3. Set the callback URL to `https://yourapp.com/api/addition/callback`
4. Note the Client ID and Client Secret
5. Enable the Gmail API and/or Calendar API in the same project

### Step 3: Set Environment Variables

```env
DATABASE_URL=postgresql://user:pass@localhost:5432/myapp
ADDITION_SECRET_KEY=a1b2c3d4e5f6...   # openssl rand -hex 32
GOOGLE_CLIENT_ID=xxx.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=GOCSPx-xxx
GITHUB_CLIENT_ID=Ov23li...
GITHUB_CLIENT_SECRET=xxx
SLACK_CLIENT_ID=123456.789012
SLACK_CLIENT_SECRET=xxx
```

### Step 4: Initialize Addition

```typescript
// lib/addition.ts
import { Addition } from '@runagent/addition'

export const addition = new Addition({
  databaseUrl: process.env.DATABASE_URL!,
  encryptionKey: process.env.ADDITION_SECRET_KEY!,
  callbackUrl: process.env.NEXT_PUBLIC_APP_URL + '/api/addition/callback',
})

// Register providers (idempotent — safe on every startup)
await addition.configureProvider('gmail', {
  clientId: process.env.GOOGLE_CLIENT_ID!,
  clientSecret: process.env.GOOGLE_CLIENT_SECRET!,
})
await addition.configureProvider('github', {
  clientId: process.env.GITHUB_CLIENT_ID!,
  clientSecret: process.env.GITHUB_CLIENT_SECRET!,
})

addition.startBackgroundTasks()
```

**First run**: Addition connects to the database, creates the `_addition` schema and all tables, stores the encrypted provider credentials. Zero manual migration steps.

### Step 5: Mount Callback Route

```typescript
// app/api/addition/callback/route.ts (Next.js)
import { addition } from '@/lib/addition'
import { redirect } from 'next/navigation'

export async function GET(request: Request) {
  const query = Object.fromEntries(new URL(request.url).searchParams)
  const result = await addition.handleCallback(query)
  redirect(result.success
    ? result.redirectAfter || '/dashboard?connected=' + result.providerId
    : '/dashboard?error=' + result.error
  )
}
```

### Step 6: Let Users Connect Accounts

```typescript
// Server Action
'use server'
import { addition } from '@/lib/addition'
import { auth } from '@/lib/auth'
import { redirect } from 'next/navigation'

export async function connectProvider(providerId: string) {
  const session = await auth()
  const url = await addition.getAuthUrl({
    userId: session.user.id,
    providerId,
    redirectAfter: '/dashboard',
  })
  redirect(url)
}
```

User clicks "Connect Gmail" → redirected to Google consent screen → approves → redirected to callback → tokens stored encrypted → redirected to dashboard.

### Step 7: Use Tools

```typescript
// Using the high-level tools layer (recommended)
const emails = await addition.executeTool({
  userId: session.user.id,
  providerId: 'gmail',
  toolId: 'list_messages',
  params: { query: 'is:unread', maxResults: 5 },
})
// emails.data.messages → [{id, threadId}, ...]

// Send an email (library handles MIME encoding automatically)
await addition.executeTool({
  userId: session.user.id,
  providerId: 'gmail',
  toolId: 'send_message',
  params: { to: 'alice@example.com', subject: 'Hello', body: 'Hi Alice!' },
})

// Create a calendar event
await addition.executeTool({
  userId: session.user.id,
  providerId: 'google-calendar',
  toolId: 'create_event',
  params: {
    summary: 'Team standup',
    startDateTime: '2024-06-15T09:00:00-07:00',
    endDateTime: '2024-06-15T09:30:00-07:00',
    attendees: ['bob@example.com'],
  },
})

// OR using the raw proxy (full API access)
const raw = await addition.proxy({
  userId: session.user.id,
  providerId: 'gmail',
  method: 'GET',
  path: '/gmail/v1/users/me/labels',
})
```

### Step 8: Manage Connections

```typescript
// List user's connections
const connections = await addition.listConnections(session.user.id)
// → [{ id, providerId: 'gmail', status: 'active', providerUserEmail: 'user@gmail.com', ... }]

// Revoke a connection
await addition.revokeConnection(connectionId)
```

---

## 18. Scaling: Adding a New Provider

When the time comes to add a 6th, 7th, or 50th provider, here is the complete checklist:

### If the provider follows standard OAuth2 (most do):

1. **Create `shared/providers/{provider}.yaml`** — copy `_template.yaml`, fill in URLs, scopes, rate limits
2. **Create `shared/toolkits/{provider}.yaml`** — define 5-10 curated tools with params and descriptions
3. **Add transforms if needed** — only if any tool needs to translate friendly params to API format
4. **No adapter needed** — the `DefaultAdapter` handles standard OAuth2

That's 2-3 YAML files and maybe one small code file. No changes to the core engine.

### If the provider has behavioral quirks:

4. **Create `ts/src/adapters/{provider}.ts`** and `python/src/runagent_addition/adapters/{provider}.py`** — implement only the hooks that differ from the default
5. **Register the adapter** in the adapter index

### Testing a new provider:

6. **Add scenarios to `shared/test-fixtures/oauth_flows.yaml`** — at minimum, a happy-path flow and an error case
7. **Add scenarios to `shared/test-fixtures/tool_execution.yaml`** — at minimum, one tool execution per toolkit

---

## 19. Future: Exposing Tools as MCP / Function Calling

This is explicitly **not in v0.1.0 scope**, but the architecture is designed so this is a straightforward structural conversion with no semantic work.

Every `ToolDefinition` already contains: `id`, `name`, `description`, `params` (with name, type, required, description, enum). This maps directly to:

### OpenAI Function Calling

```typescript
const openaiTools = addition.getToolDefinitions('gmail').map(tool => ({
  type: 'function',
  function: {
    name: `${tool.provider}_${tool.id}`,
    description: tool.description,
    parameters: {
      type: 'object',
      properties: Object.fromEntries(
        tool.params.map(p => [p.name, {
          type: p.type === 'string[]' ? 'array' : p.type,
          description: p.description,
          enum: p.enum || undefined,
          items: p.type === 'string[]' ? { type: 'string' } : undefined,
        }])
      ),
      required: tool.params.filter(p => p.required).map(p => p.name),
    },
  },
}));
```

### MCP (Model Context Protocol)

```typescript
const mcpTools = addition.getToolDefinitions('gmail').map(tool => ({
  name: `${tool.provider}_${tool.id}`,
  description: tool.description,
  inputSchema: {
    type: 'object',
    properties: /* same as above */,
    required: /* same as above */,
  },
}));
```

A future `@runagent/addition-mcp` package could expose an MCP server that wraps Addition's tool engine, providing a complete MCP-compatible integration server that still uses only your database.

---

## 20. Development Roadmap

### v0.1.0 — Foundation

**Providers**: Google (base), Gmail, Google Calendar, GitHub, Slack

**Scope**:
- Monorepo with shared/, ts/, python/ directories
- Provider registry with 5 YAML files + extends mechanism
- Provider adapters (GitHub, Slack)
- Toolkit definitions for all 4 toolkits (Gmail, Calendar, GitHub, Slack)
- Transforms (Gmail RFC 2822, Google Calendar event body)
- Database schema with auto-bootstrap
- AES-256-GCM encryption
- OAuth engine (authorize URL, callback, PKCE, state management)
- Token manager (store, retrieve, refresh)
- Authenticated proxy (credential injection, 401 retry, 429 backoff)
- Background token refresh loop
- Tool engine (validate, transform, execute, parse)
- Event emitter
- TypeScript implementation (full)
- Python implementation (full)
- Shared test fixtures + conformance tests
- Next.js example app
- FastAPI example app
- Documentation

**Non-goals for v0.1.0**: Webhook/trigger support, UI components, SQLite support, MCP export, rate limit persistence, connection health checks

> **Roadmap re-sequenced (2026-08-14).** To back an on-platform Composio/Nango
> for the host app, MCP export and endpoint auto-mounting are pulled forward
> from v0.3.0 into the **v0.2.0** arc, joined by a new **collections** concept.
> The detailed, test-first breakdown lives in `docs/mcp-collections-plan.md`
> (milestones M14–M18); this section is the summary of record.

### v0.2.0 — Projections: MCP + REST + Collections
The v0.1.0 tool catalog becomes a single source of truth projected three ways
(manual `executeTool`, REST endpoints, MCP server), sliceable into collections.
- Tool-schema projection: `params → JSON Schema`, MCP tool descriptors (M14)
- MCP server generation wrapping the tool engine (M15)
- REST endpoint generation + framework adapters (Express/Next/FastAPI) (M16)
- Collections domain model (`002_collections.sql`, per-tenant scoping) (M17)
- Collection-scoped projections + conformance + version bump to 0.2.0 (M18)

### v0.3.0 — Developer Experience & breadth
- OpenAI/Anthropic function-calling export (trivial atop the M14 schema mapper)
- SQLite for local development
- Pre-built `<ConnectButton>` React component; CLI `npx addition add google`
- Webhook/trigger support
- More providers: Microsoft, Notion, Linear, Jira, HubSpot, Salesforce, Stripe, Discord

---

## 21. Implementation Order

Build in this exact order. Each step should have passing tests before the next.

### Phase 1: Shared Foundation (steps 1-8)

1. Create monorepo directory structure (all folders, README, LICENSE)
2. Write `shared/providers/_template.yaml` and all 5 provider YAML files
3. Write `shared/toolkits/_template.yaml` and all 4 toolkit YAML files
4. Write `shared/schema/001_initial.sql`
5. Write `shared/spec.yaml`
6. Write `shared/test-fixtures/oauth_flows.yaml` (all scenarios including error cases)
7. Write `shared/test-fixtures/token_refresh.yaml`
8. Write `shared/test-fixtures/tool_execution.yaml`

### Phase 2: TypeScript Core (steps 9-24)

9. Initialize TS project (package.json, tsconfig.json, vitest config)
10. `types.ts` — all type definitions
11. `errors.ts` — all error classes
12. `crypto.ts` — AES-256-GCM encrypt/decrypt + unit tests
13. `providers.ts` — load YAML, handle `extends` merging + unit tests
14. `toolkits.ts` — load toolkit YAML, parse tool definitions + unit tests
15. `schema.ts` — connect to PG, bootstrap schema, run migrations + integration tests
16. `adapters/base.ts` — DefaultAdapter
17. `adapters/github.ts` — GitHub adapter + unit tests
18. `adapters/slack.ts` — Slack adapter + unit tests
19. `adapters/index.ts` — adapter registry
20. `oauth.ts` — auth URL generation (with PKCE/state), callback handling (with adapter hooks) + tests with mocked HTTP
21. `tokens.ts` — store encrypted, retrieve decrypted, refresh via adapter + tests
22. `proxy.ts` — resolve connection, inject auth, retry 401/429, adapter hooks + tests
23. `transforms/gmail.ts`, `transforms/google-calendar.ts`, `transforms/index.ts` + unit tests
24. `tools.ts` — tool engine (validate params, interpolate path, apply transform, call proxy, parse response) + tests
25. `background.ts` — setInterval loop, query expiring tokens, refresh + tests
26. `addition.ts` — main class wiring everything together + integration tests
27. `index.ts` — public exports
28. Conformance tests against shared fixtures
29. Build and verify npm package

### Phase 3: Python Core (steps 30-42)

30. Initialize Python project (pyproject.toml, pytest config)
31. `types.py`, `errors.py`
32. `crypto.py` + tests
33. `providers.py` + tests
34. `toolkits.py` + tests
35. `schema.py` + tests
36. `adapters/` — base, github, slack + tests
37. `oauth.py` + tests
38. `tokens.py` + tests
39. `proxy.py` + tests
40. `transforms/` + tests
41. `tools.py` + tests
42. `background.py` + tests
43. `addition.py` — main class + integration tests
44. Conformance tests against shared fixtures
45. Build and verify PyPI package

### Phase 4: Example Apps & Docs (steps 46-50)

46. Next.js example app (App Router, Server Actions, full flow)
47. FastAPI example app (full flow)
48. `docs/getting-started.md`
49. `docs/adding-providers.md`
50. `docs/architecture.md`, `docs/tools-and-toolkits.md`, `docs/security.md`

---

## 22. Key Implementation Notes

### Path Parameter Interpolation

Tool operations contain path parameters like `GET /repos/{owner}/{repo}/issues`. The tool engine must:
1. Scan the operation path for `{paramName}` placeholders
2. For each placeholder, find the matching param in the input, substitute it into the path
3. Remove that param from the remaining params (so it's not also sent as a query param or body field)

Example: `create_issue` with params `{owner: "myorg", repo: "myrepo", title: "Bug", body: "..."}`:
- Path becomes: `/repos/myorg/myrepo/issues`
- Remaining params (sent as JSON body): `{title: "Bug", body: "..."}`

### Token Refresh Background Loop

1. Run every `tokenRefreshIntervalMs` (default 60s)
2. Query: `SELECT * FROM _addition.connections WHERE token_expires_at < NOW() + interval '5 minutes' AND status = 'active' AND refresh_token_encrypted IS NOT NULL`
3. For each: decrypt refresh token, call provider's token URL, encrypt new tokens, update row
4. On success: emit `token.refreshed` event, write audit log
5. On failure: set `status = 'refresh_failed'`, emit `token.refresh_failed` event

### Proxy Retry Logic

1. Decrypt access token, make request with `Authorization: Bearer <token>`
2. On 401: if refresh token exists, refresh and retry once
3. On 429: read `Retry-After` header (seconds or HTTP date), wait, retry up to 3 times
4. On network error: retry once after 1 second
5. Always pass response through adapter's `isErrorResponse()` hook

### Schema Bootstrap Sequence

1. Check if schema `_addition` exists
2. If not: create schema, run all migration files in order
3. If yes: read `schema_version` from `_addition.meta`
4. Run any migrations with version > current
5. Update version in meta table
6. Set `initialized = true` flag (skip on subsequent calls)

### Error Classes

Both implementations must define:
- `AdditionError` — base
- `ProviderNotConfiguredError` — no credentials for this provider
- `ProviderNotFoundError` — no YAML definition for this provider ID
- `ConnectionNotFoundError` — no active connection for user+provider
- `OAuthError` — OAuth flow failed (code + description)
- `TokenRefreshError` — refresh failed
- `EncryptionError` — encrypt/decrypt failure
- `SchemaError` — schema bootstrap/migration failure
- `ToolNotFoundError` — no tool with this ID in the toolkit
- `ToolValidationError` — params failed validation (lists which params are invalid)
- `ToolExecutionError` — tool executed but API returned an error

### Versioning

Single version across the monorepo. All three files (`shared/spec.yaml` version field, `ts/package.json` version, `python/pyproject.toml` version) must always match. Bump all three together on any release.

---

## 23. Reference Codebases

For studying provider-specific edge cases and implementation patterns (reference only — do not copy code):

1. **Nango** (TypeScript, Elastic License): `github.com/NangoHQ/nango`
   - Provider specs: `packages/shared/lib/services/providers/`
   - OAuth flows: `packages/server/lib/controllers/auth/`
   - Token refresh: `packages/shared/lib/services/connections/`

2. **n8n** (TypeScript, sustainable use license): `github.com/n8n-io/n8n`
   - Credential handling: `packages/cli/src/credentials/`
   - OAuth per-provider: `packages/nodes-base/credentials/`

3. **Activepieces** (TypeScript, MIT): `github.com/activepieces/activepieces`
   - Piece auth definitions: `packages/pieces/community/`
   - Tool/action definitions: each piece's `src/lib/actions/`

Study these for patterns on handling quirks, then implement from scratch under MIT.