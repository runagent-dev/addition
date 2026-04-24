import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { Pool } from 'pg';
import { SchemaError } from './errors.js';

/** Migration filenames must match `NNN_name.sql` where NNN is a 3-digit id. */
const MIGRATION_RE = /^(\d{3})_.+\.sql$/;

/**
 * Stable 64-bit advisory lock key. Every pod racing to bootstrap the schema
 * acquires this lock; only one runs migrations at a time. Key is arbitrary but
 * must never change — changing it would split-brain the lock across versions.
 */
const BOOTSTRAP_LOCK_KEY = '6731415926535897';

interface Migration {
  version: string; // "001", "002", ...
  filename: string;
  sql: string;
}

function loadMigrations(dir: string): Migration[] {
  const files = readdirSync(dir).filter((f) => MIGRATION_RE.test(f));
  const migrations: Migration[] = files.map((f) => {
    const version = f.match(MIGRATION_RE)![1];
    return { version, filename: f, sql: readFileSync(join(dir, f), 'utf8') };
  });
  // Lexicographic order is correct because versions are zero-padded 3-digit.
  migrations.sort((a, b) => a.version.localeCompare(b.version));
  return migrations;
}

async function currentVersion(pool: Pool): Promise<string | null> {
  // Meta table may not exist yet on a fresh DB. Check the schema first.
  const { rows: schemaRows } = await pool.query(
    `SELECT 1 FROM information_schema.schemata WHERE schema_name = '_addition'`,
  );
  if (schemaRows.length === 0) return null;
  const { rows: metaRows } = await pool.query(
    `SELECT 1 FROM information_schema.tables
     WHERE table_schema = '_addition' AND table_name = 'meta'`,
  );
  if (metaRows.length === 0) return null;
  const { rows } = await pool.query(
    `SELECT value FROM _addition.meta WHERE key = 'schema_version'`,
  );
  return rows.length === 0 ? null : rows[0].value;
}

/**
 * Bootstraps the `_addition` schema. Safe to call on every application start.
 *
 * Behavior:
 *   - On an empty DB: creates `_addition` schema + all tables by running every
 *     migration in `schemaDir` in order, then records the final version in
 *     `_addition.meta.schema_version`.
 *   - On a DB already at the latest version: no-op (single SELECT + early return).
 *   - On a DB behind the latest: runs only the pending migrations.
 *
 * Each migration file is responsible for updating `_addition.meta.schema_version`
 * itself as its final statement (see `shared/schema/README.md`). This makes
 * migrations self-contained — the bootstrapper doesn't need to second-guess.
 *
 * Migrations are run inside a transaction per file. A failing migration rolls
 * back cleanly and throws `SchemaError`.
 *
 * Pool lifecycle — for M12 (`Addition` main class) wiring:
 * This function creates and tears down its own `pg.Pool` for the duration of
 * the bootstrap call. It does NOT accept an existing pool. The runtime pool
 * the rest of the library uses (OAuth engine, proxy, token manager) is
 * created separately by `Addition` after `bootstrap` returns. Cost is one
 * extra brief-lived pool at startup; benefit is that `schema.ts` has no
 * dependency on the main class construction order.
 */
export async function bootstrap(databaseUrl: string, schemaDir: string): Promise<void> {
  const migrations = loadMigrations(schemaDir);
  if (migrations.length === 0) {
    throw new SchemaError(`no migrations found in ${schemaDir}`);
  }

  const pool = new Pool({ connectionString: databaseUrl });
  try {
    // Serialize bootstrap across concurrently-starting app replicas. Without
    // this lock, two pods racing against an empty DB will both see `null` in
    // `currentVersion()` and both run migration 001. Harmless today because
    // 001 uses IF NOT EXISTS everywhere; the first ALTER TABLE / DROP / data
    // backfill in a later migration would corrupt state.
    const lockClient = await pool.connect();
    try {
      await lockClient.query('SELECT pg_advisory_lock($1::bigint)', [BOOTSTRAP_LOCK_KEY]);
      try {
        // Re-check inside the lock — another pod may have completed already.
        const current = await currentVersion(pool);
        const pending = migrations.filter((m) => current === null || m.version > current);
        if (pending.length === 0) return; // already up to date

        for (const m of pending) {
          const client = await pool.connect();
          try {
            await client.query('BEGIN');
            await client.query(m.sql);
            await client.query('COMMIT');
          } catch (err) {
            await client.query('ROLLBACK').catch(() => {});
            throw new SchemaError(
              `migration ${m.filename} failed: ${(err as Error).message}`,
              err,
            );
          } finally {
            client.release();
          }
          // Enforce that the migration actually bumped schema_version — if it
          // didn't (author forgot the closing INSERT), bootstrap would loop on
          // every restart and destructive statements would re-destroy.
          const after = await currentVersion(pool);
          if (after !== m.version) {
            throw new SchemaError(
              `migration ${m.filename} completed but did not bump _addition.meta.schema_version to '${m.version}' (got '${after ?? 'null'}')`,
            );
          }
        }
      } finally {
        await lockClient.query('SELECT pg_advisory_unlock($1::bigint)', [BOOTSTRAP_LOCK_KEY]);
      }
    } finally {
      lockClient.release();
    }
  } finally {
    await pool.end();
  }
}
