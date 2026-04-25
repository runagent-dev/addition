"""Integration tests for token manager — requires live Postgres (testcontainers)."""
from __future__ import annotations

import json
import random
import string
import time
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

import asyncpg
import pytest
import yaml

from runagent_addition.crypto import encrypt, decrypt
from runagent_addition.errors import ConnectionNotFoundError, TokenRefreshError
from runagent_addition.providers import load_providers
from runagent_addition.schema import bootstrap
from runagent_addition.tokens import (
    store_connection,
    get_connection_by_id,
    find_active_connection,
    refresh_connection,
    list_connections,
    revoke_connection,
)

SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"
SHARED_SCHEMA = Path(__file__).parents[3] / "shared" / "schema"
SHARED_FIXTURES = Path(__file__).parents[3] / "shared" / "test-fixtures"
TEST_KEY = "b" * 64


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer
    with PostgresContainer("postgres:16-alpine", dbname="addition_tokens_test") as pg:
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
    db_name = f"tokens_{suffix}"
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
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1",
            db_name,
        )
        await conn.execute(f'DROP DATABASE IF EXISTS "{db_name}"')
    finally:
        await conn.close()


async def _seed_provider(conn, provider_id, client_id="test-client-id", client_secret="test-client-secret"):
    await conn.execute(
        "INSERT INTO _addition.provider_configs (provider_id, client_id, client_secret_encrypted, scopes) "
        "VALUES ($1, $2, $3, $4)",
        provider_id, client_id, encrypt(client_secret, TEST_KEY), [],
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


def deps(conn, providers, fetch_impl=None):
    return {
        "conn": conn,
        "providers": providers,
        "encryption_key": TEST_KEY,
        "fetch": fetch_impl or mk_fetch([{"status": 200, "body": {}}]),
    }


SAMPLE_TOKENS = {
    "access_token": "plaintext-access",
    "refresh_token": "plaintext-refresh",
    "expires_in": 3600,
    "scopes_granted": ["read", "write"],
}


# ── storeConnection ───────────────────────────────────────────────────────────


class TestStoreConnection:
    async def test_db_row_has_only_ciphertext(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                row = await conn.fetchrow(
                    "SELECT access_token_encrypted, refresh_token_encrypted FROM _addition.connections WHERE id = $1",
                    cid,
                )
                assert "plaintext-access" not in row["access_token_encrypted"]
                assert "plaintext-refresh" not in row["refresh_token_encrypted"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_encrypts_both_tokens(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                row = await conn.fetchrow(
                    "SELECT access_token_encrypted, refresh_token_encrypted FROM _addition.connections WHERE id = $1",
                    cid,
                )
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == "plaintext-access"
                assert decrypt(row["refresh_token_encrypted"], TEST_KEY) == "plaintext-refresh"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_null_refresh_token_stored_as_null(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                cid = await store_connection(conn, "u1", "github", {
                    "access_token": "gh-token", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                row = await conn.fetchrow(
                    "SELECT refresh_token_encrypted FROM _addition.connections WHERE id = $1", cid
                )
                assert row["refresh_token_encrypted"] is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_uuid_connection_id(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                import re
                assert re.match(r"^[0-9a-f-]{36}$", str(cid))
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_connection_created_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE connection_id = $1", cid
                )
                assert any(r["event_type"] == "connection.created" for r in rows)
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── getConnectionById ─────────────────────────────────────────────────────────


class TestGetConnectionById:
    async def test_returns_decrypted_tokens(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                record = await get_connection_by_id(conn, cid, TEST_KEY)
                assert record is not None
                assert record["access_token"] == "plaintext-access"
                assert record["refresh_token"] == "plaintext-refresh"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_none_for_unknown_id(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                result = await get_connection_by_id(conn, "00000000-0000-0000-0000-000000000000", TEST_KEY)
                assert result is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── findActiveConnection ──────────────────────────────────────────────────────


class TestFindActiveConnection:
    async def test_returns_decrypted_tokens_for_user_provider(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                record = await find_active_connection(conn, "u1", "google", TEST_KEY)
                assert record is not None
                assert record["access_token"] == "plaintext-access"
                assert record["refresh_token"] == "plaintext-refresh"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_none_when_no_active_connection(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                result = await find_active_connection(conn, "nobody", "google", TEST_KEY)
                assert result is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_most_recently_created_active_connection(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                await store_connection(conn, "u1", "google", {
                    "access_token": "old-token", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                await store_connection(conn, "u1", "google", {
                    "access_token": "new-token", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                record = await find_active_connection(conn, "u1", "google", TEST_KEY)
                assert record["access_token"] == "new-token"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_does_not_return_refresh_failed_connection(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                await conn.execute(
                    "UPDATE _addition.connections SET status = 'refresh_failed' WHERE id = $1", cid
                )
                result = await find_active_connection(conn, "u1", "google", TEST_KEY)
                assert result is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_includes_token_expires_at_when_expires_in_provided(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                before = time.time()
                await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                after = time.time()
                record = await find_active_connection(conn, "u1", "google", TEST_KEY)
                assert record["token_expires_at"] is not None
                ts = record["token_expires_at"].timestamp()
                assert ts >= before + 3599
                assert ts <= after + 3601
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── refreshConnection ─────────────────────────────────────────────────────────


class TestRefreshConnection:
    async def test_posts_grant_type_refresh_token(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                spy = AsyncMock(return_value=MagicMock(
                    status=200,
                    headers={"content-type": "application/json"},
                    text=AsyncMock(return_value='{"access_token":"new-access","expires_in":3600}'),
                ))
                await refresh_connection(cid, deps(conn, providers, spy))
                call = spy.call_args
                body = call.kwargs.get("data", "")
                assert "grant_type=refresh_token" in body
                from urllib.parse import quote
                assert f"refresh_token={quote('plaintext-refresh', safe='')}" in body
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_updates_row_with_new_access_token_and_expires_at(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "refreshed", "expires_in": 7200}}])
                await refresh_connection(cid, deps(conn, providers, fetch_impl))
                row = await conn.fetchrow(
                    "SELECT access_token_encrypted, token_expires_at FROM _addition.connections WHERE id = $1", cid
                )
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == "refreshed"
                assert row["token_expires_at"] is not None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_keeps_existing_refresh_token_when_none_returned(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "refreshed", "expires_in": 3600}}])
                await refresh_connection(cid, deps(conn, providers, fetch_impl))
                row = await conn.fetchrow(
                    "SELECT refresh_token_encrypted FROM _addition.connections WHERE id = $1", cid
                )
                assert decrypt(row["refresh_token_encrypted"], TEST_KEY) == "plaintext-refresh"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_token_refresh_succeeded_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "refreshed", "expires_in": 3600}}])
                await refresh_connection(cid, deps(conn, providers, fetch_impl))
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE connection_id = $1", cid
                )
                assert any(r["event_type"] == "token.refresh_succeeded" for r in rows)
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_noop_when_refresh_not_supported(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                cid = await store_connection(conn, "u1", "github", {
                    "access_token": "gh-access", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = AsyncMock()
                result = await refresh_connection(cid, deps(conn, providers, spy))
                spy.assert_not_called()
                assert result["access_token"] == "gh-access"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_noop_when_no_refresh_token_stored(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", {
                    "access_token": "access", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = AsyncMock()
                result = await refresh_connection(cid, deps(conn, providers, spy))
                spy.assert_not_called()
                assert result["access_token"] == "access"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_raises_connection_not_found_for_unknown_id(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                with pytest.raises(ConnectionNotFoundError):
                    await refresh_connection("00000000-0000-0000-0000-000000000000", deps(conn, providers))
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_preserves_expires_at_when_provider_omits_expires_in(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                before_row = await conn.fetchrow(
                    "SELECT token_expires_at FROM _addition.connections WHERE id = $1", cid
                )
                original_expiry = before_row["token_expires_at"]
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "refreshed"}}])
                await refresh_connection(cid, deps(conn, providers, fetch_impl))
                after_row = await conn.fetchrow(
                    "SELECT token_expires_at FROM _addition.connections WHERE id = $1", cid
                )
                assert after_row["token_expires_at"] == original_expiry
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── refresh failure ───────────────────────────────────────────────────────────


class TestRefreshFailure:
    async def test_sets_status_refresh_failed(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 400, "body": {"error": "invalid_grant"}}])
                with pytest.raises(TokenRefreshError):
                    await refresh_connection(cid, deps(conn, providers, fetch_impl))
                row = await conn.fetchrow(
                    "SELECT status FROM _addition.connections WHERE id = $1", cid
                )
                assert row["status"] == "refresh_failed"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_does_not_overwrite_existing_tokens_on_failure(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 400, "body": {"error": "invalid_grant"}}])
                with pytest.raises(TokenRefreshError):
                    await refresh_connection(cid, deps(conn, providers, fetch_impl))
                row = await conn.fetchrow(
                    "SELECT access_token_encrypted FROM _addition.connections WHERE id = $1", cid
                )
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == "plaintext-access"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_token_refresh_failed_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 400, "body": {"error": "invalid_grant"}}])
                with pytest.raises(TokenRefreshError):
                    await refresh_connection(cid, deps(conn, providers, fetch_impl))
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE connection_id = $1", cid
                )
                assert any(r["event_type"] == "token.refresh_failed" for r in rows)
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_raises_token_refresh_error_with_provider_error_code(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                fetch_impl = mk_fetch([{"status": 400, "body": {"error": "invalid_grant", "error_description": "Token revoked"}}])
                err = None
                try:
                    await refresh_connection(cid, deps(conn, providers, fetch_impl))
                except TokenRefreshError as e:
                    err = e
                assert err is not None
                assert err.code == "invalid_grant"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── listConnections ───────────────────────────────────────────────────────────


class TestListConnections:
    async def test_returns_all_connections_for_user(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                await store_connection(conn, "u1", "github", {
                    "access_token": "gh", "refresh_token": None,
                    "expires_in": None, "scopes_granted": ["repo"],
                }, TEST_KEY)
                result = await list_connections(conn, "u1")
                assert len(result) == 2
                provider_ids = [c["provider_id"] for c in result]
                assert "google" in provider_ids
                assert "github" in provider_ids
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_empty_when_no_connections(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                result = await list_connections(conn, "ghost-user")
                assert result == []
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_does_not_return_plaintext_tokens(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                result = await list_connections(conn, "u1")
                entry = result[0]
                assert "access_token" not in entry
                assert "refresh_token" not in entry
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── revokeConnection ──────────────────────────────────────────────────────────


class TestRevokeConnection:
    async def test_removes_connection_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                await revoke_connection(conn, cid)
                result = await get_connection_by_id(conn, cid, TEST_KEY)
                assert result is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_noop_for_unknown_id(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await revoke_connection(conn, "00000000-0000-0000-0000-000000000000")
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_connection_revoked_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                cid = await store_connection(conn, "u1", "google", SAMPLE_TOKENS, TEST_KEY)
                await revoke_connection(conn, cid)
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE connection_id = $1", cid
                )
                assert any(r["event_type"] == "connection.revoked" for r in rows)
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Conformance (token_refresh.yaml) ─────────────────────────────────────────


def _load_refresh_scenarios():
    fixture_path = SHARED_FIXTURES / "token_refresh.yaml"
    with open(fixture_path) as fh:
        data = yaml.safe_load(fh)
    return data["scenarios"]


@pytest.mark.parametrize("scenario", _load_refresh_scenarios(), ids=lambda s: s["name"])
async def test_token_refresh_conformance(scenario, admin_dsn, providers, request):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        conn = await asyncpg.connect(dsn)
        try:
            await _seed_provider(conn, scenario["provider"])
            initial = scenario["initial"]
            cid = await store_connection(conn, "conformance-user", scenario["provider"], {
                "access_token": initial["access_token"],
                "refresh_token": initial.get("refresh_token"),
                "expires_in": None,
                "scopes_granted": [],
            }, TEST_KEY)

            fetch_impl = mk_fetch([scenario["mock_response"]])
            d = deps(conn, providers, fetch_impl)

            expected = scenario["expected"]
            if expected["status"] == "refresh_failed":
                with pytest.raises(TokenRefreshError):
                    await refresh_connection(cid, d)
            else:
                await refresh_connection(cid, d)

            row = await conn.fetchrow(
                "SELECT access_token_encrypted, refresh_token_encrypted, status FROM _addition.connections WHERE id = $1",
                cid,
            )
            assert row["status"] == expected["status"]
            if expected["status"] != "refresh_failed":
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == expected["access_token"]
                if "refresh_token" in expected:
                    assert decrypt(row["refresh_token_encrypted"], TEST_KEY) == expected["refresh_token"]
            else:
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == initial["access_token"]
                if "refresh_token" in initial:
                    assert decrypt(row["refresh_token_encrypted"], TEST_KEY) == initial["refresh_token"]
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)
