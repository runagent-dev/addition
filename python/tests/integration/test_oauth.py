"""Integration tests for OAuth engine — requires live Postgres (testcontainers)."""
from __future__ import annotations

import asyncio
import hashlib
import base64
import random
import string
from pathlib import Path
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import asyncpg
import pytest
import pytest_asyncio

from runagent_addition.crypto import encrypt, decrypt
from runagent_addition.errors import OAuthError, ProviderDisabledError
from runagent_addition.oauth import get_auth_url, handle_callback
from runagent_addition.providers import load_providers
from runagent_addition.schema import bootstrap

SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"
SHARED_SCHEMA = Path(__file__).parents[3] / "shared" / "schema"
TEST_KEY = "a" * 64
CALLBACK_URL = "https://app.example.com/api/addition/callback"


# ── Container + shared fixtures ───────────────────────────────────────────────


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer

    with PostgresContainer("postgres:16-alpine", dbname="addition_test") as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(pg_container):
    return pg_container.get_connection_url().replace("psycopg2", "asyncpg").replace("+asyncpg", "")


@pytest.fixture(scope="session")
def providers():
    return load_providers(str(SHARED_PROVIDERS))


# ── Per-test fresh DB helper ──────────────────────────────────────────────────


async def _with_fresh_db(admin_dsn: str):
    suffix = "".join(random.choices(string.ascii_lowercase, k=8))
    db_name = f"addition_{suffix}"
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


async def _seed_provider(
    conn: asyncpg.Connection,
    provider_id: str,
    client_id: str = "test-client-id",
    client_secret: str = "test-client-secret",
    scopes: list[str] | None = None,
    enabled: bool = True,
) -> None:
    await conn.execute(
        """INSERT INTO _addition.provider_configs
             (provider_id, client_id, client_secret_encrypted, scopes, enabled)
           VALUES ($1, $2, $3, $4, $5)""",
        provider_id,
        client_id,
        encrypt(client_secret, TEST_KEY),
        scopes or [],
        enabled,
    )


def mk_fetch(responses: list[dict]) -> AsyncMock:
    i = 0

    async def _fetch(url, **kwargs):
        nonlocal i
        r = responses[min(i, len(responses) - 1)]
        i += 1
        body = r["body"]
        text = body if isinstance(body, str) else __import__("json").dumps(body)
        headers = r.get("headers", {"content-type": "application/json"})
        resp = MagicMock()
        resp.status = r["status"]
        resp.headers = headers
        resp.text = AsyncMock(return_value=text)
        resp.json = AsyncMock(return_value=__import__("json").loads(text) if not isinstance(body, str) else __import__("json").loads(text))
        return resp

    return AsyncMock(side_effect=_fetch)


def deps(conn, providers, fetch_impl=None):
    return {
        "conn": conn,
        "providers": providers,
        "encryption_key": TEST_KEY,
        "callback_url": CALLBACK_URL,
        "fetch": fetch_impl or mk_fetch([{"status": 200, "body": {}}]),
    }


# ── getAuthUrl ────────────────────────────────────────────────────────────────


class TestGetAuthUrl:
    async def test_produces_url_with_required_params(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "github"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                parsed = urlparse(url)
                qs = parse_qs(parsed.query)
                assert parsed.scheme + "://" + parsed.netloc + parsed.path == "https://github.com/login/oauth/authorize"
                assert qs["client_id"] == ["gh-client"]
                assert qs["redirect_uri"] == [CALLBACK_URL]
                assert qs["response_type"] == ["code"]
                assert "state" in qs
                assert "scope" in qs
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_scope_delimiter_space_for_google(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "google", "scopes": ["a", "b", "c"]},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query, keep_blank_values=True)
                assert qs["scope"] == ["a b c"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_scope_delimiter_comma_for_slack(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "slack", "sl-client", "sl-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "slack", "scopes": ["chat:write", "users:read"]},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query, keep_blank_values=True)
                assert qs["scope"] == ["chat:write,users:read"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_custom_scopes_override_default(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "github", "scopes": ["only_this"]},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query)
                assert qs["scope"] == ["only_this"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_falls_back_to_default_scopes(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "github"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query)
                assert qs["scope"] == ["repo read:user user:email"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_applies_authorize_params_from_yaml(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "google"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query)
                assert qs["access_type"] == ["offline"]
                assert qs["prompt"] == ["consent"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_pkce_supported_adds_code_challenge(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "google"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query)
                assert "code_challenge" in qs
                assert qs["code_challenge_method"] == ["S256"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_no_pkce_for_github(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "github"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                qs = parse_qs(urlparse(url).query)
                assert "code_challenge" not in qs
                assert "code_challenge_method" not in qs
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_stores_state_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url(
                    {"user_id": "user-42", "provider_id": "github", "scopes": ["repo"]},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                row = await conn.fetchrow(
                    "SELECT user_id, provider_id, redirect_uri, scopes_requested, code_verifier "
                    "FROM _addition.oauth_state WHERE state = $1",
                    state,
                )
                assert row["user_id"] == "user-42"
                assert row["provider_id"] == "github"
                assert row["redirect_uri"] == CALLBACK_URL
                assert list(row["scopes_requested"]) == ["repo"]
                assert row["code_verifier"] is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_stores_code_verifier_and_challenge_matches(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "google"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                parsed_url = urlparse(url)
                qs = parse_qs(parsed_url.query)
                state = qs["state"][0]
                challenge = qs["code_challenge"][0]
                row = await conn.fetchrow(
                    "SELECT code_verifier FROM _addition.oauth_state WHERE state = $1",
                    state,
                )
                verifier = row["code_verifier"]
                assert verifier is not None
                digest = hashlib.sha256(verifier.encode()).digest()
                expected = base64.urlsafe_b64encode(digest).rstrip(b"=").decode()
                assert challenge == expected
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_stores_redirect_after_in_metadata(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "github", "redirect_after": "/welcome"},
                    deps(conn, providers),
                )
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                row = await conn.fetchrow(
                    "SELECT metadata FROM _addition.oauth_state WHERE state = $1",
                    state,
                )
                import json
                meta = json.loads(row["metadata"])
                assert meta.get("redirect_after") == "/welcome"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_raises_when_provider_not_configured(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                with pytest.raises(Exception):
                    await get_auth_url(
                        {"user_id": "u1", "provider_id": "github"},
                        deps(conn, providers),
                    )
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_generates_unique_state_per_call(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                from urllib.parse import urlparse, parse_qs
                states = set()
                for _ in range(5):
                    url = await get_auth_url(
                        {"user_id": "u1", "provider_id": "github"},
                        deps(conn, providers),
                    )
                    states.add(parse_qs(urlparse(url).query)["state"][0])
                assert len(states) == 5
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_raises_provider_disabled_error(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret", enabled=False)
                with pytest.raises(ProviderDisabledError):
                    await get_auth_url(
                        {"user_id": "u1", "provider_id": "github"},
                        deps(conn, providers),
                    )
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_authorize_url_issued_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                await get_auth_url(
                    {"user_id": "u-audit", "provider_id": "github"},
                    deps(conn, providers),
                )
                rows = await conn.fetch(
                    "SELECT event_type, user_id, provider_id FROM _addition.audit_log "
                    "WHERE user_id = 'u-audit' ORDER BY id",
                )
                types = [r["event_type"] for r in rows]
                assert "oauth.authorize_url_issued" in types
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_authorize_url_failed_audit_row_when_disabled(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret", enabled=False)
                with pytest.raises(ProviderDisabledError):
                    await get_auth_url(
                        {"user_id": "u-fail", "provider_id": "github"},
                        deps(conn, providers),
                    )
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE user_id = 'u-fail'",
                )
                types = [r["event_type"] for r in rows]
                assert "oauth.authorize_url_failed" in types
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)


# ── handleCallback ────────────────────────────────────────────────────────────


class TestHandleCallback:
    async def test_valid_state_exchanges_code_stores_tokens_returns_success(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "tok", "token_type": "bearer"}}])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback({"code": "c", "state": state}, d)
                assert result["success"] is True
                assert "connection_id" in result
                row = await conn.fetchrow(
                    "SELECT access_token_encrypted FROM _addition.connections WHERE id = $1",
                    result["connection_id"],
                )
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == "tok"
                # state row deleted
                state_row = await conn.fetchrow(
                    "SELECT 1 FROM _addition.oauth_state WHERE state = $1", state
                )
                assert state_row is None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_unknown_state_returns_invalid_state(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                result = await handle_callback(
                    {"code": "c", "state": "no-such-state"},
                    deps(conn, providers),
                )
                assert result["success"] is False
                assert result["error"] == "invalid_state"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_expired_state_returns_invalid_state(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, deps(conn, providers))
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                # Expire the state row manually
                await conn.execute(
                    "UPDATE _addition.oauth_state SET expires_at = NOW() - INTERVAL '1 second' WHERE state = $1",
                    state,
                )
                result = await handle_callback({"code": "c", "state": state}, deps(conn, providers))
                assert result["success"] is False
                assert result["error"] == "invalid_state"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_provider_error_in_query_returns_error_code(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, deps(conn, providers))
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback(
                    {"error": "access_denied", "error_description": "User denied", "state": state},
                    deps(conn, providers),
                )
                assert result["success"] is False
                assert result["error"] == "access_denied"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_github_adapter_sends_accept_json_header(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                spy = AsyncMock(return_value=MagicMock(
                    status=200,
                    headers={"content-type": "application/json"},
                    text=AsyncMock(return_value='{"access_token":"t","token_type":"bearer"}'),
                ))
                d = deps(conn, providers, spy)
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                await handle_callback({"code": "c", "state": state}, d)
                call_kwargs = spy.call_args
                sent_headers = call_kwargs.kwargs.get("headers", {})
                header_values = {k.lower(): v for k, v in sent_headers.items()}
                assert header_values.get("accept") == "application/json"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_slack_ok_false_returns_error_on_200(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "slack", "sl-client", "sl-secret")
                fetch_impl = mk_fetch([{
                    "status": 200,
                    "body": {"ok": False, "error": "invalid_client_id"},
                }])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "slack"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback({"code": "c", "state": state}, d)
                assert result["success"] is False
                assert result["error"] == "invalid_client_id"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_slack_happy_path_extracts_tokens_from_mapping(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "slack", "sl-client", "sl-secret")
                fetch_impl = mk_fetch([{
                    "status": 200,
                    "body": {
                        "ok": True,
                        "authed_user": {
                            "id": "U1",
                            "access_token": "xoxp-user-token",
                            "refresh_token": "xoxp-refresh",
                            "expires_in": 43200,
                        },
                    },
                }])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "slack"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback({"code": "c", "state": state}, d)
                assert result["success"] is True
                row = await conn.fetchrow(
                    "SELECT access_token_encrypted, refresh_token_encrypted, token_expires_at "
                    "FROM _addition.connections WHERE id = $1",
                    result["connection_id"],
                )
                assert decrypt(row["access_token_encrypted"], TEST_KEY) == "xoxp-user-token"
                assert decrypt(row["refresh_token_encrypted"], TEST_KEY) == "xoxp-refresh"
                assert row["token_expires_at"] is not None
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_pkce_code_verifier_sent_to_token_url(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                spy = AsyncMock(return_value=MagicMock(
                    status=200,
                    headers={"content-type": "application/json"},
                    text=AsyncMock(return_value='{"access_token":"g-access","refresh_token":"g-refresh","expires_in":3600}'),
                ))
                d = deps(conn, providers, spy)
                url = await get_auth_url({"user_id": "u1", "provider_id": "google"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                row = await conn.fetchrow(
                    "SELECT code_verifier FROM _addition.oauth_state WHERE state = $1", state
                )
                verifier = row["code_verifier"]
                await handle_callback({"code": "c", "state": state}, d)
                call = spy.call_args
                body = call[1].get("data", call[1].get("content", ""))
                assert f"code_verifier={verifier}" in body or __import__("urllib.parse", fromlist=["quote"]).quote(verifier) in body
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_connection_created_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "t", "token_type": "bearer"}}])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                await handle_callback({"code": "c", "state": state}, d)
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE user_id = 'u1' ORDER BY id",
                )
                types = [r["event_type"] for r in rows]
                assert "connection.created" in types
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_computes_token_expires_at_from_expires_in(self, admin_dsn, providers):
        import time
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "a", "refresh_token": "r", "expires_in": 3600}}])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "google"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                before = time.time()
                result = await handle_callback({"code": "c", "state": state}, d)
                after = time.time()
                row = await conn.fetchrow(
                    "SELECT token_expires_at FROM _addition.connections WHERE id = $1",
                    result["connection_id"],
                )
                expires_ts = row["token_expires_at"].timestamp()
                assert expires_ts >= before + 3599
                assert expires_ts <= after + 3601
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_4xx_response_returns_success_false(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                fetch_impl = mk_fetch([{"status": 400, "body": {"error": "invalid_grant", "error_description": "bad code"}}])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "google"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback({"code": "c", "state": state}, d)
                assert result["success"] is False
                assert result["error"] == "invalid_grant"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_post_body_contains_required_fields(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                spy = AsyncMock(return_value=MagicMock(
                    status=200,
                    headers={"content-type": "application/json"},
                    text=AsyncMock(return_value='{"access_token":"t","token_type":"bearer"}'),
                ))
                d = deps(conn, providers, spy)
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, d)
                from urllib.parse import urlparse, parse_qs, quote
                state = parse_qs(urlparse(url).query)["state"][0]
                await handle_callback({"code": "the-code", "state": state}, d)
                call = spy.call_args
                body = call[1].get("data", call[1].get("content", ""))
                assert "grant_type=authorization_code" in body
                assert "code=the-code" in body
                assert "client_id=gh-client" in body
                assert "client_secret=gh-secret" in body
                assert f"redirect_uri={quote(CALLBACK_URL, safe='')}" in body
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_returns_redirect_after_from_state_metadata(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                fetch_impl = mk_fetch([{"status": 200, "body": {"access_token": "t", "token_type": "bearer"}}])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url(
                    {"user_id": "u1", "provider_id": "github", "redirect_after": "/welcome"}, d
                )
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback({"code": "c", "state": state}, d)
                assert result.get("redirect_after") == "/welcome"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_replaying_same_state_returns_invalid_state(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                fetch_impl = mk_fetch([
                    {"status": 200, "body": {"access_token": "t", "token_type": "bearer"}},
                    {"status": 200, "body": {"access_token": "t2", "token_type": "bearer"}},
                ])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                await handle_callback({"code": "c", "state": state}, d)
                result2 = await handle_callback({"code": "c2", "state": state}, d)
                assert result2["success"] is False
                assert result2["error"] == "invalid_state"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_writes_token_exchange_failed_audit_row(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "google", "g-client", "g-secret")
                fetch_impl = mk_fetch([{"status": 400, "body": {"error": "invalid_grant", "error_description": "bad"}}])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "google"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                await handle_callback({"code": "c", "state": state}, d)
                rows = await conn.fetch(
                    "SELECT event_type FROM _addition.audit_log WHERE user_id = 'u1' ORDER BY id",
                )
                types = [r["event_type"] for r in rows]
                assert "oauth.token_exchange_failed" in types
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_missing_code_returns_missing_code(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, deps(conn, providers))
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                spy = AsyncMock()
                result = await handle_callback(
                    {"state": state},
                    deps(conn, providers, spy),
                )
                assert result["success"] is False
                assert result["error"] == "missing_code"
                spy.assert_not_called()
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_error_callback_audit_carries_user_and_provider(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                url = await get_auth_url({"user_id": "u-err", "provider_id": "github"}, deps(conn, providers))
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                await handle_callback(
                    {"error": "access_denied", "state": state},
                    deps(conn, providers),
                )
                rows = await conn.fetch(
                    "SELECT event_type, user_id, provider_id FROM _addition.audit_log "
                    "WHERE user_id = 'u-err' ORDER BY id",
                )
                err_rows = [r for r in rows if r["event_type"] == "oauth.callback_error"]
                assert err_rows
                assert err_rows[0]["provider_id"] == "github"
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)

    async def test_scopes_granted_from_token_response_scope_field(self, admin_dsn, providers):
        dsn, db_name = await _with_fresh_db(admin_dsn)
        try:
            conn = await asyncpg.connect(dsn)
            try:
                await _seed_provider(conn, "github", "gh-client", "gh-secret")
                fetch_impl = mk_fetch([{
                    "status": 200,
                    "body": {"access_token": "t", "token_type": "bearer", "scope": "repo gist"},
                }])
                d = deps(conn, providers, fetch_impl)
                url = await get_auth_url({"user_id": "u1", "provider_id": "github"}, d)
                from urllib.parse import urlparse, parse_qs
                state = parse_qs(urlparse(url).query)["state"][0]
                result = await handle_callback({"code": "c", "state": state}, d)
                row = await conn.fetchrow(
                    "SELECT scopes_granted FROM _addition.connections WHERE id = $1",
                    result["connection_id"],
                )
                assert list(row["scopes_granted"]) == ["repo", "gist"]
            finally:
                await conn.close()
        finally:
            await _drop_db(admin_dsn, db_name)
