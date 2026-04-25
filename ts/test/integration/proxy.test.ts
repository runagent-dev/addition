import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { readFileSync } from 'node:fs';
import { parse as parseYaml } from 'yaml';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { bootstrap } from '../../src/schema.js';
import { loadProviders } from '../../src/providers.js';
import { encrypt } from '../../src/crypto.js';
import { storeConnection } from '../../src/tokens.js';
import {
  ConnectionNotFoundError,
  ConnectionExpiredError,
  ProxyError,
  ProviderNotFoundError,
} from '../../src/errors.js';
import { proxy } from '../../src/proxy.js';
import type { ProviderRegistry } from '../../src/types.js';
import type { ProxyDeps } from '../../src/proxy.js';

const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');
const SHARED_SCHEMA = join(__dirname, '..', '..', '..', 'shared', 'schema');
const SHARED_FIXTURES = join(__dirname, '..', '..', '..', 'shared', 'test-fixtures');
const TEST_KEY = 'c'.repeat(64);

let container: StartedPostgreSqlContainer;
let databaseUrl: string;
let providers: ProviderRegistry;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_proxy_test')
    .start();
  databaseUrl = container.getConnectionUri();
  providers = loadProviders(SHARED_PROVIDERS);
});

afterAll(async () => {
  if (container) await container.stop();
});

async function withFreshDb<T>(fn: (pool: Pool) => Promise<T>): Promise<T> {
  const dbName = `proxy_${Math.random().toString(36).slice(2, 10)}`;
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
        `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, [dbName],
      );
      await cleanup.query(`DROP DATABASE IF EXISTS ${dbName}`);
    } finally {
      await cleanup.end();
    }
  }
}

async function seedProviderConfig(pool: Pool, providerId: string): Promise<void> {
  await pool.query(
    `INSERT INTO _addition.provider_configs (provider_id, client_id, client_secret_encrypted, scopes)
     VALUES ($1, $2, $3, $4)`,
    [providerId, 'test-client', encrypt('test-secret', TEST_KEY), []],
  );
}

function mkFetch(
  responses: Array<{
    status: number;
    body: unknown;
    headers?: Record<string, string>;
    throws?: boolean;
  }>,
): typeof fetch {
  let i = 0;
  const spy = vi.fn(async (_url: unknown, _init?: unknown) => {
    const r = responses[Math.min(i, responses.length - 1)];
    i++;
    if (r.throws) throw new TypeError('fetch failed');
    return {
      status: r.status,
      ok: r.status >= 200 && r.status < 300,
      headers: new Headers({ 'content-type': 'application/json', ...(r.headers ?? {}) }),
      text: async () => (typeof r.body === 'string' ? r.body : JSON.stringify(r.body)),
      json: async () => r.body,
    } as unknown as Response;
  });
  return spy as unknown as typeof fetch;
}

const NO_SLEEP = () => Promise.resolve();

function deps(pool: Pool, fetchImpl: typeof fetch, sleep = NO_SLEEP): ProxyDeps {
  return { pool, providers, encryptionKey: TEST_KEY, fetch: fetchImpl, sleep };
}

describe('proxy — auth header injection', () => {
  it('injects Authorization: Bearer <access_token> from the active connection', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 'my-token', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: { login: 'u1' } }]);
      await proxy({ userId: 'u1', providerId: 'github', method: 'GET', path: '/user' }, deps(pool, spy));
      const call = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[0];
      const init = call[1] as { headers: Record<string, string> };
      expect(init.headers.Authorization).toBe('Bearer my-token');
    });
  });

  it('uses provider api.base_url + path as the request URL', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: {} }]);
      await proxy({ userId: 'u1', providerId: 'github', method: 'GET', path: '/repos/owner/repo' }, deps(pool, spy));
      const url = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[0][0] as string;
      expect(url).toContain('api.github.com');
      expect(url).toContain('/repos/owner/repo');
    });
  });

  it('appends query params to the URL', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: [] }]);
      await proxy({
        userId: 'u1', providerId: 'github', method: 'GET', path: '/user/repos',
        params: { per_page: 10, sort: 'updated' },
      }, deps(pool, spy));
      const url = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[0][0] as string;
      expect(url).toContain('per_page=10');
      expect(url).toContain('sort=updated');
    });
  });

  it('serialises object body as JSON and sets Content-Type', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 201, body: { id: 1 } }]);
      await proxy({
        userId: 'u1', providerId: 'github', method: 'POST', path: '/repos/o/r/issues',
        body: { title: 'Bug' },
      }, deps(pool, spy));
      const init = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[0][1] as { body: string; headers: Record<string, string> };
      expect(JSON.parse(init.body)).toEqual({ title: 'Bug' });
      expect(init.headers['Content-Type']).toContain('application/json');
    });
  });

  it('merges caller-supplied headers with the auth header', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: {} }]);
      await proxy({
        userId: 'u1', providerId: 'github', method: 'GET', path: '/user',
        headers: { 'X-Custom': 'value' },
      }, deps(pool, spy));
      const init = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[0][1] as { headers: Record<string, string> };
      expect(init.headers.Authorization).toBeTruthy();
      expect(init.headers['X-Custom']).toBe('value');
    });
  });

  it('returns parsed JSON body and status in ProxyResponse', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: { login: 'sawradip' } }]);
      const result = await proxy(
        { userId: 'u1', providerId: 'github', method: 'GET', path: '/user' },
        deps(pool, spy),
      );
      expect(result.status).toBe(200);
      expect((result.body as { login: string }).login).toBe('sawradip');
    });
  });
});

describe('proxy — 401 / token refresh', () => {
  it('on 401 with refresh token: refreshes once, retries, returns success', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await storeConnection(pool, 'u1', 'google', {
        accessToken: 'old-access', refreshToken: 'refresh-tok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      const spy = mkFetch([
        { status: 401, body: { error: 'invalid_credentials' } },         // initial request fails
        { status: 200, body: { access_token: 'new-access', expires_in: 3600 } }, // token refresh
        { status: 200, body: { id: 'me' } },                              // retry succeeds
      ]);
      const result = await proxy(
        { userId: 'u1', providerId: 'google', method: 'GET', path: '/oauth2/v1/userinfo' },
        deps(pool, spy),
      );
      expect(spy).toHaveBeenCalledTimes(3);
      expect(result.status).toBe(200);
    });
  });

  it('on 401 retry the second request uses the new access token', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await storeConnection(pool, 'u1', 'google', {
        accessToken: 'old-access', refreshToken: 'refresh-tok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      const spy = mkFetch([
        { status: 401, body: {} },
        { status: 200, body: { access_token: 'brand-new', expires_in: 3600 } },
        { status: 200, body: {} },
      ]);
      await proxy(
        { userId: 'u1', providerId: 'google', method: 'GET', path: '/oauth2/v1/userinfo' },
        deps(pool, spy),
      );
      const retryInit = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[2][1] as { headers: Record<string, string> };
      expect(retryInit.headers.Authorization).toBe('Bearer brand-new');
    });
  });

  it('on 401 with no refresh token: throws ConnectionExpiredError, no retry', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 'old', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 401, body: {} }]);
      await expect(
        proxy({ userId: 'u1', providerId: 'github', method: 'GET', path: '/user' }, deps(pool, spy)),
      ).rejects.toThrow(ConnectionExpiredError);
      expect(spy).toHaveBeenCalledTimes(1);
    });
  });
});

describe('proxy — 429 retry', () => {
  it('retries up to 3 times on 429, then returns the final response', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([
        { status: 429, body: {}, headers: { 'Retry-After': '1' } },
        { status: 429, body: {}, headers: { 'Retry-After': '1' } },
        { status: 200, body: { ok: true } },
      ]);
      const result = await proxy(
        { userId: 'u1', providerId: 'github', method: 'GET', path: '/user' },
        deps(pool, spy),
      );
      expect(spy).toHaveBeenCalledTimes(3);
      expect(result.status).toBe(200);
    });
  });

  it('sleep is called with Retry-After ms on each 429', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([
        { status: 429, body: {}, headers: { 'Retry-After': '2' } },
        { status: 200, body: {} },
      ]);
      const sleepSpy = vi.fn(() => Promise.resolve());
      await proxy(
        { userId: 'u1', providerId: 'github', method: 'GET', path: '/user' },
        deps(pool, spy, sleepSpy),
      );
      expect(sleepSpy).toHaveBeenCalledWith(2000);
    });
  });

  it('stops retrying after 3 attempts and returns the last 429 response', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      // Always 429 — should give up after 3 retries.
      const spy = mkFetch([
        { status: 429, body: {}, headers: { 'Retry-After': '1' } },
        { status: 429, body: {}, headers: { 'Retry-After': '1' } },
        { status: 429, body: {}, headers: { 'Retry-After': '1' } },
        { status: 429, body: {}, headers: { 'Retry-After': '1' } },
      ]);
      const result = await proxy(
        { userId: 'u1', providerId: 'github', method: 'GET', path: '/user' },
        deps(pool, spy),
      );
      expect(result.status).toBe(429);
      expect(spy).toHaveBeenCalledTimes(4); // 1 initial + 3 retries
    });
  });
});

describe('proxy — adapter error detection', () => {
  it('Slack ok:false on 200 throws ProxyError', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'slack');
      await storeConnection(pool, 'u1', 'slack',
        { accessToken: 'xoxp-token', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: { ok: false, error: 'channel_not_found' } }]);
      await expect(
        proxy({ userId: 'u1', providerId: 'slack', method: 'GET', path: '/conversations.list' }, deps(pool, spy)),
      ).rejects.toThrow(ProxyError);
    });
  });

  it('ProxyError carries the adapter error code', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'slack');
      await storeConnection(pool, 'u1', 'slack',
        { accessToken: 'xoxp-token', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: { ok: false, error: 'not_authed' } }]);
      const err = await proxy(
        { userId: 'u1', providerId: 'slack', method: 'GET', path: '/conversations.list' },
        deps(pool, spy),
      ).catch((e) => e);
      expect(err).toBeInstanceOf(ProxyError);
      expect((err as ProxyError).code).toBe('not_authed');
    });
  });
});

describe('proxy — network error retry', () => {
  it('retries once after a network error, succeeds on second attempt', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([
        { status: 0, body: null, throws: true },
        { status: 200, body: { ok: true } },
      ]);
      const result = await proxy(
        { userId: 'u1', providerId: 'github', method: 'GET', path: '/user' },
        deps(pool, spy),
      );
      expect(spy).toHaveBeenCalledTimes(2);
      expect(result.status).toBe(200);
    });
  });

  it('rethrows after two consecutive network errors', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([
        { status: 0, body: null, throws: true },
        { status: 0, body: null, throws: true },
      ]);
      await expect(
        proxy({ userId: 'u1', providerId: 'github', method: 'GET', path: '/user' }, deps(pool, spy)),
      ).rejects.toThrow();
    });
  });
});

describe('proxy — connection not found', () => {
  it('throws ConnectionNotFoundError immediately when no active connection exists', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      const spy = mkFetch([{ status: 200, body: {} }]);
      await expect(
        proxy({ userId: 'nobody', providerId: 'github', method: 'GET', path: '/user' }, deps(pool, spy)),
      ).rejects.toThrow(ConnectionNotFoundError);
      expect(spy).not.toHaveBeenCalled();
    });
  });

  it('throws ConnectionNotFoundError when explicit connectionId does not exist', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      const spy = mkFetch([{ status: 200, body: {} }]);
      await expect(
        proxy({
          userId: 'u1', providerId: 'github', method: 'GET', path: '/user',
          connectionId: '00000000-0000-0000-0000-000000000000',
        }, deps(pool, spy)),
      ).rejects.toThrow(ConnectionNotFoundError);
      expect(spy).not.toHaveBeenCalled();
    });
  });

  it('throws ConnectionNotFoundError when connectionId belongs to a different user (tenant isolation)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      const connId = await storeConnection(pool, 'owner', 'github',
        { accessToken: 't', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = mkFetch([{ status: 200, body: {} }]);
      await expect(
        proxy({
          userId: 'attacker', providerId: 'github', method: 'GET', path: '/user',
          connectionId: connId,
        }, deps(pool, spy)),
      ).rejects.toThrow(ConnectionNotFoundError);
      expect(spy).not.toHaveBeenCalled();
    });
  });
});

describe('proxy — provider guard', () => {
  it('throws ProviderNotFoundError for an unknown providerId', async () => {
    await withFreshDb(async (pool) => {
      const spy = mkFetch([{ status: 200, body: {} }]);
      await expect(
        proxy({ userId: 'u1', providerId: 'nonexistent', method: 'GET', path: '/foo' }, deps(pool, spy)),
      ).rejects.toThrow(ProviderNotFoundError);
      expect(spy).not.toHaveBeenCalled();
    });
  });
});

// ---------- Conformance (shared/test-fixtures/proxy_requests.yaml) ----------

interface ProxyScenario {
  name: string;
  description: string;
  provider: string;
  method: string;
  path: string;
  params: Record<string, unknown>;
  body: unknown;
  mock_response: { status: number; body: unknown };
  expected: {
    status: number;
    auth_header?: string;
    auth_prefix?: string;
    url_query_params?: Record<string, string>;
    request_has_body?: boolean;
  };
}

describe('proxy — conformance (proxy_requests.yaml)', () => {
  const fixture = parseYaml(
    readFileSync(join(SHARED_FIXTURES, 'proxy_requests.yaml'), 'utf8'),
  ) as { scenarios: ProxyScenario[] };

  for (const scenario of fixture.scenarios) {
    it(scenario.name, async () => {
      await withFreshDb(async (pool) => {
        await seedProviderConfig(pool, scenario.provider);
        await storeConnection(pool, 'conformance-user', scenario.provider, {
          accessToken: 'conformance-token',
          refreshToken: null,
          expiresIn: null,
          scopesGranted: [],
        }, TEST_KEY);

        const spy = mkFetch([scenario.mock_response]);
        const result = await proxy({
          userId: 'conformance-user',
          providerId: scenario.provider,
          method: scenario.method,
          path: scenario.path,
          params: scenario.params as Record<string, string | number | boolean>,
          body: scenario.body ?? undefined,
        }, deps(pool, spy));

        expect(result.status).toBe(scenario.expected.status);

        const calls = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls;
        const url = calls[0][0] as string;
        const init = calls[0][1] as { headers: Record<string, string>; body?: string };

        if (scenario.expected.auth_header && scenario.expected.auth_prefix) {
          expect(init.headers[scenario.expected.auth_header]).toMatch(
            new RegExp(`^${scenario.expected.auth_prefix} `),
          );
        }
        if (scenario.expected.url_query_params) {
          const parsed = new URL(url);
          for (const [k, v] of Object.entries(scenario.expected.url_query_params)) {
            expect(parsed.searchParams.get(k)).toBe(v);
          }
        }
        if (scenario.expected.request_has_body) {
          expect(init.body).toBeTruthy();
        }
      });
    });
  }
});
