"""Integration tests for background task runner."""
from __future__ import annotations

import asyncio
import json
import random
import string
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

import asyncpg
import pytest

from runagent_addition.crypto import encrypt
from runagent_addition.providers import load_providers
from runagent_addition.schema import bootstrap
from runagent_addition.tokens import store_connection, get_connection_by_id
from runagent_addition.background import run_refresh_tick, start_background_tasks

SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"
SHARED_SCHEMA = Path(__file__).parents[3] / "shared" / "schema"
TEST_KEY = "e" * 64


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer
    with PostgresContainer("postgres:16-alpine", dbname="addition_bg_test") as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(pg_container):
    return pg_container.get_connection_url().replace("psycopg2", "asyncpg").replace("+asyncpg", "")


@pytest.fixture(scope="session")
def providers():
    return load_providers(str(SHARED_PROVIDERS))


# ── Helpers ───────────────────────────────────────────────────────────────────


async def _with_fresh_db(admin_dsn: str):
    suffix = "".join(random.choices(string.ascii_lowercase, k=8))
    db_name = f"bg_{suffix}"
    conn = await asyncpg.connect(admin_dsn)
    try:
        await conn.execute(f'CREATE DATABASE "{db_name}"')
    finally:
        await conn.close()
    base = admin_dsn.rsplit("/", 1)[0]
    fresh_url = f"{base}/{db_name}"
    await bootstrap(fresh_url, str(SHARED_SCHEMA))
    return fresh_url, db_name


async def _drop_db(admin_dsn: str, db_name: str) -> None:
    conn = await asyncpg.connect(admin_dsn)
    try:
        await conn.execute(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", db_name
        )
        await conn.execute(f'DROP DATABASE IF EXISTS "{db_name}"')
    finally:
        await conn.close()


async def _seed_provider(conn, provider_id):
    await conn.execute(
        "INSERT INTO _addition.provider_configs (provider_id, client_id, client_secret_encrypted, scopes) "
        "VALUES ($1, $2, $3, $4)",
        provider_id, "test-client", encrypt("test-secret", TEST_KEY), [],
    )


def mk_fetch(responses: list[dict]) -> AsyncMock:
    i = 0

    async def _fetch(url, **kwargs):
        nonlocal i
        r = responses[min(i, len(responses) - 1)]
        i += 1
        body = r["body"]
        text = json.dumps(body)
        resp = MagicMock()
        resp.status = r["status"]
        resp.headers = {"content-type": "application/json"}
        resp.text = AsyncMock(return_value=text)
        return resp

    return AsyncMock(side_effect=_fetch)


def make_deps(conn, providers, fetch_impl, buffer_ms=10 * 60 * 1000, interval_ms=60_000):
    return {
        "conn": conn,
        "providers": providers,
        "encryption_key": TEST_KEY,
        "fetch": fetch_impl,
        "token_refresh_interval_ms": interval_ms,
        "token_refresh_buffer_ms": buffer_ms,
    }


# ── run_refresh_tick — expiry selection ──────────────────────────────────────


class TestRunRefreshTickExpirySelection:
    async def test_refreshes_connection_expiring_within_buffer(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                conn_id = await store_connection(conn, "u1", "google", {
                    "access_token": "old-access", "refresh_token": "refresh-tok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                await conn.execute(
                    "UPDATE _addition.connections SET token_expires_at = NOW() + interval '2 minutes' WHERE id = $1",
                    conn_id,
                )
                spy = mk_fetch([{"status": 200, "body": {"access_token": "new-access", "expires_in": 3600}}])
                await run_refresh_tick(make_deps(conn, providers, spy, buffer_ms=10 * 60 * 1000))
                refreshed = await get_connection_by_id(conn, conn_id, TEST_KEY)
                assert refreshed["access_token"] == "new-access"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_skips_connection_expiring_outside_buffer(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                conn_id = await store_connection(conn, "u1", "google", {
                    "access_token": "original", "refresh_token": "refresh-tok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                await conn.execute(
                    "UPDATE _addition.connections SET token_expires_at = NOW() + interval '30 minutes' WHERE id = $1",
                    conn_id,
                )
                spy = mk_fetch([])
                await run_refresh_tick(make_deps(conn, providers, spy, buffer_ms=5 * 60 * 1000))
                record = await get_connection_by_id(conn, conn_id, TEST_KEY)
                assert record["access_token"] == "original"
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_skips_connections_without_refresh_token(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                conn_id = await store_connection(conn, "u1", "github", {
                    "access_token": "tok", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                await conn.execute(
                    "UPDATE _addition.connections SET token_expires_at = NOW() + interval '1 minute' WHERE id = $1",
                    conn_id,
                )
                spy = mk_fetch([])
                await run_refresh_tick(make_deps(conn, providers, spy))
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_skips_connections_with_null_expiry(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                conn_id = await store_connection(conn, "u1", "google", {
                    "access_token": "tok", "refresh_token": "rtok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([])
                await run_refresh_tick(make_deps(conn, providers, spy))
                spy.assert_not_called()
                record = await get_connection_by_id(conn, conn_id, TEST_KEY)
                assert record["access_token"] == "tok"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── run_refresh_tick — failure isolation ─────────────────────────────────────


class TestRunRefreshTickFailureIsolation:
    async def test_continues_after_one_connection_fails(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                conn_id1 = await store_connection(conn, "u1", "google", {
                    "access_token": "old1", "refresh_token": "bad-tok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                conn_id2 = await store_connection(conn, "u2", "google", {
                    "access_token": "old2", "refresh_token": "good-tok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                await conn.execute(
                    "UPDATE _addition.connections SET token_expires_at = NOW() + interval '1 minute' "
                    "WHERE id = ANY($1::uuid[])",
                    [conn_id1, conn_id2],
                )
                spy = mk_fetch([
                    {"status": 400, "body": {"error": "invalid_grant"}},
                    {"status": 200, "body": {"access_token": "new2", "expires_in": 3600}},
                ])
                await run_refresh_tick(make_deps(conn, providers, spy))
                c2 = await get_connection_by_id(conn, conn_id2, TEST_KEY)
                assert c2["access_token"] == "new2"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_refreshes_multiple_expiring_connections(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                conn_id1 = await store_connection(conn, "u1", "google", {
                    "access_token": "old1", "refresh_token": "tok1",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                conn_id2 = await store_connection(conn, "u2", "google", {
                    "access_token": "old2", "refresh_token": "tok2",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                await conn.execute(
                    "UPDATE _addition.connections SET token_expires_at = NOW() + interval '1 minute' "
                    "WHERE id = ANY($1::uuid[])",
                    [conn_id1, conn_id2],
                )
                spy = mk_fetch([
                    {"status": 200, "body": {"access_token": "new1", "expires_in": 3600}},
                    {"status": 200, "body": {"access_token": "new2", "expires_in": 3600}},
                ])
                await run_refresh_tick(make_deps(conn, providers, spy))
                c1 = await get_connection_by_id(conn, conn_id1, TEST_KEY)
                c2 = await get_connection_by_id(conn, conn_id2, TEST_KEY)
                assert c1["access_token"] == "new1"
                assert c2["access_token"] == "new2"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── start_background_tasks / stop / shutdown ──────────────────────────────────


class TestStartBackgroundTasks:
    async def test_fires_tick_at_interval(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                fired = []

                async def tick_fn(d):
                    fired.append(1)

                d = make_deps(conn, providers, mk_fetch([]), interval_ms=20)
                d["_tick_fn"] = tick_fn
                handle = start_background_tasks(d)
                await asyncio.sleep(0.05)
                handle.stop()
                assert len(fired) >= 1
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_fires_tick_multiple_times(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                fired = []

                async def tick_fn(d):
                    fired.append(1)

                d = make_deps(conn, providers, mk_fetch([]), interval_ms=20)
                d["_tick_fn"] = tick_fn
                handle = start_background_tasks(d)
                await asyncio.sleep(0.1)
                handle.stop()
                assert len(fired) >= 2
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_stop_prevents_further_ticks(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                fired = []

                async def tick_fn(d):
                    fired.append(1)

                d = make_deps(conn, providers, mk_fetch([]), interval_ms=20)
                d["_tick_fn"] = tick_fn
                handle = start_background_tasks(d)
                await asyncio.sleep(0.05)
                handle.stop()
                count_after_stop = len(fired)
                await asyncio.sleep(0.1)
                assert len(fired) == count_after_stop
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_shutdown_closes_connection(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                async def tick_fn(d):
                    pass

                d = make_deps(conn, providers, mk_fetch([]))
                d["_tick_fn"] = tick_fn
                handle = start_background_tasks(d)
                await handle.shutdown()
                # Connection should be closed after shutdown
                assert conn.is_closed()
            finally:
                if not conn.is_closed():
                    await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_shutdown_stops_further_ticks(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                fired = []

                async def tick_fn(d):
                    fired.append(1)

                d = make_deps(conn, providers, mk_fetch([]), interval_ms=20)
                d["_tick_fn"] = tick_fn
                handle = start_background_tasks(d)
                await handle.shutdown()
                count = len(fired)
                await asyncio.sleep(0.1)
                assert len(fired) == count
            finally:
                if not conn.is_closed():
                    await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)
