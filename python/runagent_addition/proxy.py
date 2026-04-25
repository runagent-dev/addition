from __future__ import annotations

import asyncio
import json
from typing import Any
from urllib.parse import urlencode

import asyncpg

from .adapters import get_adapter
from .errors import (
    ConnectionExpiredError,
    ConnectionNotFoundError,
    ProxyError,
    ProviderNotFoundError,
)
from .tokens import find_active_connection, get_connection_by_id, refresh_connection

MAX_429_RETRIES = 3


async def proxy(args: dict, deps: dict) -> dict:
    conn: asyncpg.Connection = deps["conn"]
    providers = deps["providers"]
    key: str = deps["encryption_key"]
    fetch = deps["fetch"]
    sleep = deps.get("sleep") or _default_sleep

    user_id: str = args["user_id"]
    provider_id: str = args["provider_id"]
    method: str = args["method"]
    path: str = args["path"]
    body: Any = args.get("body")
    params: dict | None = args.get("params")
    caller_headers: dict = dict(args.get("headers") or {})
    connection_id: str | None = args.get("connection_id")

    provider = providers.get(provider_id)
    if not provider:
        raise ProviderNotFoundError(provider_id)

    adapter = get_adapter(provider_id)

    if connection_id:
        record = await get_connection_by_id(conn, connection_id, key)
    else:
        record = await find_active_connection(conn, user_id, provider_id, key)

    if not record or (connection_id and record["user_id"] != user_id):
        raise ConnectionNotFoundError(connection_id or f"{user_id}/{provider_id}")

    await conn.execute(
        "INSERT INTO _addition.audit_log (event_type, user_id, provider_id, connection_id, detail) "
        "VALUES ($1, $2, $3, $4, $5)",
        "proxy.called", user_id, provider_id, record["id"],
        json.dumps({"method": method, "path": path}),
    )

    base = provider["api"]["base_url"].rstrip("/")
    url = base + path
    if params:
        url = url + ("&" if "?" in url else "?") + urlencode({k: str(v) for k, v in params.items()})

    def make_headers(access_token: str) -> dict:
        h = {
            **caller_headers,
            provider["api"]["auth_header"]: f"{provider['api']['auth_prefix']} {access_token}",
        }
        if body is not None:
            h["Content-Type"] = "application/json"
        return h

    network_retried = False

    async def execute(access_token: str) -> tuple:
        nonlocal network_retried
        headers = make_headers(access_token)
        kwargs: dict = {"method": method, "headers": headers}
        if body is not None:
            kwargs["content"] = json.dumps(body)
        try:
            resp = await fetch(url, **kwargs)
        except Exception as exc:
            if not network_retried:
                network_retried = True
                await sleep(1000)
                resp = await fetch(url, **kwargs)
            else:
                raise
        text = await resp.text()
        try:
            parsed = json.loads(text)
        except Exception:
            parsed = text
        return resp, parsed

    retries_429 = 0

    while True:
        resp, parsed = await execute(record["access_token"])

        if resp.status == 401:
            if not record["refresh_token"]:
                raise ConnectionExpiredError(record["id"])
            record = await refresh_connection(record["id"], deps)
            resp, parsed = await execute(record["access_token"])

        if resp.status == 429 and retries_429 < MAX_429_RETRIES:
            retries_429 += 1
            retry_after = int(resp.headers.get("Retry-After", "1"))
            retry_after = min(retry_after, 60)
            await sleep(retry_after * 1000)
            continue

        if resp.status != 429 and adapter.is_error_response(resp.status, parsed):
            detail = adapter.extract_error_detail(parsed)
            raise ProxyError(resp.status, detail["code"], detail.get("description"))

        return {"status": resp.status, "headers": dict(resp.headers), "body": parsed}


async def _default_sleep(ms: int) -> None:
    await asyncio.sleep(ms / 1000)
