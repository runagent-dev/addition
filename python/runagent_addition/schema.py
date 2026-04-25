from __future__ import annotations

import os
import re
from typing import List, Optional

import asyncpg

from .errors import SchemaError

_MIGRATION_RE = re.compile(r"^(\d{3})_.+\.sql$")

# Stable advisory lock key — must never change (see TS comment for rationale).
_BOOTSTRAP_LOCK_KEY = 6731415926535897


class _Migration:
    __slots__ = ("version", "filename", "sql")

    def __init__(self, version: str, filename: str, sql: str) -> None:
        self.version = version
        self.filename = filename
        self.sql = sql


def _load_migrations(dir_: str) -> List[_Migration]:
    migrations = []
    for fname in os.listdir(dir_):
        m = _MIGRATION_RE.match(fname)
        if not m:
            continue
        version = m.group(1)
        sql = (os.path.join(dir_, fname))
        with open(os.path.join(dir_, fname), encoding="utf-8") as fh:
            sql_text = fh.read()
        migrations.append(_Migration(version, fname, sql_text))
    migrations.sort(key=lambda m: m.version)
    return migrations


async def _current_version(conn: asyncpg.Connection) -> Optional[str]:
    schema_exists = await conn.fetchval(
        "SELECT 1 FROM information_schema.schemata WHERE schema_name = '_addition'"
    )
    if not schema_exists:
        return None
    meta_exists = await conn.fetchval(
        """SELECT 1 FROM information_schema.tables
           WHERE table_schema = '_addition' AND table_name = 'meta'"""
    )
    if not meta_exists:
        return None
    row = await conn.fetchrow(
        "SELECT value FROM _addition.meta WHERE key = 'schema_version'"
    )
    return row["value"] if row else None


async def bootstrap(database_url: str, schema_dir: str) -> None:
    """Bootstrap the _addition schema. Safe to call on every application start.

    - Empty DB: runs all migrations in order, records final version in meta.
    - Already up to date: no-op.
    - Behind latest: runs only pending migrations.

    Uses a Postgres advisory lock so concurrent app replicas don't race.
    Each migration must update _addition.meta.schema_version itself — raises
    SchemaError if a migration completes without bumping the version.
    """
    migrations = _load_migrations(schema_dir)
    if not migrations:
        raise SchemaError(f"no migrations found in {schema_dir}")

    conn = await asyncpg.connect(database_url)
    try:
        await conn.execute(
            "SELECT pg_advisory_lock($1::bigint)", _BOOTSTRAP_LOCK_KEY
        )
        try:
            current = await _current_version(conn)
            pending = [m for m in migrations if current is None or m.version > current]
            if not pending:
                return

            for migration in pending:
                try:
                    await conn.execute(migration.sql)
                except Exception as exc:
                    raise SchemaError(
                        f"migration {migration.filename} failed: {exc}"
                    ) from exc

                after = await _current_version(conn)
                if after != migration.version:
                    raise SchemaError(
                        f"migration {migration.filename} completed but did not bump "
                        f"_addition.meta.schema_version to '{migration.version}' "
                        f"(got '{after}')"
                    )
        finally:
            await conn.execute(
                "SELECT pg_advisory_unlock($1::bigint)", _BOOTSTRAP_LOCK_KEY
            )
    finally:
        await conn.close()
