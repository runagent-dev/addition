from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
from datetime import datetime, timezone
from typing import Any
from urllib.parse import urlencode, quote

import asyncpg

from .adapters import get_adapter
from .crypto import encrypt, decrypt
from .errors import OAuthError, ProviderDisabledError, ProviderNotFoundError
from .providers import get_provider


# ── Utilities ─────────────────────────────────────────────────────────────────


def _get_by_path(obj: Any, path: str) -> Any:
    if not isinstance(obj, dict):
        return None
    cur: Any = obj
    for seg in path.split("."):
        if not isinstance(cur, dict):
            return None
        cur = cur.get(seg)
    return cur


def _gen_code_verifier() -> str:
    return base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()


def _code_challenge(verifier: str) -> str:
    digest = hashlib.sha256(verifier.encode()).digest()
    return base64.urlsafe_b64encode(digest).rstrip(b"=").decode()


def _join_scopes(scopes: list[str], delimiter: str) -> str:
    sep = "," if delimiter == "comma" else " "
    return sep.join(scopes)


# ── DB helpers ────────────────────────────────────────────────────────────────


async def _load_credentials(conn: asyncpg.Connection, provider_id: str, key: str) -> dict:
    row = await conn.fetchrow(
        "SELECT client_id, client_secret_encrypted, enabled "
        "FROM _addition.provider_configs WHERE provider_id = $1",
        provider_id,
    )
    if row is None:
        raise ProviderNotFoundError(provider_id)
    if not row["enabled"]:
        raise ProviderDisabledError(provider_id)
    return {
        "client_id": row["client_id"],
        "client_secret": decrypt(row["client_secret_encrypted"], key),
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
        event_type,
        user_id,
        provider_id,
        connection_id,
        json.dumps(detail),
    )


async def _consume_state(conn: asyncpg.Connection, state: str) -> dict | None:
    row = await conn.fetchrow(
        """DELETE FROM _addition.oauth_state
             WHERE state = $1 AND expires_at > NOW()
           RETURNING user_id, provider_id, code_verifier, redirect_uri, scopes_requested, metadata""",
        state,
    )
    if row is None:
        return None
    return {
        "user_id": row["user_id"],
        "provider_id": row["provider_id"],
        "code_verifier": row["code_verifier"],
        "redirect_uri": row["redirect_uri"],
        "scopes_requested": list(row["scopes_requested"]),
        "metadata": json.loads(row["metadata"]) if isinstance(row["metadata"], str) else (row["metadata"] or {}),
    }


async def _delete_state_returning(conn: asyncpg.Connection, state: str) -> dict:
    row = await conn.fetchrow(
        "DELETE FROM _addition.oauth_state WHERE state = $1 RETURNING user_id, provider_id",
        state,
    )
    if row is None:
        return {"user_id": None, "provider_id": None}
    return {"user_id": row["user_id"], "provider_id": row["provider_id"]}


# ── Token extraction ──────────────────────────────────────────────────────────


def _extract_tokens(provider: dict, body: Any) -> dict:
    mapping = (provider.get("oauth2") or {}).get("token_response_mapping") or {}
    access_path = mapping.get("access_token", "access_token")
    refresh_path = mapping.get("refresh_token", "refresh_token")
    expires_path = mapping.get("expires_in", "expires_in")

    access = _get_by_path(body, access_path) if isinstance(body, dict) else None
    if not isinstance(access, str) or not access:
        raise OAuthError(
            "malformed_token_response",
            f"access_token not found at '{access_path}'",
        )

    refresh = _get_by_path(body, refresh_path) if isinstance(body, dict) else None
    expires = _get_by_path(body, expires_path) if isinstance(body, dict) else None

    return {
        "access_token": access,
        "refresh_token": refresh if isinstance(refresh, str) and refresh else None,
        "expires_in": expires if isinstance(expires, (int, float)) else None,
    }


def _extract_granted_scopes(provider: dict, body: Any, requested: list[str]) -> list[str]:
    scope_field = _get_by_path(body, "scope") if isinstance(body, dict) else None
    if isinstance(scope_field, str) and scope_field:
        delimiter = "," if (provider.get("oauth2") or {}).get("scopes_delimiter") == "comma" else " "
        return [s for s in scope_field.split(delimiter) if s]
    return requested


# ── HTTP helpers ──────────────────────────────────────────────────────────────


async def _parse_response(resp: Any, fmt: str) -> Any:
    text = await resp.text()
    if fmt == "json":
        try:
            return json.loads(text)
        except Exception:
            return text
    # form-encoded
    from urllib.parse import parse_qs
    params = parse_qs(text, keep_blank_values=True)
    return {k: v[0] for k, v in params.items()}


# ── Public API ────────────────────────────────────────────────────────────────


async def get_auth_url(args: dict, deps: dict) -> str:
    conn: asyncpg.Connection = deps["conn"]
    providers = deps["providers"]
    key: str = deps["encryption_key"]
    callback_url: str = deps["callback_url"]

    provider_id = args["provider_id"]
    user_id = args["user_id"]

    try:
        provider = get_provider(providers, provider_id)
        creds = await _load_credentials(conn, provider_id, key)

        oauth2 = provider["oauth2"]
        scopes = args.get("scopes") or oauth2["default_scopes"]
        state = secrets.token_hex(32)

        use_pkce = oauth2.get("pkce_supported") or oauth2.get("pkce_required")
        verifier = _gen_code_verifier() if use_pkce else None

        params: dict[str, str] = {
            "client_id": creds["client_id"],
            "redirect_uri": callback_url,
            "response_type": "code",
            "state": state,
            "scope": _join_scopes(scopes, oauth2.get("scopes_delimiter", "space")),
        }
        if verifier:
            params["code_challenge"] = _code_challenge(verifier)
            params["code_challenge_method"] = "S256"
        for k, v in (oauth2.get("authorize_params") or {}).items():
            params[k] = str(v)

        metadata: dict = dict(args.get("metadata") or {})
        if args.get("redirect_after") is not None:
            metadata["redirect_after"] = args["redirect_after"]

        await conn.execute(
            """INSERT INTO _addition.oauth_state
                 (state, user_id, provider_id, code_verifier, redirect_uri, scopes_requested, metadata)
               VALUES ($1, $2, $3, $4, $5, $6, $7)""",
            state, user_id, provider_id, verifier, callback_url, scopes, json.dumps(metadata),
        )

        await _write_audit(conn, "oauth.authorize_url_issued", user_id, provider_id, None, {
            "scopes": scopes,
            "pkce": verifier is not None,
        })

        return f"{oauth2['authorize_url']}?{urlencode(params)}"

    except Exception as exc:
        await _write_audit(conn, "oauth.authorize_url_failed", user_id, provider_id, None, {
            "error": str(exc),
        })
        raise


async def handle_callback(query: dict, deps: dict) -> dict:
    conn: asyncpg.Connection = deps["conn"]
    providers = deps["providers"]
    key: str = deps["encryption_key"]
    fetch = deps["fetch"]

    state = query.get("state")

    # Provider-side error
    if query.get("error"):
        stored_attr: dict = {"user_id": None, "provider_id": None}
        if state:
            stored_attr = await _delete_state_returning(conn, state)
        await _write_audit(conn, "oauth.callback_error", stored_attr["user_id"], stored_attr["provider_id"], None, {
            "error": query["error"],
            "error_description": query.get("error_description"),
        })
        return {
            "success": False,
            "error": query["error"],
            "error_description": query.get("error_description"),
        }

    if not state:
        await _write_audit(conn, "oauth.invalid_state", None, None, None, {"state": None})
        return {"success": False, "error": "invalid_state"}

    stored = await _consume_state(conn, state)
    if not stored:
        await _write_audit(conn, "oauth.invalid_state", None, None, None, {"state": state})
        return {"success": False, "error": "invalid_state"}

    if not query.get("code"):
        await _write_audit(conn, "oauth.missing_code", stored["user_id"], stored["provider_id"], None, {})
        return {"success": False, "error": "missing_code"}

    provider = get_provider(providers, stored["provider_id"])
    adapter = get_adapter(stored["provider_id"])
    creds = await _load_credentials(conn, stored["provider_id"], key)
    oauth2 = provider["oauth2"]

    body_params: dict[str, str] = {
        "grant_type": "authorization_code",
        "code": query["code"],
        "redirect_uri": stored["redirect_uri"],
        "client_id": creds["client_id"],
    }
    if oauth2.get("client_auth_method") == "body":
        body_params["client_secret"] = creds["client_secret"]
    if stored["code_verifier"]:
        body_params["code_verifier"] = stored["code_verifier"]
    for k, v in (oauth2.get("token_params") or {}).items():
        body_params[k] = str(v)

    base_headers: dict[str, str] = {"Content-Type": "application/x-www-form-urlencoded"}
    if oauth2.get("client_auth_method") == "header":
        import base64 as _b64
        cred_str = f"{creds['client_id']}:{creds['client_secret']}"
        basic = _b64.b64encode(cred_str.encode()).decode()
        base_headers["Authorization"] = f"Basic {basic}"

    token_req = {
        "url": oauth2["token_url"],
        "method": "POST",
        "headers": base_headers,
        "body": body_params,
    }
    final_req = adapter.before_token_exchange(token_req)
    form_body = urlencode(final_req["body"])

    resp = await fetch(
        final_req["url"],
        method=final_req["method"],
        headers=final_req["headers"],
        data=form_body,
    )

    parsed = await _parse_response(resp, oauth2.get("token_response_format", "json"))

    if adapter.is_error_response(resp.status, parsed):
        detail = adapter.extract_error_detail(parsed)
        await _write_audit(conn, "oauth.token_exchange_failed", stored["user_id"], stored["provider_id"], None, {
            "code": detail["code"],
            "description": detail.get("description"),
            "status": resp.status,
        })
        return {"success": False, "error": detail["code"], "error_description": detail.get("description")}

    try:
        tokens = _extract_tokens(provider, parsed)
    except OAuthError as exc:
        await _write_audit(conn, "oauth.token_exchange_failed", stored["user_id"], stored["provider_id"], None, {
            "code": exc.code,
        })
        return {"success": False, "error": exc.code, "error_description": exc.description}

    access_enc = encrypt(tokens["access_token"], key)
    refresh_enc = encrypt(tokens["refresh_token"], key) if tokens["refresh_token"] else None
    raw_enc = encrypt(json.dumps(parsed), key)
    expires_at: datetime | None = None
    if tokens["expires_in"] is not None:
        expires_at = datetime.fromtimestamp(
            datetime.now(tz=timezone.utc).timestamp() + float(tokens["expires_in"]),
            tz=timezone.utc,
        )
    scopes_granted = _extract_granted_scopes(provider, parsed, stored["scopes_requested"])

    row = await conn.fetchrow(
        """INSERT INTO _addition.connections
             (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
              token_expires_at, raw_token_response_encrypted, scopes_granted)
           VALUES ($1, $2, $3, $4, $5, $6, $7)
           RETURNING id""",
        stored["user_id"],
        stored["provider_id"],
        access_enc,
        refresh_enc,
        expires_at,
        raw_enc,
        scopes_granted,
    )
    connection_id = str(row["id"])

    await _write_audit(conn, "connection.created", stored["user_id"], stored["provider_id"], connection_id, {
        "pkce": stored["code_verifier"] is not None,
        "has_refresh_token": refresh_enc is not None,
    })

    result: dict = {
        "success": True,
        "connection_id": connection_id,
        "user_id": stored["user_id"],
        "provider_id": stored["provider_id"],
    }
    redirect_after = stored["metadata"].get("redirect_after")
    if isinstance(redirect_after, str):
        result["redirect_after"] = redirect_after
    return result
