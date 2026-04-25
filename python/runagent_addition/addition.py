from __future__ import annotations

import re
from pathlib import Path
from typing import Any, Callable

import asyncpg

from .background import run_refresh_tick, start_background_tasks, BackgroundHandle
from .crypto import encrypt
from .errors import EncryptionError
from .oauth import get_auth_url, handle_callback
from .providers import load_providers
from .proxy import proxy as proxy_fn
from .schema import bootstrap
from .tokens import (
    get_connection_by_id,
    list_connections,
    revoke_connection as revoke_conn,
)
from .toolkits import load_toolkits, get_tool_definitions, get_tool_definition
from .tools import execute_tool as execute_tool_fn

_SHARED_ROOT = Path(__file__).parents[2] / "shared"
_SHARED_PROVIDERS = _SHARED_ROOT / "providers"
_SHARED_TOOLKITS = _SHARED_ROOT / "toolkits"
_SHARED_SCHEMA = _SHARED_ROOT / "schema"

_HEX_64 = re.compile(r"[0-9a-fA-F]{64}")


class Addition:
    def __init__(
        self,
        database_url: str,
        encryption_key: str,
        callback_url: str,
        token_refresh_interval_ms: int = 60_000,
        token_refresh_buffer_ms: int = 300_000,
        fetch: Callable | None = None,
    ) -> None:
        if not _HEX_64.fullmatch(encryption_key):
            raise EncryptionError("encryption_key must be exactly 64 hexadecimal characters (32 bytes)")

        self._database_url = database_url
        self._encryption_key = encryption_key
        self._callback_url = callback_url
        self._token_refresh_interval_ms = token_refresh_interval_ms
        self._token_refresh_buffer_ms = token_refresh_buffer_ms
        self._fetch = fetch
        self._conn: asyncpg.Connection | None = None
        self._ready: bool = False
        self._shutdown: bool = False
        self._bg_handle: BackgroundHandle | None = None
        self._listeners: dict[str, list[Callable]] = {}
        self._providers = load_providers(str(_SHARED_PROVIDERS))
        self._toolkits = load_toolkits(str(_SHARED_TOOLKITS), self._providers)

    def on(self, event: str, handler: Callable) -> None:
        self._listeners.setdefault(event, []).append(handler)

    def emit(self, event: str, payload: Any) -> None:
        for handler in self._listeners.get(event, []):
            handler(payload)

    async def _ensure_ready(self) -> asyncpg.Connection:
        if self._shutdown:
            raise RuntimeError("Addition instance has been shut down")
        if self._conn is None or self._conn.is_closed():
            self._conn = await asyncpg.connect(self._database_url)
        if not self._ready:
            await bootstrap(self._database_url, str(_SHARED_SCHEMA))
            self._ready = True
        return self._conn

    def _deps(self) -> dict:
        return {
            "conn": self._conn,
            "providers": self._providers,
            "encryption_key": self._encryption_key,
            "fetch": self._fetch,
            "callback_url": self._callback_url,
        }

    # ── Provider configuration ─────────────────────────────────────────────────

    async def configure_provider(
        self,
        provider_id: str,
        client_id: str,
        client_secret: str,
        scopes: list[str] | None = None,
    ) -> None:
        conn = await self._ensure_ready()
        secret_enc = encrypt(client_secret, self._encryption_key)
        await conn.execute(
            """INSERT INTO _addition.provider_configs
                 (provider_id, client_id, client_secret_encrypted, scopes)
               VALUES ($1, $2, $3, $4)
               ON CONFLICT (provider_id)
               DO UPDATE SET client_id = EXCLUDED.client_id,
                             client_secret_encrypted = EXCLUDED.client_secret_encrypted,
                             scopes = EXCLUDED.scopes,
                             updated_at = NOW()""",
            provider_id, client_id, secret_enc, scopes or [],
        )

    async def list_configured_providers(self) -> list[dict]:
        conn = await self._ensure_ready()
        rows = await conn.fetch(
            "SELECT provider_id, client_id, scopes FROM _addition.provider_configs ORDER BY provider_id"
        )
        return [
            {"provider_id": r["provider_id"], "client_id": r["client_id"], "scopes": list(r["scopes"])}
            for r in rows
        ]

    # ── OAuth ──────────────────────────────────────────────────────────────────

    async def get_auth_url(
        self,
        user_id: str,
        provider_id: str,
        scopes: list[str] | None = None,
        redirect_after: str | None = None,
        metadata: dict | None = None,
    ) -> str:
        await self._ensure_ready()
        args: dict = {"user_id": user_id, "provider_id": provider_id}
        if scopes is not None:
            args["scopes"] = scopes
        if redirect_after is not None:
            args["redirect_after"] = redirect_after
        if metadata is not None:
            args["metadata"] = metadata
        return await get_auth_url(args, self._deps())

    async def handle_callback(self, query: dict) -> dict:
        await self._ensure_ready()
        result = await handle_callback(query, {
            **self._deps(),
            "callback_url": self._callback_url,
        })
        if result.get("success") and result.get("connection_id"):
            self.emit("connection.created", {
                "connection_id": result["connection_id"],
                "user_id": result.get("user_id"),
                "provider_id": result.get("provider_id"),
            })
        return result

    # ── Connections ────────────────────────────────────────────────────────────

    async def list_connections(self, user_id: str, provider_id: str | None = None) -> list[dict]:
        conn = await self._ensure_ready()
        all_conns = await list_connections(conn, user_id)
        if provider_id is not None:
            return [c for c in all_conns if c["provider_id"] == provider_id]
        return all_conns

    async def get_connection(self, connection_id: str) -> dict | None:
        conn = await self._ensure_ready()
        return await get_connection_by_id(conn, connection_id, self._encryption_key)

    async def revoke_connection(self, connection_id: str) -> None:
        conn = await self._ensure_ready()
        deleted = await revoke_conn(conn, connection_id)
        if deleted:
            self.emit("connection.revoked", {"connection_id": connection_id})

    # ── Proxy ──────────────────────────────────────────────────────────────────

    async def proxy(
        self,
        user_id: str,
        provider_id: str,
        method: str,
        path: str,
        body: Any = None,
        params: dict | None = None,
        headers: dict | None = None,
        connection_id: str | None = None,
    ) -> dict:
        await self._ensure_ready()

        def on_token_refreshed(cid: str) -> None:
            self.emit("token.refreshed", {"connection_id": cid})

        args: dict = {
            "user_id": user_id,
            "provider_id": provider_id,
            "method": method,
            "path": path,
        }
        if body is not None:
            args["body"] = body
        if params is not None:
            args["params"] = params
        if headers is not None:
            args["headers"] = headers
        if connection_id is not None:
            args["connection_id"] = connection_id

        return await proxy_fn(args, {
            **self._deps(),
            "on_token_refreshed": on_token_refreshed,
        })

    # ── Tools ──────────────────────────────────────────────────────────────────

    def get_tool_definitions(self, provider_id: str) -> list[dict]:
        return get_tool_definitions(self._toolkits, provider_id)

    def get_tool_definition(self, provider_id: str, tool_id: str) -> dict:
        return get_tool_definition(self._toolkits, provider_id, tool_id)

    async def execute_tool(
        self,
        user_id: str,
        provider_id: str,
        tool_id: str,
        params: dict,
        connection_id: str | None = None,
    ) -> dict:
        await self._ensure_ready()

        def on_token_refreshed(cid: str) -> None:
            self.emit("token.refreshed", {"connection_id": cid})

        proxy_deps = {**self._deps(), "on_token_refreshed": on_token_refreshed}

        async def _proxy(req: dict) -> dict:
            proxy_args = {
                "user_id": req["user_id"],
                "provider_id": req["provider_id"],
                "method": req["method"],
                "path": req["path"],
            }
            if "body" in req:
                proxy_args["body"] = req["body"]
            if "params" in req:
                proxy_args["params"] = req["params"]
            if "headers" in req:
                proxy_args["headers"] = req["headers"]
            if "connection_id" in req:
                proxy_args["connection_id"] = req["connection_id"]
            r = await proxy_fn(proxy_args, proxy_deps)
            return {"status": r["status"], "body": r["body"]}

        tool_args: dict = {
            "user_id": user_id,
            "provider_id": provider_id,
            "tool_id": tool_id,
            "params": params,
        }
        if connection_id is not None:
            tool_args["connection_id"] = connection_id

        result = await execute_tool_fn(tool_args, {
            "toolkits": self._toolkits,
            "proxy": _proxy,
        })
        self.emit("tool.executed", {
            "user_id": user_id,
            "provider_id": provider_id,
            "tool_id": tool_id,
            "result": result,
        })
        return result

    def list_toolkits(self) -> list[dict]:
        return [
            {"provider": tk["provider"], "name": tk["name"], "tool_count": len(tk["tools"])}
            for tk in self._toolkits.values()
        ]

    # ── Background tasks ───────────────────────────────────────────────────────

    def start_background_tasks(self) -> None:
        if self._bg_handle is not None:
            return
        if self._conn is None:
            raise RuntimeError("call an async method first to open the connection before starting background tasks")
        self._bg_handle = start_background_tasks({
            "conn": self._conn,
            "providers": self._providers,
            "encryption_key": self._encryption_key,
            "fetch": self._fetch,
            "token_refresh_interval_ms": self._token_refresh_interval_ms,
            "token_refresh_buffer_ms": self._token_refresh_buffer_ms,
        })

    def stop_background_tasks(self) -> None:
        if self._bg_handle is not None:
            self._bg_handle.stop()
            self._bg_handle = None

    async def shutdown(self) -> None:
        self._shutdown = True
        if self._bg_handle is not None:
            self._bg_handle.stop()
            self._bg_handle = None
        if self._conn is not None and not self._conn.is_closed():
            await self._conn.close()
