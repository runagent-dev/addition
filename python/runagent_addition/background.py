from __future__ import annotations

import asyncio
from typing import Callable

import asyncpg

from .tokens import refresh_connection


async def run_refresh_tick(deps: dict) -> None:
    conn: asyncpg.Connection = deps["conn"]
    buffer_ms: int = deps["token_refresh_buffer_ms"]

    rows = await conn.fetch(
        """SELECT id FROM _addition.connections
           WHERE status = 'active'
             AND refresh_token_encrypted IS NOT NULL
             AND token_expires_at IS NOT NULL
             AND token_expires_at <= NOW() + ($1 * interval '1 ms')
           ORDER BY token_expires_at ASC
           LIMIT 1000""",
        buffer_ms,
    )

    for row in rows:
        try:
            await refresh_connection(str(row["id"]), deps)
        except Exception:
            pass


class BackgroundHandle:
    def __init__(self, task: asyncio.Task, conn: asyncpg.Connection) -> None:
        self._task = task
        self._conn = conn

    def stop(self) -> None:
        self._task.cancel()

    async def shutdown(self) -> None:
        self._task.cancel()
        try:
            await self._task
        except asyncio.CancelledError:
            pass
        if not self._conn.is_closed():
            await self._conn.close()


def start_background_tasks(deps: dict) -> BackgroundHandle:
    interval_ms: int = deps["token_refresh_interval_ms"]
    tick_fn: Callable = deps.get("_tick_fn") or run_refresh_tick
    conn: asyncpg.Connection = deps["conn"]

    async def _loop() -> None:
        while True:
            await asyncio.sleep(interval_ms / 1000)
            try:
                await tick_fn(deps)
            except Exception:
                pass

    task = asyncio.create_task(_loop())
    return BackgroundHandle(task, conn)
