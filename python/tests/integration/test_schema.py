"""Integration tests for schema bootstrap — requires a live Postgres instance.

Each test runs against a fresh, isolated database created inside a shared
container. The container is started once per session; fresh DBs are created
and dropped per test for isolation.
"""
from __future__ import annotations

import asyncio
import os
import random
import shutil
import string
import tempfile
from pathlib import Path

import asyncpg
import pytest
import pytest_asyncio

from runagent_addition.errors import SchemaError
from runagent_addition.schema import bootstrap

SHARED_SCHEMA = Path(__file__).parents[3] / "shared" / "schema"

# ── Container fixture ─────────────────────────────────────────────────────────


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer

    with PostgresContainer("postgres:16-alpine", dbname="addition_test") as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(pg_container):
    return pg_container.get_connection_url().replace("psycopg2", "asyncpg").replace("+asyncpg", "")


# ── Per-test fresh DB helper ──────────────────────────────────────────────────


async def _with_fresh_db(admin_dsn: str):
    """Create a uniquely-named database and return its DSN. Caller must drop it."""
    suffix = "".join(random.choices(string.ascii_lowercase, k=8))
    db_name = f"addition_{suffix}"
    conn = await asyncpg.connect(admin_dsn)
    try:
        await conn.execute(f'CREATE DATABASE "{db_name}"')
    finally:
        await conn.close()

    base = admin_dsn.rsplit("/", 1)[0]
    return f"{base}/{db_name}", db_name


async def _drop_db(admin_dsn: str, db_name: str) -> None:
    conn = await asyncpg.connect(admin_dsn)
    try:
        await conn.execute(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1",
            db_name,
        )
        await conn.execute(f'DROP DATABASE IF EXISTS "{db_name}"')
    finally:
        await conn.close()


# ── Tests ─────────────────────────────────────────────────────────────────────


@pytest.mark.asyncio
async def test_creates_addition_schema(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))
        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT schema_name FROM information_schema.schemata WHERE schema_name = '_addition'"
            )
            assert len(rows) == 1
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_creates_all_5_tables_under_addition_not_public(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))
        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                """SELECT table_name FROM information_schema.tables
                   WHERE table_schema = '_addition' ORDER BY table_name"""
            )
            names = [r["table_name"] for r in rows]
            assert names == ["audit_log", "connections", "meta", "oauth_state", "provider_configs"]

            pub_rows = await conn.fetch(
                """SELECT table_name FROM information_schema.tables
                   WHERE table_schema = 'public'
                     AND table_name IN ('meta','provider_configs','connections','oauth_state','audit_log')"""
            )
            assert len(pub_rows) == 0
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_creates_documented_indexes(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))
        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT indexname FROM pg_indexes WHERE schemaname = '_addition' ORDER BY indexname"
            )
            names = [r["indexname"] for r in rows]
            assert "idx_connections_user_provider" in names
            assert "idx_connections_expiring" in names
            assert "idx_oauth_state_expires" in names
            assert "idx_audit_log_user" in names
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_records_schema_version_in_meta(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))
        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT value FROM _addition.meta WHERE key = 'schema_version'"
            )
            assert len(rows) == 1
            assert rows[0]["value"] == "001"
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_idempotent_bootstrap_twice(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))
        await bootstrap(dsn, str(SHARED_SCHEMA))  # must not raise

        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT COUNT(*)::int AS n FROM _addition.meta WHERE key = 'schema_version'"
            )
            assert rows[0]["n"] == 1
            t_rows = await conn.fetch(
                "SELECT COUNT(*)::int AS n FROM information_schema.tables WHERE table_schema = '_addition'"
            )
            assert t_rows[0]["n"] == 5
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_skips_already_applied_migrations(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))

        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT updated_at FROM _addition.meta WHERE key = 'schema_version'"
            )
            before_ts = rows[0]["updated_at"]
        finally:
            await conn.close()

        await asyncio.sleep(0.05)
        await bootstrap(dsn, str(SHARED_SCHEMA))

        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT updated_at FROM _addition.meta WHERE key = 'schema_version'"
            )
            assert rows[0]["updated_at"] == before_ts
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_picks_up_new_002_migration(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    tmp = tempfile.mkdtemp()
    try:
        for f in SHARED_SCHEMA.iterdir():
            if f.suffix == ".sql":
                shutil.copy(f, Path(tmp) / f.name)

        Path(tmp, "002_add_column.sql").write_text(
            """ALTER TABLE _addition.provider_configs
                 ADD COLUMN IF NOT EXISTS test_marker TEXT;
               INSERT INTO _addition.meta (key, value)
                 VALUES ('schema_version', '002')
                 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();"""
        )

        await bootstrap(dsn, tmp)

        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT value FROM _addition.meta WHERE key = 'schema_version'"
            )
            assert rows[0]["value"] == "002"

            cols = await conn.fetch(
                """SELECT column_name FROM information_schema.columns
                   WHERE table_schema = '_addition' AND table_name = 'provider_configs'
                     AND column_name = 'test_marker'"""
            )
            assert len(cols) == 1
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)
        shutil.rmtree(tmp)


@pytest.mark.asyncio
async def test_concurrent_bootstrap_race_safe(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        await asyncio.gather(*[bootstrap(dsn, str(SHARED_SCHEMA)) for _ in range(5)])

        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT COUNT(*)::int AS n FROM _addition.meta WHERE key = 'schema_version'"
            )
            assert rows[0]["n"] == 1
            t_rows = await conn.fetch(
                "SELECT COUNT(*)::int AS n FROM information_schema.tables WHERE table_schema = '_addition'"
            )
            assert t_rows[0]["n"] == 5
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)


@pytest.mark.asyncio
async def test_raises_schema_error_when_migration_does_not_bump_version(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    tmp = tempfile.mkdtemp()
    try:
        for f in SHARED_SCHEMA.iterdir():
            if f.suffix == ".sql":
                shutil.copy(f, Path(tmp) / f.name)

        Path(tmp, "002_forgot_bump.sql").write_text(
            "ALTER TABLE _addition.provider_configs ADD COLUMN IF NOT EXISTS forgot TEXT;"
        )

        with pytest.raises(SchemaError, match="002"):
            await bootstrap(dsn, tmp)
    finally:
        await _drop_db(admin_dsn, db_name)
        shutil.rmtree(tmp)


@pytest.mark.asyncio
async def test_applies_pending_migrations_when_behind(admin_dsn):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    tmp = tempfile.mkdtemp()
    try:
        await bootstrap(dsn, str(SHARED_SCHEMA))

        for f in SHARED_SCHEMA.iterdir():
            if f.suffix == ".sql":
                shutil.copy(f, Path(tmp) / f.name)

        Path(tmp, "002_add_column.sql").write_text(
            """ALTER TABLE _addition.connections
                 ADD COLUMN IF NOT EXISTS another_marker TEXT;
               INSERT INTO _addition.meta (key, value)
                 VALUES ('schema_version', '002')
                 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();"""
        )

        await bootstrap(dsn, tmp)

        conn = await asyncpg.connect(dsn)
        try:
            rows = await conn.fetch(
                "SELECT value FROM _addition.meta WHERE key = 'schema_version'"
            )
            assert rows[0]["value"] == "002"
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)
        shutil.rmtree(tmp)
