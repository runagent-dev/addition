export interface ProviderOAuth2Config {
  authorize_url: string;
  token_url: string;
  revoke_url: string | null;
  refresh_supported: boolean;
  pkce_supported: boolean;
  pkce_required: boolean;
  default_scopes: string[];
  scopes_delimiter: 'space' | 'comma';
  authorize_params: Record<string, string>;
  token_params: Record<string, string>;
  token_response_format: 'json' | 'form';
  client_auth_method: 'body' | 'header';
  /**
   * Dot-paths into the token-exchange JSON response for providers that don't
   * return tokens at the top level (e.g. Slack nests under `authed_user`).
   *
   * Optional — when absent, the OAuth engine uses the standard OAuth2 layout
   * (top-level `access_token`, `refresh_token`, `expires_in`). This is the
   * single source of truth for where tokens live in the response; adapters
   * MUST NOT duplicate this extraction logic (see CLAUDE.md §6 invariant 10).
   */
  token_response_mapping?: {
    access_token: string;
    refresh_token: string;
    expires_in: string;
  };
}

export interface ProviderApiConfig {
  base_url: string;
  auth_header: string;
  auth_prefix: string;
  rate_limit: {
    requests_per_second: number;
    retry_after_header: string;
  };
}

export interface ProviderConfig {
  id: string;
  name: string;
  auth_type: 'oauth2' | 'api_key' | 'basic';
  oauth2: ProviderOAuth2Config;
  api: ProviderApiConfig;
  quirks: string[];
}

export type ProviderRegistry = Record<string, ProviderConfig>;

// ---------- Toolkits (Layer 3) ----------

export type ToolParamType =
  | 'string'
  | 'integer'
  | 'boolean'
  | 'string[]'
  | 'object';

export interface ToolParamDefinition {
  name: string;
  type: ToolParamType;
  required: boolean;
  default?: unknown;
  enum?: (string | number)[];
  description: string;
}

export interface ToolPaginationConfig {
  type: 'cursor' | 'offset' | 'page';
  /** Query-string param name to send the cursor/page in. Always a simple key. */
  cursor_param: string;
  /**
   * Location of the next cursor in the API response. Supports **dot-paths**
   * into nested objects — same convention as `token_response_mapping` in
   * provider configs. Examples:
   *   - `"nextPageToken"` → `response.nextPageToken` (top-level)
   *   - `"response_metadata.next_cursor"` → `response.response_metadata.next_cursor` (Slack)
   * `null` means the provider uses a different paging signal (e.g. GitHub's
   * `page` offsets or Link header) and the tool engine should not auto-extract.
   */
  cursor_response_field: string | null;
}

export interface ToolResponseConfig {
  type: 'object' | 'array';
  fields: Record<string, string>;
}

export interface ToolDefinition {
  id: string;
  name: string;
  description: string;
  provider: string;
  operation: string;
  params: ToolParamDefinition[];
  /** Named transform id in "{provider}:{transform_name}" form, or null. */
  transform: string | null;
  pagination: ToolPaginationConfig | null;
  response: ToolResponseConfig | null;
}

export interface ToolkitConfig {
  provider: string;
  name: string;
  description: string;
  tools: ToolDefinition[];
}

/** Map from provider id → toolkit. */
export type ToolkitRegistry = Record<string, ToolkitConfig>;
