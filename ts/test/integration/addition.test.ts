import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { Addition } from '../../src/addition.js';
import { EncryptionError } from '../../src/errors.js';

const SHARED_SCHEMA = join(__dirname, '..', '..', '..', 'shared', 'schema');
const TEST_KEY = 'f'.repeat(64);
const CALLBACK_URL = 'http://localhost:3000/api/addition/callback';

let container: StartedPostgreSqlContainer;
let databaseUrl: string;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_main_test')
    .start();
  databaseUrl = container.getConnectionUri();
});

afterAll(async () => {
  if (container) await container.stop();
});

async function withFreshDb<T>(fn: (dbUrl: string) => Promise<T>): Promise<T> {
  const dbName = `add_${Math.random().toString(36).slice(2, 10)}`;
  const admin = new Pool({ connectionString: databaseUrl });
  try {
    await admin.query(`CREATE DATABASE ${dbName}`);
  } finally {
    await admin.end();
  }
  const u = new URL(databaseUrl);
  u.pathname = '/' + dbName;
  const freshUrl = u.toString();
  try {
    return await fn(freshUrl);
  } finally {
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

function mkAddition(dbUrl: string, fetchImpl?: typeof fetch): Addition {
  return new Addition({
    databaseUrl: dbUrl,
    encryptionKey: TEST_KEY,
    callbackUrl: CALLBACK_URL,
    fetch: fetchImpl ?? (globalThis.fetch as typeof fetch),
  });
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

// ─── Constructor validation ───────────────────────────────────────────────────

describe('Addition constructor', () => {
  it('throws EncryptionError immediately for an encryptionKey shorter than 64 hex chars', () => {
    expect(() => new Addition({
      databaseUrl: 'postgres://localhost/test',
      encryptionKey: 'tooshort',
      callbackUrl: CALLBACK_URL,
    })).toThrow(EncryptionError);
  });

  it('throws EncryptionError for a key that is the right length but not hex', () => {
    expect(() => new Addition({
      databaseUrl: 'postgres://localhost/test',
      encryptionKey: 'z'.repeat(64),
      callbackUrl: CALLBACK_URL,
    })).toThrow(EncryptionError);
  });

  it('does not make any DB calls on construction — invalid DB URL is tolerated', () => {
    expect(() => new Addition({
      databaseUrl: 'postgres://no-such-host:5432/nope',
      encryptionKey: TEST_KEY,
      callbackUrl: CALLBACK_URL,
    })).not.toThrow();
  });
});

// ─── Schema bootstrap ─────────────────────────────────────────────────────────

describe('Addition — schema bootstrap', () => {
  it('bootstraps the _addition schema automatically on first method call', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      // configureProvider is the first call — should trigger bootstrap
      await addition.configureProvider({ providerId: 'github', clientId: 'c', clientSecret: 's' });
      // Verify schema exists by querying it directly
      const pool = new Pool({ connectionString: dbUrl });
      try {
        const { rows } = await pool.query(
          `SELECT table_name FROM information_schema.tables WHERE table_schema = '_addition'`,
        );
        expect(rows.length).toBeGreaterThan(0);
      } finally {
        await pool.end();
      }
      await addition.shutdown();
    });
  });
});

// ─── configureProvider ────────────────────────────────────────────────────────

describe('Addition — configureProvider', () => {
  it('stores a provider config with encrypted client secret', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      await addition.configureProvider({ providerId: 'github', clientId: 'my-client', clientSecret: 'secret' });
      const providers = await addition.listConfiguredProviders();
      expect(providers).toHaveLength(1);
      expect(providers[0].providerId).toBe('github');
      expect(providers[0].clientId).toBe('my-client');
      await addition.shutdown();
    });
  });

  it('is idempotent — calling twice updates rather than duplicating', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      await addition.configureProvider({ providerId: 'github', clientId: 'c1', clientSecret: 's1' });
      await addition.configureProvider({ providerId: 'github', clientId: 'c2', clientSecret: 's2' });
      const providers = await addition.listConfiguredProviders();
      const github = providers.filter((p) => p.providerId === 'github');
      expect(github).toHaveLength(1);
      expect(github[0].clientId).toBe('c2');
      await addition.shutdown();
    });
  });

  it('accepts optional scopes override', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      await addition.configureProvider({
        providerId: 'github', clientId: 'c', clientSecret: 's',
        scopes: ['repo', 'read:user'],
      });
      const providers = await addition.listConfiguredProviders();
      expect(providers[0].scopes).toEqual(expect.arrayContaining(['repo', 'read:user']));
      await addition.shutdown();
    });
  });
});

// ─── listConfiguredProviders ──────────────────────────────────────────────────

describe('Addition — listConfiguredProviders', () => {
  it('returns only configured providers, not every YAML definition', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      await addition.configureProvider({ providerId: 'github', clientId: 'c', clientSecret: 's' });
      const providers = await addition.listConfiguredProviders();
      // Only github was configured; slack, google, etc. must not appear
      expect(providers).toHaveLength(1);
      expect(providers[0].providerId).toBe('github');
      await addition.shutdown();
    });
  });

  it('returns empty array when nothing has been configured', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      // Trigger bootstrap without configuring anything
      const providers = await addition.listConfiguredProviders();
      expect(providers).toEqual([]);
      await addition.shutdown();
    });
  });
});

// ─── Events ───────────────────────────────────────────────────────────────────

describe('Addition — events', () => {
  it('emits connection.created after a successful OAuth callback', async () => {
    await withFreshDb(async (dbUrl) => {
      const fetch = mkFetch([
        { status: 200, body: { access_token: 'tok', expires_in: 3600 } },
      ]);
      const addition = mkAddition(dbUrl, fetch);
      await addition.configureProvider({ providerId: 'github', clientId: 'c', clientSecret: 's' });

      const handler = vi.fn();
      addition.on('connection.created', handler);

      // Build a valid state so handleCallback doesn't reject it
      const authUrl = await addition.getAuthUrl({ userId: 'u1', providerId: 'github' });
      const state = new URL(authUrl).searchParams.get('state')!;

      await addition.handleCallback({ code: 'fake-code', state });
      expect(handler).toHaveBeenCalledOnce();
      expect(handler.mock.calls[0][0]).toMatchObject({ userId: 'u1', providerId: 'github' });
      await addition.shutdown();
    });
  });

  it('emits tool.executed after a successful executeTool call', async () => {
    await withFreshDb(async (dbUrl) => {
      const fetch = mkFetch([{ status: 200, body: { id: 1, title: 'test' } }]);
      const addition = mkAddition(dbUrl, fetch);
      await addition.configureProvider({ providerId: 'github', clientId: 'c', clientSecret: 's' });

      // Seed a connection directly via the internal pool (bootstrap first)
      await addition.listConfiguredProviders(); // trigger bootstrap
      const pool = new Pool({ connectionString: dbUrl });
      try {
        const { encrypt } = await import('../../src/crypto.js');
        await pool.query(
          `INSERT INTO _addition.connections
             (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
              token_expires_at, scopes_granted)
           VALUES ($1, $2, $3, $4, $5, $6)`,
          ['u1', 'github', encrypt('access-tok', TEST_KEY), null, null, []],
        );
      } finally {
        await pool.end();
      }

      const handler = vi.fn();
      addition.on('tool.executed', handler);

      await addition.executeTool({
        userId: 'u1', providerId: 'github', toolId: 'list_repos', params: {},
      });
      expect(handler).toHaveBeenCalledOnce();
      await addition.shutdown();
    });
  });

  it('emits token.refreshed when proxy refreshes an expired token on 401', async () => {
    await withFreshDb(async (dbUrl) => {
      const fetch = mkFetch([
        { status: 401, body: { error: 'invalid_credentials' } },
        { status: 200, body: { access_token: 'new-tok', expires_in: 3600 } },
        { status: 200, body: { id: 'me' } },
      ]);
      const addition = mkAddition(dbUrl, fetch);
      await addition.configureProvider({ providerId: 'google', clientId: 'c', clientSecret: 's' });

      await addition.listConfiguredProviders(); // trigger bootstrap
      const pool = new Pool({ connectionString: dbUrl });
      try {
        const { encrypt } = await import('../../src/crypto.js');
        await pool.query(
          `INSERT INTO _addition.connections
             (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
              token_expires_at, scopes_granted)
           VALUES ($1, $2, $3, $4, $5, $6)`,
          ['u1', 'google', encrypt('old-tok', TEST_KEY), encrypt('refresh-tok', TEST_KEY), null, []],
        );
      } finally {
        await pool.end();
      }

      const handler = vi.fn();
      addition.on('token.refreshed', handler);

      await addition.proxy({ userId: 'u1', providerId: 'google', method: 'GET', path: '/oauth2/v1/userinfo' });
      expect(handler).toHaveBeenCalledOnce();
      await addition.shutdown();
    });
  });
});

// ─── Lifecycle ────────────────────────────────────────────────────────────────

describe('Addition — lifecycle', () => {
  it('shutdown() closes the pool — subsequent queries throw', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      await addition.listConfiguredProviders(); // bootstrap
      await addition.shutdown();
      await expect(addition.listConfiguredProviders()).rejects.toThrow();
    });
  });

  it('startBackgroundTasks() and stopBackgroundTasks() do not throw', async () => {
    await withFreshDb(async (dbUrl) => {
      const addition = mkAddition(dbUrl);
      await addition.listConfiguredProviders(); // bootstrap
      expect(() => addition.startBackgroundTasks()).not.toThrow();
      expect(() => addition.stopBackgroundTasks()).not.toThrow();
      await addition.shutdown();
    });
  });
});
