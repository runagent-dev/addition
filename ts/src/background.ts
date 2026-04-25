import { Pool } from 'pg';
import { refreshConnection } from './tokens.js';
import type { ProviderRegistry } from './types.js';

export interface BackgroundDeps {
  pool: Pool;
  providers: ProviderRegistry;
  encryptionKey: string;
  fetch: typeof fetch;
  tokenRefreshIntervalMs: number;
  tokenRefreshBufferMs: number;
  /** Injectable tick function — override in tests to isolate interval scheduling. */
  _tickFn?: (deps: BackgroundDeps) => Promise<void>;
}

export interface BackgroundHandle {
  stop(): void;
  shutdown(): Promise<void>;
}

export async function runRefreshTick(deps: BackgroundDeps): Promise<void> {
  const { pool, providers, encryptionKey, fetch: fetchFn, tokenRefreshBufferMs } = deps;

  const { rows } = await pool.query<{ id: string }>(
    `SELECT id FROM _addition.connections
     WHERE status = 'active'
       AND refresh_token_encrypted IS NOT NULL
       AND token_expires_at IS NOT NULL
       AND token_expires_at <= NOW() + ($1 * interval '1 ms')`,
    [tokenRefreshBufferMs],
  );

  for (const row of rows) {
    try {
      await refreshConnection(row.id, { pool, providers, encryptionKey, fetch: fetchFn });
    } catch {
      // Failure on one connection must not prevent others from being refreshed.
    }
  }
}

export function startBackgroundTasks(deps: BackgroundDeps): BackgroundHandle {
  const tick = deps._tickFn ?? runRefreshTick;
  const id = setInterval(() => { void tick(deps); }, deps.tokenRefreshIntervalMs);

  return {
    stop() {
      clearInterval(id);
    },
    async shutdown() {
      clearInterval(id);
      await deps.pool.end();
    },
  };
}
