"""Integration tests for the Addition main class — requires live Postgres."""
from __future__ import annotations

import json
import random
import string
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

import asyncpg
import pytest

from runagent_addition.crypto import encrypt
from runagent_addition.errors import EncryptionError
from runagent_addition.addition import Addition

SHARED_SCHEMA = Path(__file__).parents[3] / "shared" / "schema"
TEST_KEY = "f" * 64
CALLBACK_URL = "http://localhost:3000/api/addition/callback"


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer
    with PostgresContainer("postgres:16-alpine", dbname="addition_main_test") as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(pg_container):
    return pg_container.get_connection_url().replace("psycopg2", "asyncpg").replace("+asyncpg", "")


# ── Helpers ───────────────────────────────────────────────────────────────────


async def _with_fresh_db(admin_dsn: str):
    suffix = "".join(random.choices(string.ascii_lowercase, k=8))
    db_name = f"add_{suffix}"
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
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", db_name
        )
        await conn.execute(f'DROP DATABASE IF EXISTS "{db_name}"')
    finally:
        await conn.close()


def mk_addition(db_url: str, fetch_impl=None) -> Addition:
    return Addition(
        database_url=db_url,
        encryption_key=TEST_KEY,
        callback_url=CALLBACK_URL,
        fetch=fetch_impl,
    )


def mk_fetch(responses: list[dict]) -> AsyncMock:
    i = 0

    async def _fetch(url, **kwargs):
        nonlocal i
        r = responses[min(i, len(responses) - 1)]
        i += 1
        body = r["body"]
        text = body if isinstance(body, str) else json.dumps(body)
        resp = MagicMock()
        resp.status = r["status"]
        resp.headers = {"content-type": "application/json"}
        resp.text = AsyncMock(return_value=text)
        return resp

    return AsyncMock(side_effect=_fetch)


async def _seed_connection(db_url: str, user_id: str, provider_id: str) -> str:
    conn = await asyncpg.connect(db_url)
    try:
        await conn.execute(
            "INSERT INTO _addition.provider_configs (provider_id, client_id, client_secret_encrypted, scopes) "
            "VALUES ($1, $2, $3, $4) ON CONFLICT (provider_id) DO NOTHING",
            provider_id, "test-client", encrypt("test-secret", TEST_KEY), [],
        )
        row = await conn.fetchrow(
            "INSERT INTO _addition.connections "
            "(user_id, provider_id, access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes_granted) "
            "VALUES ($1, $2, $3, $4, $5, $6) RETURNING id",
            user_id, provider_id, encrypt("tok", TEST_KEY), None, None, [],
        )
        return str(row["id"])
    finally:
        await conn.close()


# ── Constructor validation ────────────────────────────────────────────────────


class TestConstructor:
    def test_raises_encryption_error_for_short_key(self):
        with pytest.raises(EncryptionError):
            Addition(database_url="postgres://localhost/test", encryption_key="tooshort",
                     callback_url=CALLBACK_URL)

    def test_raises_encryption_error_for_non_hex_key(self):
        with pytest.raises(EncryptionError):
            Addition(database_url="postgres://localhost/test", encryption_key="z" * 64,
                     callback_url=CALLBACK_URL)

    def test_does_not_raise_for_invalid_db_url(self):
        # No DB call on construction
        Addition(database_url="postgres://no-such-host:5432/nope",
                 encryption_key=TEST_KEY, callback_url=CALLBACK_URL)


# ── Schema bootstrap ──────────────────────────────────────────────────────────


class TestSchemaBootstrap:
    async def test_bootstraps_schema_on_first_method_call(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.configure_provider(provider_id="github", client_id="c", client_secret="s")
            conn = await asyncpg.connect(db_url)
            try:
                rows = await conn.fetch(
                    "SELECT table_name FROM information_schema.tables WHERE table_schema = '_addition'"
                )
                assert len(rows) > 0
            finally:
                await conn.close()
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── configureProvider ─────────────────────────────────────────────────────────


class TestConfigureProvider:
    async def test_stores_provider_config(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.configure_provider(provider_id="github", client_id="my-client", client_secret="secret")
            providers = await addition.list_configured_providers()
            assert len(providers) == 1
            assert providers[0]["provider_id"] == "github"
            assert providers[0]["client_id"] == "my-client"
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_idempotent_upsert(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.configure_provider(provider_id="github", client_id="c1", client_secret="s1")
            await addition.configure_provider(provider_id="github", client_id="c2", client_secret="s2")
            providers = await addition.list_configured_providers()
            github = [p for p in providers if p["provider_id"] == "github"]
            assert len(github) == 1
            assert github[0]["client_id"] == "c2"
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_accepts_scopes_override(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.configure_provider(provider_id="github", client_id="c", client_secret="s",
                                              scopes=["repo", "read:user"])
            providers = await addition.list_configured_providers()
            assert "repo" in providers[0]["scopes"]
            assert "read:user" in providers[0]["scopes"]
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── listConfiguredProviders ───────────────────────────────────────────────────


class TestListConfiguredProviders:
    async def test_returns_only_configured_providers(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.configure_provider(provider_id="github", client_id="c", client_secret="s")
            providers = await addition.list_configured_providers()
            assert len(providers) == 1
            assert providers[0]["provider_id"] == "github"
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_empty_when_nothing_configured(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            providers = await addition.list_configured_providers()
            assert providers == []
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Events ────────────────────────────────────────────────────────────────────


class TestEvents:
    async def test_emits_connection_created_after_oauth_callback(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "tok", "expires_in": 3600}}])
            addition = mk_addition(db_url, fetch_impl)
            await addition.configure_provider(provider_id="github", client_id="c", client_secret="s")

            fired = []
            addition.on("connection.created", lambda e: fired.append(e))

            from urllib.parse import urlparse, parse_qs
            auth_url = await addition.get_auth_url(user_id="u1", provider_id="github")
            state = parse_qs(urlparse(auth_url).query)["state"][0]
            await addition.handle_callback({"code": "fake-code", "state": state})

            assert len(fired) == 1
            assert fired[0]["user_id"] == "u1"
            assert fired[0]["provider_id"] == "github"
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_emits_tool_executed_after_execute_tool(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            fetch_impl = mk_fetch([{"status": 200, "body": {"id": 1, "title": "test"}}])
            addition = mk_addition(db_url, fetch_impl)
            await addition.configure_provider(provider_id="github", client_id="c", client_secret="s")

            conn = await asyncpg.connect(db_url)
            try:
                await conn.execute(
                    "INSERT INTO _addition.connections "
                    "(user_id, provider_id, access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes_granted) "
                    "VALUES ($1, $2, $3, $4, $5, $6)",
                    "u1", "github", encrypt("access-tok", TEST_KEY), None, None, [],
                )
            finally:
                await conn.close()

            fired = []
            addition.on("tool.executed", lambda e: fired.append(e))
            await addition.execute_tool(user_id="u1", provider_id="github", tool_id="list_repos", params={})

            assert len(fired) == 1
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_emits_token_refreshed_on_401_proxy_refresh(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            fetch_impl = mk_fetch([
                {"status": 401, "body": {"error": "invalid_credentials"}},
                {"status": 200, "body": {"access_token": "new-tok", "expires_in": 3600}},
                {"status": 200, "body": {"id": "me"}},
            ])
            addition = mk_addition(db_url, fetch_impl)
            await addition.configure_provider(provider_id="google", client_id="c", client_secret="s")

            conn = await asyncpg.connect(db_url)
            try:
                await conn.execute(
                    "INSERT INTO _addition.connections "
                    "(user_id, provider_id, access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes_granted) "
                    "VALUES ($1, $2, $3, $4, $5, $6)",
                    "u1", "google", encrypt("old-tok", TEST_KEY), encrypt("refresh-tok", TEST_KEY), None, [],
                )
            finally:
                await conn.close()

            fired = []
            addition.on("token.refreshed", lambda e: fired.append(e))
            await addition.proxy(user_id="u1", provider_id="google", method="GET", path="/oauth2/v1/userinfo")

            assert len(fired) == 1
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Lifecycle ─────────────────────────────────────────────────────────────────


class TestLifecycle:
    async def test_shutdown_closes_connection(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            await addition.shutdown()
            with pytest.raises(Exception):
                await addition.list_configured_providers()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_start_stop_background_tasks_do_not_raise(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            addition.start_background_tasks()
            addition.stop_background_tasks()
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Connection management ─────────────────────────────────────────────────────


class TestConnectionManagement:
    async def test_get_connection_returns_record_for_known_id(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            conn_id = await _seed_connection(db_url, "u1", "github")
            record = await addition.get_connection(conn_id)
            assert record is not None
            assert record["id"] == conn_id
            assert record["user_id"] == "u1"
            assert record["provider_id"] == "github"
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_get_connection_returns_none_for_unknown_id(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            record = await addition.get_connection("00000000-0000-0000-0000-000000000000")
            assert record is None
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_list_connections_filters_by_provider_id(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            await _seed_connection(db_url, "u1", "github")
            await _seed_connection(db_url, "u1", "github")
            # Also seed a different provider
            conn = await asyncpg.connect(db_url)
            try:
                await conn.execute(
                    "INSERT INTO _addition.provider_configs (provider_id, client_id, client_secret_encrypted, scopes) "
                    "VALUES ($1, $2, $3, $4) ON CONFLICT (provider_id) DO NOTHING",
                    "slack", "test-client", encrypt("test-secret", TEST_KEY), [],
                )
                await conn.execute(
                    "INSERT INTO _addition.connections "
                    "(user_id, provider_id, access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes_granted) "
                    "VALUES ($1, $2, $3, $4, $5, $6)",
                    "u1", "slack", encrypt("tok", TEST_KEY), None, None, [],
                )
            finally:
                await conn.close()
            conns = await addition.list_connections(user_id="u1", provider_id="github")
            assert len(conns) == 2
            assert all(c["provider_id"] == "github" for c in conns)
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_revoke_connection_emits_event_only_for_existing(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            conn_id = await _seed_connection(db_url, "u1", "github")

            fired = []
            addition.on("connection.revoked", lambda e: fired.append(e))

            await addition.revoke_connection(conn_id)
            assert len(fired) == 1

            await addition.revoke_connection(conn_id)
            assert len(fired) == 1  # no second event
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_revoke_connection_no_event_for_nonexistent(self, admin_dsn):
        db_url, db_name = await _with_fresh_db(admin_dsn)
        try:
            addition = mk_addition(db_url)
            await addition.list_configured_providers()
            fired = []
            addition.on("connection.revoked", lambda e: fired.append(e))
            await addition.revoke_connection("00000000-0000-0000-0000-000000000000")
            assert fired == []
            await addition.shutdown()
        finally:
            await _drop_db(admin_dsn, db_name)
