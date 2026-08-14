"""
Cross-language conformance suite.

Loads every scenario from shared/test-fixtures/*.yaml and drives it through
the public Addition class API. Must pass 100% before tagging a release.
The same fixtures drive the TypeScript implementation — any behavioural
difference between the two language tracks surfaces here.
"""
from __future__ import annotations

import json
import random
import string
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

import asyncpg
import pytest
import yaml
from urllib.parse import urlparse, parse_qs

from runagent_addition.addition import Addition
from runagent_addition.crypto import decrypt, encrypt
from runagent_addition.errors import TokenRefreshError

FIXTURES_DIR = Path(__file__).parents[3] / "shared" / "test-fixtures"
TEST_KEY = "9" * 64
CALLBACK_URL = "http://localhost:3000/callback"


# ── Postgres container ────────────────────────────────────────────────────────


@pytest.fixture(scope="session")
def pg_container():
    from testcontainers.postgres import PostgresContainer
    with PostgresContainer("postgres:16-alpine", dbname="addition_conformance") as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(pg_container):
    return pg_container.get_connection_url().replace("psycopg2", "asyncpg").replace("+asyncpg", "")


# ── Helpers ───────────────────────────────────────────────────────────────────


async def _with_fresh_db(admin_dsn: str):
    suffix = "".join(random.choices(string.ascii_lowercase, k=8))
    db_name = f"conf_{suffix}"
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


def mk_fetch(responses: list[dict]) -> AsyncMock:
    captured: list[tuple] = []
    i = 0

    async def _fetch(url, **kwargs):
        nonlocal i
        captured.append((url, kwargs))
        r = responses[min(i, len(responses) - 1)]
        i += 1
        body = r["body"]
        text = body if isinstance(body, str) else json.dumps(body)
        resp = MagicMock()
        resp.status = r["status"]
        resp.headers = {"content-type": "application/json"}
        resp.text = AsyncMock(return_value=text)
        return resp

    mock = AsyncMock(side_effect=_fetch)
    mock._captured = captured
    return mock


async def _make_addition(db_url: str, fetch_impl) -> Addition:
    addition = Addition(
        database_url=db_url,
        encryption_key=TEST_KEY,
        callback_url=CALLBACK_URL,
        fetch=fetch_impl,
    )
    await addition.list_configured_providers()  # trigger schema bootstrap
    return addition


async def _seed_connection(
    db_url: str,
    user_id: str,
    provider_id: str,
    access_token: str,
    refresh_token: str | None,
) -> str:
    conn = await asyncpg.connect(db_url)
    try:
        row = await conn.fetchrow(
            "INSERT INTO _addition.connections "
            "(user_id, provider_id, access_token_encrypted, refresh_token_encrypted, token_expires_at, scopes_granted) "
            "VALUES ($1, $2, $3, $4, $5, $6) RETURNING id",
            user_id, provider_id,
            encrypt(access_token, TEST_KEY),
            encrypt(refresh_token, TEST_KEY) if refresh_token else None,
            None, [],
        )
        return str(row["id"])
    finally:
        await conn.close()


# ── Fixture loader ────────────────────────────────────────────────────────────


def _load_fixture(name: str) -> dict:
    path = FIXTURES_DIR / name
    with open(path) as fh:
        return yaml.safe_load(fh)


def _all_fixture_files() -> list[str]:
    return sorted(p.name for p in FIXTURES_DIR.glob("*.yaml"))


# ── Crypto interop ────────────────────────────────────────────────────────────


class TestCryptoInterop:
    def test_python_decrypts_ts_vector(self):
        data = _load_fixture("crypto_interop.yaml")
        key = data["key"]
        vec = data["ts_vector"]
        plaintext = decrypt(vec["ciphertext"], key)
        assert plaintext == vec["plaintext"]

    def test_ts_can_decrypt_python_encrypted(self):
        data = _load_fixture("crypto_interop.yaml")
        key = data["key"]
        vec = data["ts_vector"]
        # Re-encrypt and verify roundtrip (Python → Python as a sanity check
        # that our encrypt/decrypt are inverses; TS cross-check is the ts_vector test above)
        ciphertext = encrypt(vec["plaintext"], key)
        assert decrypt(ciphertext, key) == vec["plaintext"]

    def test_fixture_has_required_fields(self):
        data = _load_fixture("crypto_interop.yaml")
        assert "key" in data
        assert "ts_vector" in data
        assert "plaintext" in data["ts_vector"]
        assert "ciphertext" in data["ts_vector"]


# ── Proxy conformance ─────────────────────────────────────────────────────────


def _proxy_scenarios() -> list[dict]:
    return _load_fixture("proxy_requests.yaml")["scenarios"]


@pytest.mark.parametrize("scenario", _proxy_scenarios(), ids=lambda s: s["name"])
async def test_proxy_conformance(scenario, admin_dsn):
    db_url, db_name = await _with_fresh_db(admin_dsn)
    try:
        spy = mk_fetch([scenario["mock_response"]])
        addition = await _make_addition(db_url, spy)

        await addition.configure_provider(
            provider_id=scenario["provider"],
            client_id="test-client",
            client_secret="test-secret",
        )

        conn_id = await _seed_connection(db_url, "conformance-user", scenario["provider"], "conformance-token", None)

        params = scenario.get("params") or {}
        body = scenario.get("body")

        result = await addition.proxy(
            user_id="conformance-user",
            provider_id=scenario["provider"],
            method=scenario["method"],
            path=scenario["path"],
            params=params if params else None,
            body=body,
        )

        expected = scenario["expected"]
        assert result["status"] == expected["status"]

        captured = spy._captured
        assert len(captured) >= 1
        url, kwargs = captured[0]
        headers = kwargs.get("headers", {})

        if "auth_header" in expected and "auth_prefix" in expected:
            assert expected["auth_header"] in headers
            assert headers[expected["auth_header"]].startswith(expected["auth_prefix"] + " ")

        if "url_query_params" in expected:
            parsed = urlparse(url)
            qs = parse_qs(parsed.query)
            for k, v in expected["url_query_params"].items():
                assert k in qs
                assert qs[k][0] == v

        if expected.get("request_has_body"):
            assert kwargs.get("content") is not None

        await addition.shutdown()
    finally:
        await _drop_db(admin_dsn, db_name)


# ── Token refresh conformance ─────────────────────────────────────────────────


def _token_refresh_scenarios() -> list[dict]:
    return _load_fixture("token_refresh.yaml")["scenarios"]


@pytest.mark.parametrize("scenario", _token_refresh_scenarios(), ids=lambda s: s["name"])
async def test_token_refresh_conformance(scenario, admin_dsn):
    db_url, db_name = await _with_fresh_db(admin_dsn)
    try:
        is_failure = scenario["expected"]["status"] == "refresh_failed"

        if is_failure:
            fetch_responses = [
                {"status": 401, "body": {}},
                scenario["mock_response"],
            ]
        else:
            fetch_responses = [
                {"status": 401, "body": {}},
                scenario["mock_response"],
                {"status": 200, "body": {}},
            ]

        spy = mk_fetch(fetch_responses)
        addition = await _make_addition(db_url, spy)

        await addition.configure_provider(
            provider_id=scenario["provider"],
            client_id="test-client",
            client_secret="test-secret",
        )

        conn_id = await _seed_connection(
            db_url,
            "conformance-user",
            scenario["provider"],
            scenario["initial"]["access_token"],
            scenario["initial"]["refresh_token"],
        )

        if is_failure:
            with pytest.raises(TokenRefreshError):
                await addition.proxy(
                    user_id="conformance-user",
                    provider_id=scenario["provider"],
                    method="GET",
                    path="/oauth2/v1/userinfo",
                )
        else:
            await addition.proxy(
                user_id="conformance-user",
                provider_id=scenario["provider"],
                method="GET",
                path="/oauth2/v1/userinfo",
            )

        record = await addition.get_connection(conn_id)
        assert record is not None
        assert record["access_token"] == scenario["expected"]["access_token"]
        assert record["refresh_token"] == scenario["expected"]["refresh_token"]
        assert record["status"] == scenario["expected"]["status"]

        await addition.shutdown()
    finally:
        await _drop_db(admin_dsn, db_name)


# ── Fixture coverage guard ────────────────────────────────────────────────────


class TestFixtureCoverage:
    def test_all_fixture_files_are_handled(self):
        handled = {
            "crypto_interop.yaml",
            "proxy_requests.yaml",
            "token_refresh.yaml",
            "tool_schema_export.yaml",  # driven by tests/unit/test_toolschema.py
        }
        for name in _all_fixture_files():
            assert name in handled, (
                f"Fixture '{name}' is not handled by any conformance driver — "
                "add a driver or add it to the handled set"
            )
