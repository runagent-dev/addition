import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { bootstrap } from '../../src/schema.js';
import { loadProviders } from '../../src/providers.js';
import { encrypt } from '../../src/crypto.js';
import { storeConnection, getConnectionById } from '../../src/tokens.js';
import { startBackgroundTasks, runRefreshTick } from '../../src/background.js';
import type { BackgroundDeps } from '../../src/background.js';
import type { ProviderRegistry } from '../../src/types.js';

const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');
const SHARED_SCHEMA = join(__dirname, '..', '..', '..', 'shared', 'schema');
const TEST_KEY = 'e'.repeat(64);

let container: StartedPostgreSqlContainer;
let databaseUrl: string;
let providers: ProviderRegistry;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_bg_test')
    .start();
  databaseUrl = container.getConnectionUri();
  providers = loadProviders(SHARED_PROVIDERS);
});

afterAll(async () => {
  if (container) await container.stop();
});

afterEach(() => {
  vi.useRealTimers();
});

async function withFreshDb<T>(fn: (pool: Pool) => Promise<T>): Promise<T> {
  const dbName = `bg_${Math.random().toString(36).slice(2, 10)}`;
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
    await pool.end().catch(() => {/* pool may already be ended by shutdown() */});
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

function mkFetch(responses: Array<{ status: number; body: unknown }>): typeof fetch {
  let i = 0;
  return vi.fn(async () => {
    const r = responses[Math.min(i, responses.length - 1)];
    i++;
    return {
      status: r.status,
      ok: r.status >= 200 && r.status < 300,
      headers: new Headers({ 'content-type': 'application/json' }),
      text: async () => JSON.stringify(r.body),
      json: async () => r.body,
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

function makeDeps(
  pool: Pool,
  fetchImpl: typeof fetch,
  bufferMs = 10 * 60 * 1000,
): BackgroundDeps {
  return {
    pool,
    providers,
    encryptionKey: TEST_KEY,
    fetch: fetchImpl,
    tokenRefreshIntervalMs: 60_000,
    tokenRefreshBufferMs: bufferMs,
  };
}

// ─── runRefreshTick — direct DB tests (no fake timers) ───────────────────────

describe('runRefreshTick — expiry selection', () => {
  it('refreshes a connection expiring within the buffer window', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const connId = await storeConnection(pool, 'u1', 'google', {
        accessToken: 'old-access', refreshToken: 'refresh-tok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      await pool.query(
        `UPDATE _addition.connections SET token_expires_at = NOW() + interval '2 minutes' WHERE id = $1`,
        [connId],
      );
      const spy = mkFetch([
        { status: 200, body: { access_token: 'new-access', expires_in: 3600 } },
      ]);
      await runRefreshTick(makeDeps(pool, spy, 10 * 60 * 1000));
      const refreshed = await getConnectionById(pool, connId, TEST_KEY);
      expect(refreshed?.accessToken).toBe('new-access');
    });
  });

  it('does not refresh a connection expiring outside the buffer window', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const connId = await storeConnection(pool, 'u1', 'google', {
        accessToken: 'original', refreshToken: 'refresh-tok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      await pool.query(
        `UPDATE _addition.connections SET token_expires_at = NOW() + interval '30 minutes' WHERE id = $1`,
        [connId],
      );
      const spy = mkFetch([]);
      await runRefreshTick(makeDeps(pool, spy, 5 * 60 * 1000));
      const conn = await getConnectionById(pool, connId, TEST_KEY);
      expect(conn?.accessToken).toBe('original');
      expect(spy).not.toHaveBeenCalled();
    });
  });

  it('skips connections without a refresh token', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'github');
      const connId = await storeConnection(pool, 'u1', 'github', {
        accessToken: 'tok', refreshToken: null, expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      await pool.query(
        `UPDATE _addition.connections SET token_expires_at = NOW() + interval '1 minute' WHERE id = $1`,
        [connId],
      );
      const spy = mkFetch([]);
      await runRefreshTick(makeDeps(pool, spy));
      expect(spy).not.toHaveBeenCalled();
    });
  });

  it('skips connections with no expiry set (token_expires_at IS NULL)', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const connId = await storeConnection(pool, 'u1', 'google', {
        accessToken: 'tok', refreshToken: 'rtok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      // token_expires_at is NULL — no-op
      const spy = mkFetch([]);
      await runRefreshTick(makeDeps(pool, spy));
      expect(spy).not.toHaveBeenCalled();
      const conn = await getConnectionById(pool, connId, TEST_KEY);
      expect(conn?.accessToken).toBe('tok');
    });
  });
});

describe('runRefreshTick — failure isolation', () => {
  it('continues refreshing other connections when one refresh fails', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const connId1 = await storeConnection(pool, 'u1', 'google', {
        accessToken: 'old1', refreshToken: 'bad-tok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      const connId2 = await storeConnection(pool, 'u2', 'google', {
        accessToken: 'old2', refreshToken: 'good-tok', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      await pool.query(
        `UPDATE _addition.connections SET token_expires_at = NOW() + interval '1 minute'
         WHERE id = ANY($1::uuid[])`,
        [[connId1, connId2]],
      );
      // First refresh fails, second succeeds.
      const spy = mkFetch([
        { status: 400, body: { error: 'invalid_grant' } },
        { status: 200, body: { access_token: 'new2', expires_in: 3600 } },
      ]);
      await runRefreshTick(makeDeps(pool, spy));
      const conn2 = await getConnectionById(pool, connId2, TEST_KEY);
      expect(conn2?.accessToken).toBe('new2');
    });
  });

  it('refreshes multiple expiring connections in the same tick', async () => {
    await withFreshDb(async (pool) => {
      await seedProviderConfig(pool, 'google');
      const connId1 = await storeConnection(pool, 'u1', 'google', {
        accessToken: 'old1', refreshToken: 'tok1', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      const connId2 = await storeConnection(pool, 'u2', 'google', {
        accessToken: 'old2', refreshToken: 'tok2', expiresIn: null, scopesGranted: [],
      }, TEST_KEY);
      await pool.query(
        `UPDATE _addition.connections SET token_expires_at = NOW() + interval '1 minute'
         WHERE id = ANY($1::uuid[])`,
        [[connId1, connId2]],
      );
      const spy = mkFetch([
        { status: 200, body: { access_token: 'new1', expires_in: 3600 } },
        { status: 200, body: { access_token: 'new2', expires_in: 3600 } },
      ]);
      await runRefreshTick(makeDeps(pool, spy));
      const [c1, c2] = await Promise.all([
        getConnectionById(pool, connId1, TEST_KEY),
        getConnectionById(pool, connId2, TEST_KEY),
      ]);
      expect(c1?.accessToken).toBe('new1');
      expect(c2?.accessToken).toBe('new2');
    });
  });
});

// ─── startBackgroundTasks / stop / shutdown ──────────────────────────────────

describe('startBackgroundTasks', () => {
  it('fires the tick function at tokenRefreshIntervalMs', async () => {
    await withFreshDb(async (pool) => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
      const tickSpy = vi.fn(async () => {});
      const handle = startBackgroundTasks({
        ...makeDeps(pool, mkFetch([])),
        tokenRefreshIntervalMs: 500,
        _tickFn: tickSpy,
      });
      expect(tickSpy).not.toHaveBeenCalled();
      vi.advanceTimersByTime(500);
      expect(tickSpy).toHaveBeenCalledOnce();
      handle.stop();
    });
  });

  it('fires the tick multiple times on successive intervals', async () => {
    await withFreshDb(async (pool) => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
      const tickSpy = vi.fn(async () => {});
      const handle = startBackgroundTasks({
        ...makeDeps(pool, mkFetch([])),
        tokenRefreshIntervalMs: 500,
        _tickFn: tickSpy,
      });
      vi.advanceTimersByTime(1500);
      expect(tickSpy).toHaveBeenCalledTimes(3);
      handle.stop();
    });
  });

  it('stop() clears the interval — no further ticks fire', async () => {
    await withFreshDb(async (pool) => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
      const tickSpy = vi.fn(async () => {});
      const handle = startBackgroundTasks({
        ...makeDeps(pool, mkFetch([])),
        tokenRefreshIntervalMs: 500,
        _tickFn: tickSpy,
      });
      vi.advanceTimersByTime(500);
      expect(tickSpy).toHaveBeenCalledTimes(1);
      handle.stop();
      vi.advanceTimersByTime(2000);
      expect(tickSpy).toHaveBeenCalledTimes(1); // no additional fires
    });
  });

  it('shutdown() stops the interval and closes the pool', async () => {
    await withFreshDb(async (pool) => {
      const tickSpy = vi.fn(async () => {});
      const handle = startBackgroundTasks({
        ...makeDeps(pool, mkFetch([])),
        _tickFn: tickSpy,
      });
      await handle.shutdown();
      await expect(pool.query('SELECT 1')).rejects.toThrow();
    });
  });

  it('shutdown() does not fire any more ticks after closing', async () => {
    await withFreshDb(async (pool) => {
      vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
      const tickSpy = vi.fn(async () => {});
      const handle = startBackgroundTasks({
        ...makeDeps(pool, mkFetch([])),
        tokenRefreshIntervalMs: 500,
        _tickFn: tickSpy,
      });
      await handle.shutdown();
      vi.advanceTimersByTime(2000);
      expect(tickSpy).not.toHaveBeenCalled();
    });
  });
});
