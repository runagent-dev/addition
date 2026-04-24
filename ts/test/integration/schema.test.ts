import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { mkdtempSync, writeFileSync, readdirSync, copyFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { bootstrap } from '../../src/schema.js';

const SHARED_SCHEMA = join(__dirname, '..', '..', '..', 'shared', 'schema');

let container: StartedPostgreSqlContainer;
let databaseUrl: string;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_test')
    .start();
  databaseUrl = container.getConnectionUri();
});

afterAll(async () => {
  if (container) await container.stop();
});

/**
 * Each test runs against a fresh, empty database inside the shared container.
 * We create a uniquely-named DB, run the test, then drop it. This is faster
 * than starting a new container per test while still guaranteeing isolation.
 */
async function withFreshDb<T>(fn: (url: string) => Promise<T>): Promise<T> {
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
  try {
    return await fn(freshUrl);
  } finally {
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

describe('schema bootstrap', () => {
  it('creates the _addition schema on an empty database', async () => {
    await withFreshDb(async (url) => {
      await bootstrap(url, SHARED_SCHEMA);
      const pool = new Pool({ connectionString: url });
      try {
        const { rows } = await pool.query(
          `SELECT schema_name FROM information_schema.schemata WHERE schema_name = '_addition'`,
        );
        expect(rows).toHaveLength(1);
      } finally {
        await pool.end();
      }
    });
  });

  it('creates all 5 tables under _addition namespace (not public)', async () => {
    await withFreshDb(async (url) => {
      await bootstrap(url, SHARED_SCHEMA);
      const pool = new Pool({ connectionString: url });
      try {
        const { rows } = await pool.query(
          `SELECT table_name FROM information_schema.tables
           WHERE table_schema = '_addition' ORDER BY table_name`,
        );
        const names = rows.map((r) => r.table_name);
        expect(names).toEqual([
          'audit_log',
          'connections',
          'meta',
          'oauth_state',
          'provider_configs',
        ]);
        // And none of these leaked into public
        const { rows: pub } = await pool.query(
          `SELECT table_name FROM information_schema.tables
           WHERE table_schema = 'public'
             AND table_name IN ('meta', 'provider_configs', 'connections', 'oauth_state', 'audit_log')`,
        );
        expect(pub).toHaveLength(0);
      } finally {
        await pool.end();
      }
    });
  });

  it('creates the documented indexes', async () => {
    await withFreshDb(async (url) => {
      await bootstrap(url, SHARED_SCHEMA);
      const pool = new Pool({ connectionString: url });
      try {
        const { rows } = await pool.query(
          `SELECT indexname FROM pg_indexes WHERE schemaname = '_addition' ORDER BY indexname`,
        );
        const names = rows.map((r) => r.indexname);
        expect(names).toContain('idx_connections_user_provider');
        expect(names).toContain('idx_connections_expiring');
        expect(names).toContain('idx_oauth_state_expires');
        expect(names).toContain('idx_audit_log_user');
      } finally {
        await pool.end();
      }
    });
  });

  it('records the schema_version in _addition.meta', async () => {
    await withFreshDb(async (url) => {
      await bootstrap(url, SHARED_SCHEMA);
      const pool = new Pool({ connectionString: url });
      try {
        const { rows } = await pool.query(
          `SELECT value FROM _addition.meta WHERE key = 'schema_version'`,
        );
        expect(rows).toHaveLength(1);
        expect(rows[0].value).toBe('001');
      } finally {
        await pool.end();
      }
    });
  });

  it('is idempotent: calling bootstrap twice is a no-op', async () => {
    await withFreshDb(async (url) => {
      await bootstrap(url, SHARED_SCHEMA);
      // Second call must not throw, must not change the schema version.
      await bootstrap(url, SHARED_SCHEMA);

      const pool = new Pool({ connectionString: url });
      try {
        const { rows } = await pool.query(
          `SELECT COUNT(*)::int AS n FROM _addition.meta WHERE key = 'schema_version'`,
        );
        expect(rows[0].n).toBe(1);
        const { rows: tables } = await pool.query(
          `SELECT COUNT(*)::int AS n FROM information_schema.tables WHERE table_schema = '_addition'`,
        );
        expect(tables[0].n).toBe(5);
      } finally {
        await pool.end();
      }
    });
  });

  it('skips already-applied migrations (version check)', async () => {
    await withFreshDb(async (url) => {
      // First run: apply 001.
      await bootstrap(url, SHARED_SCHEMA);

      // Capture the timestamp on meta.
      const pool = new Pool({ connectionString: url });
      try {
        const { rows: before } = await pool.query(
          `SELECT updated_at FROM _addition.meta WHERE key = 'schema_version'`,
        );
        const beforeTs = before[0].updated_at;

        // Small delay so if we DID re-run the migration, updated_at would move.
        await new Promise((r) => setTimeout(r, 50));

        // Second bootstrap: must be skipped (no touch to meta.updated_at).
        await bootstrap(url, SHARED_SCHEMA);

        const { rows: after } = await pool.query(
          `SELECT updated_at FROM _addition.meta WHERE key = 'schema_version'`,
        );
        expect(after[0].updated_at.getTime()).toBe(beforeTs.getTime());
      } finally {
        await pool.end();
      }
    });
  });

  it('picks up a new 002_*.sql migration and bumps the version', async () => {
    await withFreshDb(async (url) => {
      // Copy 001 + synthesize a minimal 002 into a tmp dir.
      const tmp = mkdtempSync(join(tmpdir(), 'addition-schema-'));
      try {
        for (const f of readdirSync(SHARED_SCHEMA)) {
          if (f.endsWith('.sql')) copyFileSync(join(SHARED_SCHEMA, f), join(tmp, f));
        }
        writeFileSync(
          join(tmp, '002_add_column.sql'),
          `ALTER TABLE _addition.provider_configs
             ADD COLUMN IF NOT EXISTS test_marker TEXT;
           INSERT INTO _addition.meta (key, value)
             VALUES ('schema_version', '002')
             ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();`,
          'utf8',
        );

        // First bootstrap with only 001 already on disk? No — both are on disk now,
        // so a single run should apply both in order.
        await bootstrap(url, tmp);

        const pool = new Pool({ connectionString: url });
        try {
          const { rows } = await pool.query(
            `SELECT value FROM _addition.meta WHERE key = 'schema_version'`,
          );
          expect(rows[0].value).toBe('002');

          const { rows: cols } = await pool.query(
            `SELECT column_name FROM information_schema.columns
             WHERE table_schema = '_addition' AND table_name = 'provider_configs'
               AND column_name = 'test_marker'`,
          );
          expect(cols).toHaveLength(1);
        } finally {
          await pool.end();
        }
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });
  });

  it('applies pending migrations when version is behind', async () => {
    await withFreshDb(async (url) => {
      // Apply 001.
      await bootstrap(url, SHARED_SCHEMA);

      // Add a 002 migration and re-bootstrap; 001 must be skipped, 002 applied.
      const tmp = mkdtempSync(join(tmpdir(), 'addition-schema-'));
      try {
        for (const f of readdirSync(SHARED_SCHEMA)) {
          if (f.endsWith('.sql')) copyFileSync(join(SHARED_SCHEMA, f), join(tmp, f));
        }
        writeFileSync(
          join(tmp, '002_add_column.sql'),
          `ALTER TABLE _addition.connections
             ADD COLUMN IF NOT EXISTS another_marker TEXT;
           INSERT INTO _addition.meta (key, value)
             VALUES ('schema_version', '002')
             ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();`,
          'utf8',
        );

        await bootstrap(url, tmp);

        const pool = new Pool({ connectionString: url });
        try {
          const { rows } = await pool.query(
            `SELECT value FROM _addition.meta WHERE key = 'schema_version'`,
          );
          expect(rows[0].value).toBe('002');
        } finally {
          await pool.end();
        }
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });
  });
});
