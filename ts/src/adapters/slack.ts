import { DefaultAdapter } from './base.js';
import type { AdapterErrorDetail } from '../types.js';

/**
 * Slack's API quirks (slack.yaml §quirks):
 *   - Every endpoint returns HTTP 200, even on errors. Failure is signaled
 *     by `{ok: false, error: "..."}` in the body.
 *   - Token extraction for user tokens lives under `authed_user.*` — that's
 *     handled data-drivenly via `slack.yaml`'s `token_response_mapping`, NOT
 *     here (CLAUDE.md §6 invariant #10).
 */
export class SlackAdapter extends DefaultAdapter {
  override isErrorResponse(status: number, body: unknown): boolean {
    if (super.isErrorResponse(status, body)) return true;
    if (body && typeof body === 'object') {
      const b = body as Record<string, unknown>;
      if ('ok' in b && b.ok === false) return true;
    }
    return false;
  }

  override extractErrorDetail(body: unknown): AdapterErrorDetail {
    if (body && typeof body === 'object') {
      const b = body as Record<string, unknown>;
      if (typeof b.error === 'string') {
        return { code: b.error };
      }
    }
    return super.extractErrorDetail(body);
  }
}
