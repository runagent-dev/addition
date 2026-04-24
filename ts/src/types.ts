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
