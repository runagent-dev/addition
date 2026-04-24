import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { Pool } from 'pg';
import { SchemaError } from './errors.js';

/** Migration filenames must match `NNN_name.sql` where NNN is a 3-digit id. */
const MIGRATION_RE = /^(\d{3})_.+\.sql$/;

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
 */
export async function bootstrap(databaseUrl: string, schemaDir: string): Promise<void> {
  const migrations = loadMigrations(schemaDir);
  if (migrations.length === 0) {
    throw new SchemaError(`no migrations found in ${schemaDir}`);
  }

  const pool = new Pool({ connectionString: databaseUrl });
  try {
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
    }
  } finally {
    await pool.end();
  }
}
