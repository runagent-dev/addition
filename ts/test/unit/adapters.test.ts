import { describe, expect, it } from 'vitest';
import {
  DefaultAdapter,
  GitHubAdapter,
  SlackAdapter,
  getAdapter,
} from '../../src/adapters/index.js';
import type { TokenExchangeRequest } from '../../src/types.js';

function mkReq(overrides: Partial<TokenExchangeRequest> = {}): TokenExchangeRequest {
  return {
    url: 'https://example.com/token',
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: { grant_type: 'authorization_code', code: 'abc' },
    ...overrides,
  };
}

describe('adapters — invariant #10 scope', () => {
  // Scope guard: adapters handle request-shaping and error detection ONLY.
  // Token extraction is YAML + engine (CLAUDE.md §6 invariant #10). If anyone
  // later adds a `parseTokenResponse` hook to bring back the pre-M2 design,
  // these assertions should fail.
  it('DefaultAdapter has no parseTokenResponse method', () => {
    const a = new DefaultAdapter() as unknown as Record<string, unknown>;
    expect(a.parseTokenResponse).toBeUndefined();
  });
  it('GitHubAdapter has no parseTokenResponse method', () => {
    const a = new GitHubAdapter() as unknown as Record<string, unknown>;
    expect(a.parseTokenResponse).toBeUndefined();
  });
  it('SlackAdapter has no parseTokenResponse method', () => {
    const a = new SlackAdapter() as unknown as Record<string, unknown>;
    expect(a.parseTokenResponse).toBeUndefined();
  });
});

describe('DefaultAdapter', () => {
  const a = new DefaultAdapter();

  it('isErrorResponse: true for HTTP 500', () => {
    expect(a.isErrorResponse(500, {})).toBe(true);
  });

  it('isErrorResponse: true for HTTP 400', () => {
    expect(a.isErrorResponse(400, { error: 'bad_request' })).toBe(true);
  });

  it('isErrorResponse: false for HTTP 200', () => {
    expect(a.isErrorResponse(200, {})).toBe(false);
  });

  it('isErrorResponse: false for HTTP 201', () => {
    expect(a.isErrorResponse(201, { id: 'abc' })).toBe(false);
  });

  it('extractErrorDetail: reads OAuth2 {error, error_description}', () => {
    const detail = a.extractErrorDetail({
      error: 'invalid_grant',
      error_description: 'Authorization code expired',
    });
    expect(detail.code).toBe('invalid_grant');
    expect(detail.description).toBe('Authorization code expired');
  });

  it('extractErrorDetail: handles missing description', () => {
    const detail = a.extractErrorDetail({ error: 'invalid_grant' });
    expect(detail.code).toBe('invalid_grant');
    expect(detail.description).toBeUndefined();
  });

  it('extractErrorDetail: falls back to unknown_error for non-OAuth bodies', () => {
    expect(a.extractErrorDetail({}).code).toBe('unknown_error');
    expect(a.extractErrorDetail(null).code).toBe('unknown_error');
    expect(a.extractErrorDetail('string body').code).toBe('unknown_error');
  });

  it('beforeTokenExchange: default is a no-op (returns request unchanged)', () => {
    const req = mkReq();
    expect(a.beforeTokenExchange(req)).toEqual(req);
  });
});

describe('GitHubAdapter', () => {
  const a = new GitHubAdapter();

  it('beforeTokenExchange: adds Accept: application/json (GitHub quirk)', () => {
    const req = mkReq();
    const out = a.beforeTokenExchange(req);
    expect(out.headers['Accept']).toBe('application/json');
  });

  it('beforeTokenExchange: preserves existing headers', () => {
    const req = mkReq();
    const out = a.beforeTokenExchange(req);
    expect(out.headers['Content-Type']).toBe('application/x-www-form-urlencoded');
  });

  it('beforeTokenExchange: does not mutate the input request', () => {
    const req = mkReq();
    const snapshot = JSON.parse(JSON.stringify(req));
    a.beforeTokenExchange(req);
    expect(req).toEqual(snapshot);
  });

  it('isErrorResponse: true for HTTP 200 with {error: "bad_verification_code"} (GitHub token quirk)', () => {
    expect(
      a.isErrorResponse(200, {
        error: 'bad_verification_code',
        error_description: 'The code passed is incorrect or expired.',
      }),
    ).toBe(true);
  });

  it('isErrorResponse: false for HTTP 200 healthy body (no error field)', () => {
    expect(a.isErrorResponse(200, { access_token: 'gho_xxx', token_type: 'bearer' })).toBe(
      false,
    );
  });

  it('isErrorResponse: true for HTTP 500 (inherits default behavior)', () => {
    expect(a.isErrorResponse(500, {})).toBe(true);
  });

  it('extractErrorDetail: reads {error, error_description}', () => {
    const detail = a.extractErrorDetail({
      error: 'bad_verification_code',
      error_description: 'The code passed is incorrect or expired.',
    });
    expect(detail.code).toBe('bad_verification_code');
    expect(detail.description).toBe('The code passed is incorrect or expired.');
  });
});

describe('SlackAdapter', () => {
  const a = new SlackAdapter();

  it('isErrorResponse: true for HTTP 200 with {ok: false, error: "invalid_auth"} (Slack quirk)', () => {
    expect(a.isErrorResponse(200, { ok: false, error: 'invalid_auth' })).toBe(true);
  });

  it('isErrorResponse: false for HTTP 200 with {ok: true, ...}', () => {
    expect(a.isErrorResponse(200, { ok: true, channel: 'C123', ts: '1.2' })).toBe(false);
  });

  it('isErrorResponse: true for HTTP 500 (inherits default behavior)', () => {
    expect(a.isErrorResponse(500, {})).toBe(true);
  });

  it('isErrorResponse: ignores bodies without an `ok` field', () => {
    // A Slack response without `ok` is weird but should fall back to status-based detection.
    expect(a.isErrorResponse(200, { some: 'payload' })).toBe(false);
  });

  it('extractErrorDetail: reads top-level {error: "invalid_auth"} (Slack style)', () => {
    const detail = a.extractErrorDetail({ ok: false, error: 'invalid_auth' });
    expect(detail.code).toBe('invalid_auth');
  });

  it('beforeTokenExchange: default pass-through (no special headers)', () => {
    const req = mkReq();
    expect(a.beforeTokenExchange(req)).toEqual(req);
  });
});

describe('adapter registry — getAdapter', () => {
  it('github → GitHubAdapter', () => {
    expect(getAdapter('github')).toBeInstanceOf(GitHubAdapter);
  });

  it('slack → SlackAdapter', () => {
    expect(getAdapter('slack')).toBeInstanceOf(SlackAdapter);
  });

  it('google → DefaultAdapter (no special quirks)', () => {
    expect(getAdapter('google')).toBeInstanceOf(DefaultAdapter);
  });

  it('gmail → DefaultAdapter', () => {
    expect(getAdapter('gmail')).toBeInstanceOf(DefaultAdapter);
  });

  it('google-calendar → DefaultAdapter', () => {
    expect(getAdapter('google-calendar')).toBeInstanceOf(DefaultAdapter);
  });

  it('unknown provider id → DefaultAdapter (graceful fallback)', () => {
    expect(getAdapter('unknown-provider')).toBeInstanceOf(DefaultAdapter);
  });

  it('registry returns stable singletons (same instance on repeated calls)', () => {
    expect(getAdapter('github')).toBe(getAdapter('github'));
    expect(getAdapter('slack')).toBe(getAdapter('slack'));
  });
});
