import type { AdapterErrorDetail, TokenExchangeRequest } from '../types.js';

/**
 * Adapter contract — scoped by CLAUDE.md §6 invariant #10.
 *
 * Adapters own **request-shaping** and **error detection** only.
 * Token extraction is data-driven via `provider.oauth2.token_response_mapping`
 * in the provider YAML, executed by the OAuth engine (M8). Adapters MUST NOT
 * implement a `parseTokenResponse` hook — a scope test in `adapters.test.ts`
 * guards this.
 *
 * All methods are concrete (not optional) so callers never have to null-check.
 * Providers without quirks simply use `DefaultAdapter`.
 */
export interface ProviderAdapter {
  /**
   * Rewrite the outgoing token-exchange request (headers, body). Return the
   * modified request; do not mutate the input. Use for provider-specific
   * request quirks (e.g. GitHub's `Accept: application/json`).
   */
  beforeTokenExchange(req: TokenExchangeRequest): TokenExchangeRequest;

  /**
   * Decide whether an HTTP response should be treated as an error, even if
   * the status is 2xx. Use for:
   *   - GitHub token exchange returning 200 with `{error: "..."}` in the body
   *   - Slack API returning 200 with `{ok: false, error: "..."}`
   * Standard `status >= 400` detection is handled by the default impl — only
   * override when the provider uses a non-standard error shape.
   */
  isErrorResponse(status: number, body: unknown): boolean;

  /**
   * Given a body that `isErrorResponse` flagged, extract a machine-readable
   * error `code` and optional human-readable `description`. Used by the
   * OAuth engine and proxy to construct `OAuthError` / similar error types.
   */
  extractErrorDetail(body: unknown): AdapterErrorDetail;
}

export class DefaultAdapter implements ProviderAdapter {
  beforeTokenExchange(req: TokenExchangeRequest): TokenExchangeRequest {
    return req;
  }

  isErrorResponse(status: number, _body: unknown): boolean {
    return status >= 400;
  }

  extractErrorDetail(body: unknown): AdapterErrorDetail {
    if (body && typeof body === 'object') {
      const b = body as Record<string, unknown>;
      if (typeof b.error === 'string') {
        const detail: AdapterErrorDetail = { code: b.error };
        if (typeof b.error_description === 'string') detail.description = b.error_description;
        return detail;
      }
    }
    return { code: 'unknown_error' };
  }
}
