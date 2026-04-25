"""Integration tests for proxy engine — requires live Postgres (testcontainers)."""
from __future__ import annotations

import json
import random
import string
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

import asyncpg
import pytest
import yaml

from runagent_addition.crypto import encrypt
from runagent_addition.errors import (
    ConnectionExpiredError,
    ConnectionNotFoundError,
    ProxyError,
    ProviderNotFoundError,
)
from runagent_addition.providers import load_providers
from runagent_addition.schema import bootstrap
from runagent_addition.tokens import store_connection
from runagent_addition.proxy import proxy

SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"
SHARED_SCHEMA = Path(__file__).parents[3] / "shared" / "schema"
SHARED_FIXTURES = Path(__file__).parents[3] / "shared" / "test-fixtures"
TEST_KEY = "c" * 64


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer
    with PostgresContainer("postgres:16-alpine", dbname="addition_proxy_test") as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(pg_container):
    return pg_container.get_connection_url().replace("psycopg2", "asyncpg").replace("+asyncpg", "")


@pytest.fixture(scope="session")
def providers():
    return load_providers(str(SHARED_PROVIDERS))


# ── Helpers ───────────────────��────────────────────────────────────��──────────


async def _with_fresh_db(admin_dsn: str):
    suffix = "".join(random.choices(string.ascii_lowercase, k=8))
    db_name = f"proxy_{suffix}"
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
        if r.get("throws"):
            raise OSError("fetch failed")
        body = r["body"]
        text = body if isinstance(body, str) else json.dumps(body)
        headers = {"content-type": "application/json", **(r.get("headers") or {})}
        resp = MagicMock()
        resp.status = r["status"]
        resp.headers = headers
        resp.text = AsyncMock(return_value=text)
        resp.json = AsyncMock(return_value=json.loads(text) if not isinstance(body, str) else json.loads(text))
        return resp

    return AsyncMock(side_effect=_fetch)


NO_SLEEP = AsyncMock(return_value=None)


def deps(conn, providers, fetch_impl, sleep=None):
    return {
        "conn": conn,
        "providers": providers,
        "encryption_key": TEST_KEY,
        "fetch": fetch_impl,
        "sleep": sleep or NO_SLEEP,
    }


# ── Auth header injection ─────────────────────────────────────────────────────


class TestAuthHeaderInjection:
    async def test_injects_bearer_token(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "my-token", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {"login": "u1"}}])
                await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                    deps(conn, providers, spy),
                )
                call = spy.call_args
                sent_headers = call.kwargs.get("headers", {})
                assert sent_headers.get("Authorization") == "Bearer my-token"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_uses_provider_base_url_and_path(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {}}])
                await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/repos/owner/repo"},
                    deps(conn, providers, spy),
                )
                url = spy.call_args.args[0]
                assert "api.github.com" in url
                assert "/repos/owner/repo" in url
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_appends_query_params(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": []}])
                await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET",
                     "path": "/user/repos", "params": {"per_page": 10, "sort": "updated"}},
                    deps(conn, providers, spy),
                )
                url = spy.call_args.args[0]
                assert "per_page=10" in url
                assert "sort=updated" in url
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_serialises_body_as_json_and_sets_content_type(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 201, "body": {"id": 1}}])
                await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "POST",
                     "path": "/repos/o/r/issues", "body": {"title": "Bug"}},
                    deps(conn, providers, spy),
                )
                call = spy.call_args
                sent_body = call.kwargs.get("content", call.kwargs.get("data", ""))
                assert json.loads(sent_body) == {"title": "Bug"}
                sent_headers = call.kwargs.get("headers", {})
                assert "application/json" in sent_headers.get("Content-Type", "")
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_merges_caller_headers_with_auth(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {}}])
                await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET",
                     "path": "/user", "headers": {"X-Custom": "value"}},
                    deps(conn, providers, spy),
                )
                call = spy.call_args
                sent_headers = call.kwargs.get("headers", {})
                assert sent_headers.get("Authorization")
                assert sent_headers.get("X-Custom") == "value"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_status_and_body(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {"login": "sawradip"}}])
                result = await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                    deps(conn, providers, spy),
                )
                assert result["status"] == 200
                assert result["body"]["login"] == "sawradip"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── 401 / token refresh ──────────────────────────────��────────────────────────


class TestTokenRefresh:
    async def test_401_with_refresh_token_refreshes_and_retries(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                await store_connection(conn, "u1", "google", {
                    "access_token": "old-access", "refresh_token": "refresh-tok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 401, "body": {"error": "invalid_credentials"}},
                    {"status": 200, "body": {"access_token": "new-access", "expires_in": 3600}},
                    {"status": 200, "body": {"id": "me"}},
                ])
                result = await proxy(
                    {"user_id": "u1", "provider_id": "google", "method": "GET", "path": "/oauth2/v1/userinfo"},
                    deps(conn, providers, spy),
                )
                assert spy.call_count == 3
                assert result["status"] == 200
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_401_retry_uses_new_access_token(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google")
                await store_connection(conn, "u1", "google", {
                    "access_token": "old-access", "refresh_token": "refresh-tok",
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 401, "body": {}},
                    {"status": 200, "body": {"access_token": "brand-new", "expires_in": 3600}},
                    {"status": 200, "body": {}},
                ])
                await proxy(
                    {"user_id": "u1", "provider_id": "google", "method": "GET", "path": "/oauth2/v1/userinfo"},
                    deps(conn, providers, spy),
                )
                retry_call = spy.call_args_list[2]
                retry_headers = retry_call.kwargs.get("headers", {})
                assert retry_headers.get("Authorization") == "Bearer brand-new"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_401_without_refresh_token_raises_connection_expired(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "old", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 401, "body": {}}])
                with pytest.raises(ConnectionExpiredError):
                    await proxy(
                        {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                        deps(conn, providers, spy),
                    )
                assert spy.call_count == 1
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── 429 retry ───────────────────────────��───────────────────────────────────���─


class TestRateLimitRetry:
    async def test_retries_up_to_3_times_then_succeeds(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 429, "body": {}, "headers": {"Retry-After": "1"}},
                    {"status": 429, "body": {}, "headers": {"Retry-After": "1"}},
                    {"status": 200, "body": {"ok": True}},
                ])
                result = await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                    deps(conn, providers, spy),
                )
                assert spy.call_count == 3
                assert result["status"] == 200
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_sleep_called_with_retry_after_ms(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 429, "body": {}, "headers": {"Retry-After": "2"}},
                    {"status": 200, "body": {}},
                ])
                sleep_spy = AsyncMock(return_value=None)
                await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                    deps(conn, providers, spy, sleep=sleep_spy),
                )
                sleep_spy.assert_called_with(2000)
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_gives_up_after_3_retries_returns_last_429(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 429, "body": {}, "headers": {"Retry-After": "1"}},
                    {"status": 429, "body": {}, "headers": {"Retry-After": "1"}},
                    {"status": 429, "body": {}, "headers": {"Retry-After": "1"}},
                    {"status": 429, "body": {}, "headers": {"Retry-After": "1"}},
                ])
                result = await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                    deps(conn, providers, spy),
                )
                assert result["status"] == 429
                assert spy.call_count == 4  # 1 initial + 3 retries
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Adapter error detection ───────────────────────────────��───────────────────


class TestAdapterErrorDetection:
    async def test_slack_ok_false_raises_proxy_error(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "slack")
                await store_connection(conn, "u1", "slack", {
                    "access_token": "xoxp-token", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {"ok": False, "error": "channel_not_found"}}])
                with pytest.raises(ProxyError):
                    await proxy(
                        {"user_id": "u1", "provider_id": "slack", "method": "GET", "path": "/conversations.list"},
                        deps(conn, providers, spy),
                    )
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_proxy_error_carries_adapter_error_code(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "slack")
                await store_connection(conn, "u1", "slack", {
                    "access_token": "xoxp-token", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {"ok": False, "error": "not_authed"}}])
                err = None
                try:
                    await proxy(
                        {"user_id": "u1", "provider_id": "slack", "method": "GET", "path": "/conversations.list"},
                        deps(conn, providers, spy),
                    )
                except ProxyError as e:
                    err = e
                assert err is not None
                assert err.code == "not_authed"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Network error retry ───────────────────────────────────────────────────────


class TestNetworkErrorRetry:
    async def test_retries_once_on_network_error(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 0, "body": None, "throws": True},
                    {"status": 200, "body": {"ok": True}},
                ])
                result = await proxy(
                    {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                    deps(conn, providers, spy),
                )
                assert spy.call_count == 2
                assert result["status"] == 200
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_rethrows_after_two_consecutive_network_errors(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                await store_connection(conn, "u1", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([
                    {"status": 0, "body": None, "throws": True},
                    {"status": 0, "body": None, "throws": True},
                ])
                with pytest.raises(Exception):
                    await proxy(
                        {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user"},
                        deps(conn, providers, spy),
                    )
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Connection not found ──────────────────────────────────────────────────────


class TestConnectionNotFound:
    async def test_raises_when_no_active_connection(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                spy = mk_fetch([{"status": 200, "body": {}}])
                with pytest.raises(ConnectionNotFoundError):
                    await proxy(
                        {"user_id": "nobody", "provider_id": "github", "method": "GET", "path": "/user"},
                        deps(conn, providers, spy),
                    )
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_raises_when_explicit_connection_id_not_found(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                spy = mk_fetch([{"status": 200, "body": {}}])
                with pytest.raises(ConnectionNotFoundError):
                    await proxy(
                        {"user_id": "u1", "provider_id": "github", "method": "GET", "path": "/user",
                         "connection_id": "00000000-0000-0000-0000-000000000000"},
                        deps(conn, providers, spy),
                    )
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_raises_when_connection_belongs_to_different_user(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github")
                conn_id = await store_connection(conn, "owner", "github", {
                    "access_token": "t", "refresh_token": None,
                    "expires_in": None, "scopes_granted": [],
                }, TEST_KEY)
                spy = mk_fetch([{"status": 200, "body": {}}])
                with pytest.raises(ConnectionNotFoundError):
                    await proxy(
                        {"user_id": "attacker", "provider_id": "github", "method": "GET",
                         "path": "/user", "connection_id": conn_id},
                        deps(conn, providers, spy),
                    )
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Provider guard ────────────────────────────────────────────────────────────


class TestProviderGuard:
    async def test_raises_provider_not_found_for_unknown_provider(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                spy = mk_fetch([{"status": 200, "body": {}}])
                with pytest.raises(ProviderNotFoundError):
                    await proxy(
                        {"user_id": "u1", "provider_id": "nonexistent", "method": "GET", "path": "/foo"},
                        deps(conn, providers, spy),
                    )
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── Conformance (proxy_requests.yaml) ──────────────────��─────────────────────


def _load_proxy_scenarios():
    with open(SHARED_FIXTURES / "proxy_requests.yaml") as fh:
        return yaml.safe_load(fh)["scenarios"]


@pytest.mark.parametrize("scenario", _load_proxy_scenarios(), ids=lambda s: s["name"])
async def test_proxy_conformance(scenario, admin_dsn, providers):
    dsn, db_name = await _with_fresh_db(admin_dsn)
    try:
        conn = await asyncpg.connect(dsn)
        try:
            await _seed_provider(conn, scenario["provider"])
            await store_connection(conn, "conformance-user", scenario["provider"], {
                "access_token": "conformance-token", "refresh_token": None,
                "expires_in": None, "scopes_granted": [],
            }, TEST_KEY)

            spy = mk_fetch([scenario["mock_response"]])
            args = {
                "user_id": "conformance-user",
                "provider_id": scenario["provider"],
                "method": scenario["method"],
                "path": scenario["path"],
            }
            if scenario.get("params"):
                args["params"] = scenario["params"]
            if scenario.get("body") is not None:
                args["body"] = scenario["body"]

            result = await proxy(args, deps(conn, providers, spy))

            expected = scenario["expected"]
            assert result["status"] == expected["status"]

            call = spy.call_args
            url = call.args[0]
            call_headers = call.kwargs.get("headers", {})
            call_body = call.kwargs.get("content", call.kwargs.get("data"))

            if expected.get("auth_header") and expected.get("auth_prefix"):
                val = call_headers.get(expected["auth_header"], "")
                assert val.startswith(expected["auth_prefix"] + " ")

            if expected.get("url_query_params"):
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query)
                for k, v in expected["url_query_params"].items():
                    assert qs.get(k) == [str(v)]

            if expected.get("request_has_body"):
                assert call_body
        finally:
            await conn.close()
    finally:
        await _drop_db(admin_dsn, db_name)
