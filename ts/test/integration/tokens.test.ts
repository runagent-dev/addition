import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { readFileSync } from 'node:fs';
import { parse as parseYaml } from 'yaml';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { bootstrap } from '../../src/schema.js';
import { loadProviders } from '../../src/providers.js';
import { encrypt, decrypt } from '../../src/crypto.js';
import { TokenRefreshError } from '../../src/errors.js';
import {
  storeConnection,
  getConnection,
  refreshConnection,
  listConnections,
  disconnect,
} from '../../src/tokens.js';
import type { ProviderRegistry } from '../../src/types.js';
import type { TokenDeps, StoredTokens } from '../../src/tokens.js';

const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');
const SHARED_SCHEMA = join(__dirname, '..', '..', '..', 'shared', 'schema');
const SHARED_FIXTURES = join(__dirname, '..', '..', '..', 'shared', 'test-fixtures');
const TEST_KEY = 'b'.repeat(64); // 32 bytes of 0xbb

let container: StartedPostgreSqlContainer;
let databaseUrl: string;
let providers: ProviderRegistry;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_tokens_test')
    .start();
  databaseUrl = container.getConnectionUri();
  providers = loadProviders(SHARED_PROVIDERS);
});

afterAll(async () => {
  if (container) await container.stop();
});

async function withFreshDb<T>(fn: (pool: Pool) => Promise<T>): Promise<T> {
  const dbName = `tokens_${Math.random().toString(36).slice(2, 10)}`;
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
): Promise<void> {
  await pool.query(
    `INSERT INTO _addition.provider_configs
       (provider_id, client_id, client_secret_encrypted, scopes)
     VALUES ($1, $2, $3, $4)`,
    [providerId, clientId, encrypt(clientSecret, TEST_KEY), []],
  );
}

function mkFetch(
  responses: Array<{ status: number; body: unknown }>,
): typeof fetch {
  let i = 0;
  const spy = vi.fn(async (_url: unknown, _init?: unknown) => {
    const r = responses[Math.min(i++, responses.length - 1)];
    return {
      status: r.status,
      ok: r.status >= 200 && r.status < 300,
      headers: new Headers({ 'content-type': 'application/json' }),
      text: async () => (typeof r.body === 'string' ? r.body : JSON.stringify(r.body)),
      json: async () => r.body,
    } as unknown as Response;
  });
  return spy as unknown as typeof fetch;
}

function deps(pool: Pool, fetchImpl?: typeof fetch): TokenDeps {
  return {
    pool,
    providers,
    encryptionKey: TEST_KEY,
    fetch: fetchImpl ?? mkFetch([{ status: 200, body: {} }]),
  };
}

const SAMPLE_TOKENS: StoredTokens = {
  accessToken: 'plaintext-access',
  refreshToken: 'plaintext-refresh',
  expiresIn: 3600,
  scopesGranted: ['read', 'write'],
};

describe('token manager — storeConnection', () => {
  it('DB row has only ciphertext — access_token not in plaintext', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const { rows } = await pool.query(
        `SELECT access_token_encrypted, refresh_token_encrypted FROM _addition.connections WHERE id = $1`,
        [id],
      );
      expect(rows[0].access_token_encrypted).not.toContain('plaintext-access');
      expect(rows[0].refresh_token_encrypted).not.toContain('plaintext-refresh');
    });
  });

  it('encrypts both access and refresh tokens', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const { rows } = await pool.query(
        `SELECT access_token_encrypted, refresh_token_encrypted FROM _addition.connections WHERE id = $1`,
        [id],
      );
      expect(decrypt(rows[0].access_token_encrypted, TEST_KEY)).toBe('plaintext-access');
      expect(decrypt(rows[0].refresh_token_encrypted, TEST_KEY)).toBe('plaintext-refresh');
    });
  });

  it('stores null refresh_token when not provided', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      const id = await storeConnection(
        pool, 'u1', 'github',
        { accessToken: 'gh-token', refreshToken: null, expiresIn: null, scopesGranted: [] },
        TEST_KEY,
      );
      const { rows } = await pool.query(
        `SELECT refresh_token_encrypted FROM _addition.connections WHERE id = $1`, [id],
      );
      expect(rows[0].refresh_token_encrypted).toBeNull();
    });
  });

  it('returns a UUID connection id', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      expect(id).toMatch(/^[0-9a-f-]{36}$/);
    });
  });
});

describe('token manager — getConnection', () => {
  it('returns decrypted accessToken and refreshToken in memory', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const conn = await getConnection(pool, 'u1', 'google', TEST_KEY);
      expect(conn).not.toBeNull();
      expect(conn!.accessToken).toBe('plaintext-access');
      expect(conn!.refreshToken).toBe('plaintext-refresh');
    });
  });

  it('returns null when userId/providerId has no connection', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const conn = await getConnection(pool, 'nobody', 'google', TEST_KEY);
      expect(conn).toBeNull();
    });
  });

  it('returns the most recently created connection when multiple exist', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await storeConnection(pool, 'u1', 'google',
        { accessToken: 'old-token', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      await storeConnection(pool, 'u1', 'google',
        { accessToken: 'new-token', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const conn = await getConnection(pool, 'u1', 'google', TEST_KEY);
      expect(conn!.accessToken).toBe('new-token');
    });
  });

  it('includes tokenExpiresAt when expiresIn was provided', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const before = Date.now();
      await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const after = Date.now();
      const conn = await getConnection(pool, 'u1', 'google', TEST_KEY);
      expect(conn!.tokenExpiresAt).not.toBeNull();
      const ms = conn!.tokenExpiresAt!.getTime();
      expect(ms).toBeGreaterThanOrEqual(before + 3599_000);
      expect(ms).toBeLessThanOrEqual(after + 3601_000);
    });
  });
});

describe('token manager — refreshConnection', () => {
  it('POSTs grant_type=refresh_token with the decrypted refresh_token', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const spy = vi.fn(async () => ({
        status: 200, ok: true,
        headers: new Headers({ 'content-type': 'application/json' }),
        text: async () => JSON.stringify({ access_token: 'new-access', expires_in: 3600 }),
        json: async () => ({ access_token: 'new-access', expires_in: 3600 }),
      })) as unknown as typeof fetch;
      await refreshConnection(id, deps(pool, spy));
      const call = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls[0];
      const body = (call[1] as { body: string }).body;
      expect(body).toContain('grant_type=refresh_token');
      expect(body).toContain(`refresh_token=${encodeURIComponent('plaintext-refresh')}`);
    });
  });

  it('updates the row with new encrypted access token and new expires_at', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 200, body: { access_token: 'refreshed', expires_in: 7200 } }]);
      await refreshConnection(id, deps(pool, fetchImpl));
      const { rows } = await pool.query(
        `SELECT access_token_encrypted, token_expires_at FROM _addition.connections WHERE id = $1`, [id],
      );
      expect(decrypt(rows[0].access_token_encrypted, TEST_KEY)).toBe('refreshed');
      expect(rows[0].token_expires_at).not.toBeNull();
    });
  });

  it('keeps existing refresh_token when provider does not return a new one', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 200, body: { access_token: 'refreshed', expires_in: 3600 } }]);
      await refreshConnection(id, deps(pool, fetchImpl));
      const { rows } = await pool.query(
        `SELECT refresh_token_encrypted FROM _addition.connections WHERE id = $1`, [id],
      );
      expect(decrypt(rows[0].refresh_token_encrypted, TEST_KEY)).toBe('plaintext-refresh');
    });
  });

  it('writes token.refresh_succeeded audit row on success', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 200, body: { access_token: 'refreshed', expires_in: 3600 } }]);
      await refreshConnection(id, deps(pool, fetchImpl));
      const { rows } = await pool.query(
        `SELECT event_type FROM _addition.audit_log WHERE connection_id = $1`, [id],
      );
      expect(rows.some((r) => r.event_type === 'token.refresh_succeeded')).toBe(true);
    });
  });

  it('is a no-op when provider has refresh_supported: false (GitHub)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      // GitHub: refresh_supported: false
      const id = await storeConnection(pool, 'u1', 'github',
        { accessToken: 'gh-access', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = vi.fn() as unknown as typeof fetch;
      const result = await refreshConnection(id, deps(pool, spy));
      expect(spy).not.toHaveBeenCalled();
      expect(result.accessToken).toBe('gh-access');
    });
  });

  it('is a no-op when connection has no refresh_token (regardless of provider)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google',
        { accessToken: 'access', refreshToken: null, expiresIn: null, scopesGranted: [] }, TEST_KEY);
      const spy = vi.fn() as unknown as typeof fetch;
      const result = await refreshConnection(id, deps(pool, spy));
      expect(spy).not.toHaveBeenCalled();
      expect(result.accessToken).toBe('access');
    });
  });
});

describe('token manager — refresh failure', () => {
  it('sets status=refresh_failed on provider error response', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 400, body: { error: 'invalid_grant' } }]);
      await expect(refreshConnection(id, deps(pool, fetchImpl))).rejects.toThrow(TokenRefreshError);
      const { rows } = await pool.query(
        `SELECT status FROM _addition.connections WHERE id = $1`, [id],
      );
      expect(rows[0].status).toBe('refresh_failed');
    });
  });

  it('does NOT overwrite existing tokens when refresh fails', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 400, body: { error: 'invalid_grant' } }]);
      await expect(refreshConnection(id, deps(pool, fetchImpl))).rejects.toThrow(TokenRefreshError);
      const { rows } = await pool.query(
        `SELECT access_token_encrypted FROM _addition.connections WHERE id = $1`, [id],
      );
      expect(decrypt(rows[0].access_token_encrypted, TEST_KEY)).toBe('plaintext-access');
    });
  });

  it('writes token.refresh_failed audit row on failure', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 400, body: { error: 'invalid_grant' } }]);
      await expect(refreshConnection(id, deps(pool, fetchImpl))).rejects.toThrow(TokenRefreshError);
      const { rows } = await pool.query(
        `SELECT event_type FROM _addition.audit_log WHERE connection_id = $1`, [id],
      );
      expect(rows.some((r) => r.event_type === 'token.refresh_failed')).toBe(true);
    });
  });

  it('throws TokenRefreshError with the provider error code', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const id = await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const fetchImpl = mkFetch([{ status: 400, body: { error: 'invalid_grant', error_description: 'Token revoked' } }]);
      const err = await refreshConnection(id, deps(pool, fetchImpl)).catch((e) => e);
      expect(err).toBeInstanceOf(TokenRefreshError);
      expect((err as TokenRefreshError).code).toBe('invalid_grant');
    });
  });
});

describe('token manager — listConnections', () => {
  it('returns all connections for a userId, most recent first', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await seedProviderConfig(pool, 'github');
      await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      await storeConnection(pool, 'u1', 'github',
        { accessToken: 'gh', refreshToken: null, expiresIn: null, scopesGranted: ['repo'] }, TEST_KEY);
      const list = await listConnections(pool, 'u1');
      expect(list).toHaveLength(2);
      const providerIds = list.map((c) => c.providerId);
      expect(providerIds).toContain('google');
      expect(providerIds).toContain('github');
    });
  });

  it('returns empty array when user has no connections', async () => {
    await withFreshDb(async (pool) => {
      const list = await listConnections(pool, 'ghost-user');
      expect(list).toEqual([]);
    });
  });

  it('does not return plaintext tokens (metadata only)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const list = await listConnections(pool, 'u1');
      const entry = list[0] as Record<string, unknown>;
      expect(entry.accessToken).toBeUndefined();
      expect(entry.refreshToken).toBeUndefined();
    });
  });
});

describe('token manager — disconnect', () => {
  it('removes the connection row', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      await storeConnection(pool, 'u1', 'google', SAMPLE_TOKENS, TEST_KEY);
      const removed = await disconnect(pool, 'u1', 'google');
      expect(removed).toBe(true);
      const conn = await getConnection(pool, 'u1', 'google', TEST_KEY);
      expect(conn).toBeNull();
    });
  });

  it('returns false when no connection exists for userId/providerId', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const removed = await disconnect(pool, 'nobody', 'google');
      expect(removed).toBe(false);
    });
  });
});

// ---------- Conformance (shared/test-fixtures/token_refresh.yaml) ----------

interface RefreshScenario {
  name: string;
  description: string;
  provider: string;
  initial: { access_token: string; refresh_token?: string };
  mock_response: { status: number; body: Record<string, unknown> };
  expected: { access_token: string; refresh_token?: string; status: string };
}

describe('token manager — conformance (token_refresh.yaml)', () => {
  const fixture = parseYaml(
    readFileSync(join(SHARED_FIXTURES, 'token_refresh.yaml'), 'utf8'),
  ) as { scenarios: RefreshScenario[] };

  for (const scenario of fixture.scenarios) {
    it(scenario.name, async () => {
      await withFreshDb(async (pool) => {
        await seedProviderConfig(pool, scenario.provider);
        const id = await storeConnection(pool, 'conformance-user', scenario.provider, {
          accessToken: scenario.initial.access_token,
          refreshToken: scenario.initial.refresh_token ?? null,
          expiresIn: null,
          scopesGranted: [],
        }, TEST_KEY);

        const fetchImpl = mkFetch([scenario.mock_response]);
        const d = deps(pool, fetchImpl);

        if (scenario.expected.status === 'refresh_failed') {
          await expect(refreshConnection(id, d)).rejects.toThrow(TokenRefreshError);
        } else {
          await refreshConnection(id, d);
        }

        const { rows } = await pool.query(
          `SELECT access_token_encrypted, refresh_token_encrypted, status
             FROM _addition.connections WHERE id = $1`,
          [id],
        );
        expect(rows[0].status).toBe(scenario.expected.status);
        if (scenario.expected.status !== 'refresh_failed') {
          expect(decrypt(rows[0].access_token_encrypted, TEST_KEY)).toBe(scenario.expected.access_token);
          if (scenario.expected.refresh_token !== undefined) {
            expect(decrypt(rows[0].refresh_token_encrypted, TEST_KEY)).toBe(scenario.expected.refresh_token);
          }
        } else {
          // Tokens must not have been overwritten.
          expect(decrypt(rows[0].access_token_encrypted, TEST_KEY)).toBe(scenario.initial.access_token);
        }
      });
    });
  }
});
