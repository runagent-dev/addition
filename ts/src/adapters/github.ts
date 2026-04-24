import { DefaultAdapter } from './base.js';
import type { TokenExchangeRequest } from '../types.js';

/**
 * GitHub's OAuth quirks (github.yaml §quirks):
 *   - Token exchange requires `Accept: application/json` or GitHub returns
 *     form-encoded responses instead of JSON.
 *   - Token exchange returns HTTP 200 with `{error, error_description}` on
 *     failure instead of a 4xx status.
 */
export class GitHubAdapter extends DefaultAdapter {
  override beforeTokenExchange(req: TokenExchangeRequest): TokenExchangeRequest {
    return {
      ...req,
      headers: { ...req.headers, Accept: 'application/json' },
    };
  }

  override isErrorResponse(status: number, body: unknown): boolean {
    if (super.isErrorResponse(status, body)) return true;
    if (body && typeof body === 'object') {
      const b = body as Record<string, unknown>;
      if (typeof b.error === 'string') return true;
    }
    return false;
  }
}
