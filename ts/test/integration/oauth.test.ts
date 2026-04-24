import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { bootstrap } from '../../src/schema.js';
import { loadProviders } from '../../src/providers.js';
import { encrypt, decrypt } from '../../src/crypto.js';
import { getAuthUrl, handleCallback } from '../../src/oauth.js';
import type { ProviderRegistry } from '../../src/types.js';
import type { OAuthDeps } from '../../src/oauth.js';

const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');
const SHARED_SCHEMA = join(__dirname, '..', '..', '..', 'shared', 'schema');
const TEST_KEY = 'a'.repeat(64); // 32 bytes of 0xaa
const CALLBACK_URL = 'https://app.example.com/api/addition/callback';

let container: StartedPostgreSqlContainer;
let databaseUrl: string;
let providers: ProviderRegistry;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_test')
    .start();
  databaseUrl = container.getConnectionUri();
  providers = loadProviders(SHARED_PROVIDERS);
});

afterAll(async () => {
  if (container) await container.stop();
});

async function withFreshDb<T>(fn: (pool: Pool) => Promise<T>): Promise<T> {
  const dbName = `addition_${Math.random().toString(36).slice(2, 10)}`;
  const admin = new Pool({ connectionString: databaseUrl });
  try {
    await admin.query(`CREATE DATABASE ${dbName}`);
  } finally {
    await admin.end();
  }
  const u = new URL(databaseUrl);
  u.pathname = '/' + dbName;
  const freshUrl = u.toString();
  await bootstrap(freshUrl, SHARED_SCHEMA);
  const pool = new Pool({ connectionString: freshUrl });
  try {
    return await fn(pool);
  } finally {
    await pool.end();
    const cleanup = new Pool({ connectionString: databaseUrl });
    try {
      await cleanup.query(
        `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`,
        [dbName],
      );
      await cleanup.query(`DROP DATABASE IF EXISTS ${dbName}`);
    } finally {
      await cleanup.end();
    }
  }
}

async function seedProviderConfig(
  pool: Pool,
  providerId: string,
  clientId = 'test-client-id',
  clientSecret = 'test-client-secret',
  scopes: string[] = [],
): Promise<void> {
  await pool.query(
    `INSERT INTO _addition.provider_configs
       (provider_id, client_id, client_secret_encrypted, scopes)
     VALUES ($1, $2, $3, $4)`,
    [providerId, clientId, encrypt(clientSecret, TEST_KEY), scopes],
  );
}

function mkFetch(responses: Array<{ status: number; body: unknown; headers?: Record<string, string> }>): typeof fetch {
  let i = 0;
  const spy = vi.fn(async (_url: any, _init?: any) => {
    const r = responses[Math.min(i++, responses.length - 1)];
    const headers = new Headers(r.headers ?? { 'content-type': 'application/json' });
    return {
      status: r.status,
      ok: r.status >= 200 && r.status < 300,
      headers,
      text: async () => (typeof r.body === 'string' ? r.body : JSON.stringify(r.body)),
      json: async () => (typeof r.body === 'string' ? JSON.parse(r.body) : r.body),
    } as unknown as Response;
  });
  return spy as unknown as typeof fetch;
}

function deps(pool: Pool, fetchImpl?: typeof fetch): OAuthDeps {
  return {
    pool,
    providers,
    encryptionKey: TEST_KEY,
    callbackUrl: CALLBACK_URL,
    fetch: fetchImpl ?? mkFetch([{ status: 200, body: {} }]),
  };
}

describe('oauth engine — getAuthUrl', () => {
  it('produces an authorize URL with client_id, redirect_uri, state, scope, response_type=code', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'github' },
        deps(pool),
      );
      const parsed = new URL(url);
      expect(parsed.origin + parsed.pathname).toBe('https://github.com/login/oauth/authorize');
      expect(parsed.searchParams.get('client_id')).toBe('gh-client');
      expect(parsed.searchParams.get('redirect_uri')).toBe(CALLBACK_URL);
      expect(parsed.searchParams.get('response_type')).toBe('code');
      expect(parsed.searchParams.get('state')).toBeTruthy();
      expect(parsed.searchParams.get('scope')).toBeTruthy();
    });
  });

  it('joins scopes with the provider scopes_delimiter (space for Google)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'google', scopes: ['a', 'b', 'c'] },
        deps(pool),
      );
      const scope = new URL(url).searchParams.get('scope');
      expect(scope).toBe('a b c');
    });
  });

  it('joins scopes with COMMA for Slack (scopes_delimiter: comma)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'slack', 'sl-client', 'sl-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'slack', scopes: ['chat:write', 'users:read'] },
        deps(pool),
      );
      const scope = new URL(url).searchParams.get('scope');
      expect(scope).toBe('chat:write,users:read');
    });
  });

  it('custom scopes override default_scopes', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'github', scopes: ['only_this'] },
        deps(pool),
      );
      expect(new URL(url).searchParams.get('scope')).toBe('only_this');
    });
  });

  it('when no custom scopes, falls back to provider default_scopes', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'github' },
        deps(pool),
      );
      const scope = new URL(url).searchParams.get('scope');
      // github.yaml default_scopes = [repo, read:user, user:email]
      expect(scope).toBe('repo read:user user:email');
    });
  });

  it('applies authorize_params from YAML (Google access_type=offline, prompt=consent)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'google' },
        deps(pool),
      );
      const parsed = new URL(url);
      expect(parsed.searchParams.get('access_type')).toBe('offline');
      expect(parsed.searchParams.get('prompt')).toBe('consent');
    });
  });

  it('when pkce_supported: true, includes code_challenge + code_challenge_method=S256', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'google' },
        deps(pool),
      );
      const parsed = new URL(url);
      expect(parsed.searchParams.get('code_challenge')).toBeTruthy();
      expect(parsed.searchParams.get('code_challenge_method')).toBe('S256');
    });
  });

  it('when pkce_supported: false (GitHub), no code_challenge in URL', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'github' },
        deps(pool),
      );
      const parsed = new URL(url);
      expect(parsed.searchParams.get('code_challenge')).toBeNull();
      expect(parsed.searchParams.get('code_challenge_method')).toBeNull();
    });
  });

  it('stores state row with user_id, provider_id, redirect_uri, scopes_requested', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const url = await getAuthUrl(
        { userId: 'user-42', providerId: 'github', scopes: ['repo'] },
        deps(pool),
      );
      const state = new URL(url).searchParams.get('state')!;
      const { rows } = await pool.query(
        `SELECT user_id, provider_id, redirect_uri, scopes_requested, code_verifier, metadata
           FROM _addition.oauth_state WHERE state = $1`,
        [state],
      );
      expect(rows).toHaveLength(1);
      expect(rows[0].user_id).toBe('user-42');
      expect(rows[0].provider_id).toBe('github');
      expect(rows[0].redirect_uri).toBe(CALLBACK_URL);
      expect(rows[0].scopes_requested).toEqual(['repo']);
      expect(rows[0].code_verifier).toBeNull(); // github pkce_supported: false
    });
  });

  it('stores code_verifier when PKCE is used, and challenge matches verifier', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'google' },
        deps(pool),
      );
      const parsed = new URL(url);
      const state = parsed.searchParams.get('state')!;
      const challenge = parsed.searchParams.get('code_challenge')!;
      const { rows } = await pool.query(
        `SELECT code_verifier FROM _addition.oauth_state WHERE state = $1`,
        [state],
      );
      expect(rows[0].code_verifier).toBeTruthy();
      // Verify the challenge is SHA256(verifier), base64url.
      const expectedChallenge = createHash('sha256')
        .update(rows[0].code_verifier)
        .digest('base64url');
      expect(challenge).toBe(expectedChallenge);
    });
  });

  it('stores redirectAfter and metadata in oauth_state.metadata', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const url = await getAuthUrl(
        {
          userId: 'u1',
          providerId: 'github',
          redirectAfter: '/dashboard',
          metadata: { source: 'onboarding' },
        },
        deps(pool),
      );
      const state = new URL(url).searchParams.get('state')!;
      const { rows } = await pool.query(
        `SELECT metadata FROM _addition.oauth_state WHERE state = $1`,
        [state],
      );
      expect(rows[0].metadata.redirect_after).toBe('/dashboard');
      expect(rows[0].metadata.source).toBe('onboarding');
    });
  });

  it('throws when provider is not configured (not in provider_configs)', async () => {
    await withFreshDb(async (pool) => {
      // No seeding — github not configured.
      await expect(
        getAuthUrl({ userId: 'u1', providerId: 'github' }, deps(pool)),
      ).rejects.toThrow(/github/);
    });
  });

  it('generates a unique state per call', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const u1 = await getAuthUrl({ userId: 'u1', providerId: 'github' }, deps(pool));
      const u2 = await getAuthUrl({ userId: 'u1', providerId: 'github' }, deps(pool));
      const s1 = new URL(u1).searchParams.get('state');
      const s2 = new URL(u2).searchParams.get('state');
      expect(s1).not.toBe(s2);
    });
  });
});

describe('oauth engine — handleCallback', () => {
  it('valid state: exchanges code, stores encrypted tokens, deletes state, returns success', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const fetchImpl = mkFetch([
        {
          status: 200,
          body: { access_token: 'gh-access', token_type: 'bearer', scope: 'repo,user:email' },
        },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'github' }, d);
      const state = new URL(url).searchParams.get('state')!;

      const result = await handleCallback({ code: 'the-code', state }, d);

      expect(result.success).toBe(true);
      expect(result.userId).toBe('u1');
      expect(result.providerId).toBe('github');
      expect(result.connectionId).toBeTruthy();

      // State row deleted.
      const { rows: stateRows } = await pool.query(
        `SELECT 1 FROM _addition.oauth_state WHERE state = $1`,
        [state],
      );
      expect(stateRows).toHaveLength(0);

      // Connection row created with encrypted token.
      const { rows: conn } = await pool.query(
        `SELECT user_id, provider_id, access_token_encrypted, refresh_token_encrypted
           FROM _addition.connections WHERE id = $1`,
        [result.connectionId],
      );
      expect(conn).toHaveLength(1);
      expect(conn[0].user_id).toBe('u1');
      expect(conn[0].provider_id).toBe('github');
      expect(conn[0].access_token_encrypted).not.toContain('gh-access');
      expect(decrypt(conn[0].access_token_encrypted, TEST_KEY)).toBe('gh-access');
    });
  });

  it('unknown state → returns {success: false, error: "invalid_state"}', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const result = await handleCallback(
        { code: 'c', state: 'no-such-state' },
        deps(pool),
      );
      expect(result.success).toBe(false);
      expect(result.error).toBe('invalid_state');
    });
  });

  it('expired state → returns {success: false, error: "invalid_state"}', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const d = deps(pool);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'github' }, d);
      const state = new URL(url).searchParams.get('state')!;
      // Force-expire the state.
      await pool.query(
        `UPDATE _addition.oauth_state SET expires_at = NOW() - INTERVAL '1 minute'
           WHERE state = $1`,
        [state],
      );
      const result = await handleCallback({ code: 'c', state }, d);
      expect(result.success).toBe(false);
      expect(result.error).toBe('invalid_state');
    });
  });

  it('provider error in query (error=access_denied) → returns {success:false, error: code}', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const d = deps(pool);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'github' }, d);
      const state = new URL(url).searchParams.get('state')!;
      const result = await handleCallback(
        { error: 'access_denied', state },
        d,
      );
      expect(result.success).toBe(false);
      expect(result.error).toBe('access_denied');
      // State row should be cleaned up on provider error too.
      const { rows } = await pool.query(
        `SELECT 1 FROM _addition.oauth_state WHERE state = $1`,
        [state],
      );
      expect(rows).toHaveLength(0);
    });
  });

  it('GitHub adapter: sends Accept: application/json header on token exchange', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const spy = vi.fn(async () => ({
        status: 200,
        ok: true,
        headers: new Headers({ 'content-type': 'application/json' }),
        text: async () => JSON.stringify({ access_token: 't', token_type: 'bearer' }),
        json: async () => ({ access_token: 't', token_type: 'bearer' }),
      })) as unknown as typeof fetch;
      const d = deps(pool, spy);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'github' }, d);
      const state = new URL(url).searchParams.get('state')!;
      await handleCallback({ code: 'c', state }, d);
      const call = (spy as unknown as { mock: { calls: any[][] } }).mock.calls[0];
      const init = call[1];
      expect(init.headers.Accept).toBe('application/json');
    });
  });

  it('Slack adapter: detects {ok: false} and returns error even on 200', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'slack', 'sl-client', 'sl-secret');
      const fetchImpl = mkFetch([
        { status: 200, body: { ok: false, error: 'invalid_code' } },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'slack' }, d);
      const state = new URL(url).searchParams.get('state')!;
      const result = await handleCallback({ code: 'c', state }, d);
      expect(result.success).toBe(false);
      expect(result.error).toBe('invalid_code');
      // No connection row created.
      const { rows } = await pool.query(
        `SELECT 1 FROM _addition.connections WHERE user_id = 'u1'`,
      );
      expect(rows).toHaveLength(0);
    });
  });

  it('Slack happy path: extracts tokens from authed_user.* via token_response_mapping', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'slack', 'sl-client', 'sl-secret');
      const fetchImpl = mkFetch([
        {
          status: 200,
          body: {
            ok: true,
            authed_user: {
              id: 'U1',
              access_token: 'xoxp-user-token',
              refresh_token: 'xoxp-refresh',
              expires_in: 43200,
            },
          },
        },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'slack' }, d);
      const state = new URL(url).searchParams.get('state')!;
      const result = await handleCallback({ code: 'c', state }, d);
      expect(result.success).toBe(true);
      const { rows } = await pool.query(
        `SELECT access_token_encrypted, refresh_token_encrypted, token_expires_at
           FROM _addition.connections WHERE id = $1`,
        [result.connectionId],
      );
      expect(decrypt(rows[0].access_token_encrypted, TEST_KEY)).toBe('xoxp-user-token');
      expect(decrypt(rows[0].refresh_token_encrypted, TEST_KEY)).toBe('xoxp-refresh');
      expect(rows[0].token_expires_at).toBeTruthy();
    });
  });

  it('PKCE provider (google): code_verifier from stored state is sent to token_url', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const spy = vi.fn(async () => ({
        status: 200,
        ok: true,
        headers: new Headers({ 'content-type': 'application/json' }),
        text: async () =>
          JSON.stringify({
            access_token: 'g-access',
            refresh_token: 'g-refresh',
            expires_in: 3600,
          }),
        json: async () => ({
          access_token: 'g-access',
          refresh_token: 'g-refresh',
          expires_in: 3600,
        }),
      })) as unknown as typeof fetch;
      const d = deps(pool, spy);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'google' }, d);
      const state = new URL(url).searchParams.get('state')!;
      const { rows } = await pool.query(
        `SELECT code_verifier FROM _addition.oauth_state WHERE state = $1`,
        [state],
      );
      const verifier = rows[0].code_verifier;
      await handleCallback({ code: 'c', state }, d);
      const call = (spy as unknown as { mock: { calls: any[][] } }).mock.calls[0];
      const init = call[1];
      expect(init.body).toContain(`code_verifier=${encodeURIComponent(verifier)}`);
    });
  });

  it('writes audit_log row on successful callback', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const fetchImpl = mkFetch([
        { status: 200, body: { access_token: 't', token_type: 'bearer' } },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'github' }, d);
      const state = new URL(url).searchParams.get('state')!;
      await handleCallback({ code: 'c', state }, d);
      const { rows } = await pool.query(
        `SELECT event_type FROM _addition.audit_log WHERE user_id = 'u1' ORDER BY id`,
      );
      const types = rows.map((r) => r.event_type);
      expect(types).toContain('connection.created');
    });
  });

  it('computes token_expires_at from expires_in', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const fetchImpl = mkFetch([
        {
          status: 200,
          body: { access_token: 'a', refresh_token: 'r', expires_in: 3600 },
        },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'google' }, d);
      const state = new URL(url).searchParams.get('state')!;
      const before = Date.now();
      const result = await handleCallback({ code: 'c', state }, d);
      const after = Date.now();
      const { rows } = await pool.query(
        `SELECT token_expires_at FROM _addition.connections WHERE id = $1`,
        [result.connectionId],
      );
      const expiresMs = rows[0].token_expires_at.getTime();
      // Should be roughly "now + 3600s".
      expect(expiresMs).toBeGreaterThanOrEqual(before + 3599_000);
      expect(expiresMs).toBeLessThanOrEqual(after + 3601_000);
    });
  });

  it('token exchange 4xx response → returns {success: false, error: code}', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google', 'g-client', 'g-secret');
      const fetchImpl = mkFetch([
        {
          status: 400,
          body: { error: 'invalid_grant', error_description: 'bad code' },
        },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'google' }, d);
      const state = new URL(url).searchParams.get('state')!;
      const result = await handleCallback({ code: 'c', state }, d);
      expect(result.success).toBe(false);
      expect(result.error).toBe('invalid_grant');
    });
  });

  it('POST body contains client_id, client_secret, code, redirect_uri, grant_type', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const spy = vi.fn(async () => ({
        status: 200,
        ok: true,
        headers: new Headers({ 'content-type': 'application/json' }),
        text: async () => JSON.stringify({ access_token: 't', token_type: 'bearer' }),
        json: async () => ({ access_token: 't', token_type: 'bearer' }),
      })) as unknown as typeof fetch;
      const d = deps(pool, spy);
      const url = await getAuthUrl({ userId: 'u1', providerId: 'github' }, d);
      const state = new URL(url).searchParams.get('state')!;
      await handleCallback({ code: 'the-code', state }, d);
      const call = (spy as unknown as { mock: { calls: any[][] } }).mock.calls[0];
      const init = call[1];
      expect(init.method).toBe('POST');
      expect(init.body).toContain('grant_type=authorization_code');
      expect(init.body).toContain('code=the-code');
      expect(init.body).toContain('client_id=gh-client');
      expect(init.body).toContain('client_secret=gh-secret');
      expect(init.body).toContain(
        `redirect_uri=${encodeURIComponent(CALLBACK_URL)}`,
      );
    });
  });

  it('returns redirectAfter from state metadata on success', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github', 'gh-client', 'gh-secret');
      const fetchImpl = mkFetch([
        { status: 200, body: { access_token: 't', token_type: 'bearer' } },
      ]);
      const d = deps(pool, fetchImpl);
      const url = await getAuthUrl(
        { userId: 'u1', providerId: 'github', redirectAfter: '/welcome' },
        d,
      );
      const state = new URL(url).searchParams.get('state')!;
      const result = await handleCallback({ code: 'c', state }, d);
      expect(result.redirectAfter).toBe('/welcome');
    });
  });
});
