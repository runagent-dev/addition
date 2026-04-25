from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any
from urllib.parse import urlencode, parse_qs

import base64

import asyncpg

from .adapters import get_adapter
from .crypto import encrypt, decrypt
from .errors import ConnectionNotFoundError, ProviderNotFoundError, TokenRefreshError


# ── Helpers ───────────────────────────────────────────────────────────────────


def _get_by_path(obj: Any, path: str) -> Any:
    if not isinstance(obj, dict):
        return None
    cur: Any = obj
    for seg in path.split("."):
        if not isinstance(cur, dict):
            return None
        cur = cur.get(seg)
    return cur


def _expires_at_from_in(expires_in: float) -> datetime:
    return datetime.fromtimestamp(
        datetime.now(tz=timezone.utc).timestamp() + expires_in,
        tz=timezone.utc,
    )


def _decrypt_row(row: asyncpg.Record, key: str) -> dict:
    return {
        "id": str(row["id"]),
        "user_id": row["user_id"],
        "provider_id": row["provider_id"],
        "access_token": decrypt(row["access_token_encrypted"], key),
        "refresh_token": decrypt(row["refresh_token_encrypted"], key) if row["refresh_token_encrypted"] else None,
        "token_expires_at": row["token_expires_at"],
        "scopes_granted": list(row["scopes_granted"]),
        "status": row["status"],
    }


async def _write_audit(
    conn: asyncpg.Connection,
    event_type: str,
    user_id: str | None,
    provider_id: str | None,
    connection_id: str | None,
    detail: dict,
) -> None:
    await conn.execute(
        """INSERT INTO _addition.audit_log
             (event_type, user_id, provider_id, connection_id, detail)
           VALUES ($1, $2, $3, $4, $5)""",
        event_type, user_id, provider_id, connection_id, json.dumps(detail),
    )


async def _load_client_secret(conn: asyncpg.Connection, provider_id: str, key: str) -> dict:
    row = await conn.fetchrow(
        "SELECT client_id, client_secret_encrypted FROM _addition.provider_configs WHERE provider_id = $1",
        provider_id,
    )
    if row is None:
        raise ProviderNotFoundError(provider_id)
    return {
        "client_id": row["client_id"],
        "client_secret": decrypt(row["client_secret_encrypted"], key),
    }


# ── Public API ────────────────────────────────────────────────────────────────


async def store_connection(
    conn: asyncpg.Connection,
    user_id: str,
    provider_id: str,
    tokens: dict,
    key: str,
) -> str:
    access_enc = encrypt(tokens["access_token"], key)
    refresh_enc = encrypt(tokens["refresh_token"], key) if tokens.get("refresh_token") else None
    expires_at = _expires_at_from_in(tokens["expires_in"]) if tokens.get("expires_in") is not None else None

    row = await conn.fetchrow(
        """INSERT INTO _addition.connections
             (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
              token_expires_at, scopes_granted)
           VALUES ($1, $2, $3, $4, $5, $6)
           RETURNING id""",
        user_id, provider_id, access_enc, refresh_enc, expires_at,
        tokens.get("scopes_granted") or [],
    )
    connection_id = str(row["id"])
    await _write_audit(conn, "connection.created", user_id, provider_id, connection_id, {
        "has_refresh_token": refresh_enc is not None,
        "scopes": tokens.get("scopes_granted") or [],
    })
    return connection_id


async def get_connection_by_id(
    conn: asyncpg.Connection,
    connection_id: str,
    key: str,
) -> dict | None:
    row = await conn.fetchrow(
        """SELECT id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
                  token_expires_at, scopes_granted, status
           FROM _addition.connections WHERE id = $1""",
        connection_id,
    )
    if row is None:
        return None
    return _decrypt_row(row, key)


async def find_active_connection(
    conn: asyncpg.Connection,
    user_id: str,
    provider_id: str,
    key: str,
) -> dict | None:
    row = await conn.fetchrow(
        """SELECT id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
                  token_expires_at, scopes_granted, status
           FROM _addition.connections
           WHERE user_id = $1 AND provider_id = $2 AND status = 'active'
           ORDER BY created_at DESC LIMIT 1""",
        user_id, provider_id,
    )
    if row is None:
        return None
    return _decrypt_row(row, key)


async def refresh_connection(connection_id: str, deps: dict) -> dict:
    conn: asyncpg.Connection = deps["conn"]
    providers = deps["providers"]
    key: str = deps["encryption_key"]
    fetch = deps["fetch"]

    row = await conn.fetchrow(
        """SELECT id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
                  token_expires_at, scopes_granted, status
           FROM _addition.connections WHERE id = $1""",
        connection_id,
    )
    if row is None:
        raise ConnectionNotFoundError(connection_id)

    provider_id: str = row["provider_id"]
    user_id: str = row["user_id"]
    provider = providers[provider_id]
    oauth2 = provider["oauth2"]

    # No-op when refresh is unsupported or no token stored
    if not oauth2.get("refresh_supported") or not row["refresh_token_encrypted"]:
        return _decrypt_row(row, key)

    refresh_token = decrypt(row["refresh_token_encrypted"], key)
    creds = await _load_client_secret(conn, provider_id, key)
    adapter = get_adapter(provider_id)

    body: dict[str, str] = {
        "grant_type": "refresh_token",
        "refresh_token": refresh_token,
        "client_id": creds["client_id"],
    }
    if oauth2.get("client_auth_method") == "body":
        body["client_secret"] = creds["client_secret"]

    headers: dict[str, str] = {"Content-Type": "application/x-www-form-urlencoded"}
    if oauth2.get("client_auth_method") == "header":
        cred_str = f"{creds['client_id']}:{creds['client_secret']}"
        basic = base64.b64encode(cred_str.encode()).decode()
        headers["Authorization"] = f"Basic {basic}"

    shaped = adapter.before_token_exchange({
        "url": oauth2["token_url"],
        "method": "POST",
        "headers": headers,
        "body": body,
    })

    resp = await fetch(
        shaped["url"],
        method=shaped["method"],
        headers=shaped["headers"],
        data=urlencode(shaped["body"]),
    )

    text = await resp.text()
    if oauth2.get("token_response_format", "json") == "json":
        try:
            parsed: Any = json.loads(text)
        except Exception:
            parsed = text
    else:
        params = parse_qs(text, keep_blank_values=True)
        parsed = {k: v[0] for k, v in params.items()}

    if adapter.is_error_response(resp.status, parsed):
        detail = adapter.extract_error_detail(parsed)
        await conn.execute(
            "UPDATE _addition.connections SET status = 'refresh_failed', status_detail = $2, updated_at = NOW() WHERE id = $1",
            connection_id, detail["code"],
        )
        await _write_audit(conn, "token.refresh_failed", user_id, provider_id, connection_id, {
            "code": detail["code"],
            "description": detail.get("description"),
        })
        raise TokenRefreshError(detail["code"], detail.get("description"))

    mapping = (oauth2.get("token_response_mapping") or {})
    access_path = mapping.get("access_token", "access_token")
    refresh_path = mapping.get("refresh_token", "refresh_token")
    expires_path = mapping.get("expires_in", "expires_in")

    new_access = _get_by_path(parsed, access_path) if isinstance(parsed, dict) else None
    if not isinstance(new_access, str) or not new_access:
        await conn.execute(
            "UPDATE _addition.connections SET status = 'refresh_failed', status_detail = $2, updated_at = NOW() WHERE id = $1",
            connection_id, "malformed_token_response",
        )
        await _write_audit(conn, "token.refresh_failed", user_id, provider_id, connection_id, {
            "code": "malformed_token_response",
        })
        raise TokenRefreshError("malformed_token_response")

    new_refresh = _get_by_path(parsed, refresh_path) if isinstance(parsed, dict) else None
    new_expires_in = _get_by_path(parsed, expires_path) if isinstance(parsed, dict) else None

    new_access_enc = encrypt(new_access, key)
    # Keep existing refresh token when provider doesn't return a new one
    new_refresh_enc = (
        encrypt(new_refresh, key)
        if isinstance(new_refresh, str) and new_refresh
        else row["refresh_token_encrypted"]
    )
    new_expires_at = _expires_at_from_in(float(new_expires_in)) if isinstance(new_expires_in, (int, float)) else None

    updated = await conn.fetchrow(
        """UPDATE _addition.connections
             SET access_token_encrypted = $2,
                 refresh_token_encrypted = $3,
                 token_expires_at = COALESCE($4, token_expires_at),
                 status = 'active',
                 status_detail = NULL,
                 updated_at = NOW()
           WHERE id = $1
           RETURNING id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
                     token_expires_at, scopes_granted, status""",
        connection_id, new_access_enc, new_refresh_enc, new_expires_at,
    )

    await _write_audit(conn, "token.refresh_succeeded", user_id, provider_id, connection_id, {
        "has_new_refresh_token": isinstance(new_refresh, str) and bool(new_refresh),
    })

    return _decrypt_row(updated, key)


async def list_connections(conn: asyncpg.Connection, user_id: str) -> list[dict]:
    rows = await conn.fetch(
        """SELECT id, provider_id, scopes_granted, status, created_at
           FROM _addition.connections
           WHERE user_id = $1
           ORDER BY created_at DESC""",
        user_id,
    )
    return [
        {
            "id": str(r["id"]),
            "provider_id": r["provider_id"],
            "scopes_granted": list(r["scopes_granted"]),
            "status": r["status"],
            "created_at": r["created_at"],
        }
        for r in rows
    ]


async def revoke_connection(conn: asyncpg.Connection, connection_id: str) -> bool:
    row = await conn.fetchrow(
        "DELETE FROM _addition.connections WHERE id = $1 RETURNING user_id, provider_id",
        connection_id,
    )
    if row is not None:
        await _write_audit(conn, "connection.revoked", row["user_id"], row["provider_id"], connection_id, {})
        return True
    return False
