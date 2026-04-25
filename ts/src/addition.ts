import { EventEmitter } from 'node:events';
import { join } from 'node:path';
import { Pool } from 'pg';
import { bootstrap } from './schema.js';
import { loadProviders } from './providers.js';
import { loadToolkits, getToolDefinitions, getToolDefinition } from './toolkits.js';
import { encrypt } from './crypto.js';
import { getAuthUrl, handleCallback } from './oauth.js';
import {
  listConnections,
  getConnectionById,
  revokeConnection as revokeConn,
} from './tokens.js';
import { proxy as proxyFn } from './proxy.js';
import { executeTool as executeToolFn } from './tools.js';
import { startBackgroundTasks } from './background.js';
import { EncryptionError } from './errors.js';
import type { ProviderRegistry, ToolkitRegistry, ToolDefinition } from './types.js';
import type { CallbackResult } from './oauth.js';
import type { ConnectionRecord } from './tokens.js';
import type { ExecuteToolArgs, ToolExecutionResult } from './tools.js';
import type { ProxyArgs, ProxyResponse } from './proxy.js';
import type { BackgroundHandle } from './background.js';

const SHARED_ROOT = join(__dirname, '..', '..', 'shared');
const SHARED_PROVIDERS = join(SHARED_ROOT, 'providers');
const SHARED_TOOLKITS = join(SHARED_ROOT, 'toolkits');
const SHARED_SCHEMA = join(SHARED_ROOT, 'schema');

const HEX_64 = /^[0-9a-fA-F]{64}$/;

export interface AdditionOptions {
  databaseUrl: string;
  encryptionKey: string;
  callbackUrl: string;
  tokenRefreshIntervalMs?: number;
  tokenRefreshBufferMs?: number;
  /** Injectable fetch — used by tests to mock HTTP. Defaults to globalThis.fetch. */
  fetch?: typeof fetch;
}

export class Addition extends EventEmitter {
  private readonly pool: Pool;
  private readonly providers: ProviderRegistry;
  private readonly toolkits: ToolkitRegistry;
  private readonly encryptionKey: string;
  private readonly callbackUrl: string;
  private readonly tokenRefreshIntervalMs: number;
  private readonly tokenRefreshBufferMs: number;
  private readonly fetchFn: typeof fetch;
  private readonly _ready: Promise<void>;
  private bgHandle: BackgroundHandle | null = null;

  constructor(options: AdditionOptions) {
    super();

    if (!HEX_64.test(options.encryptionKey)) {
      throw new EncryptionError('encryptionKey must be exactly 64 hexadecimal characters (32 bytes)');
    }

    this.encryptionKey = options.encryptionKey;
    this.callbackUrl = options.callbackUrl;
    this.tokenRefreshIntervalMs = options.tokenRefreshIntervalMs ?? 60_000;
    this.tokenRefreshBufferMs = options.tokenRefreshBufferMs ?? 300_000;
    this.fetchFn = options.fetch ?? globalThis.fetch.bind(globalThis);
    this.pool = new Pool({ connectionString: options.databaseUrl });
    this.providers = loadProviders(SHARED_PROVIDERS);
    this.toolkits = loadToolkits(SHARED_TOOLKITS, this.providers);
    // Bootstrap schema lazily; all async methods await this before proceeding.
    this._ready = bootstrap(options.databaseUrl, SHARED_SCHEMA);
  }

  // ── Provider configuration ──────────────────────────────────────────────────

  async configureProvider(params: {
    providerId: string;
    clientId: string;
    clientSecret: string;
    scopes?: string[];
  }): Promise<void> {
    await this._ready;
    const { providerId, clientId, clientSecret, scopes } = params;
    const secretEnc = encrypt(clientSecret, this.encryptionKey);
    await this.pool.query(
      `INSERT INTO _addition.provider_configs
         (provider_id, client_id, client_secret_encrypted, scopes)
       VALUES ($1, $2, $3, $4)
       ON CONFLICT (provider_id)
       DO UPDATE SET client_id = EXCLUDED.client_id,
                     client_secret_encrypted = EXCLUDED.client_secret_encrypted,
                     scopes = EXCLUDED.scopes,
                     updated_at = NOW()`,
      [providerId, clientId, secretEnc, scopes ?? []],
    );
  }

  async listConfiguredProviders(): Promise<Array<{ providerId: string; clientId: string; scopes: string[] }>> {
    await this._ready;
    const { rows } = await this.pool.query(
      `SELECT provider_id, client_id, scopes FROM _addition.provider_configs ORDER BY provider_id`,
    );
    return rows.map((r) => ({
      providerId: r.provider_id as string,
      clientId: r.client_id as string,
      scopes: r.scopes as string[],
    }));
  }

  // ── OAuth ───────────────────────────────────────────────────────────────────

  async getAuthUrl(params: {
    userId: string;
    providerId: string;
    scopes?: string[];
    redirectAfter?: string;
    metadata?: Record<string, unknown>;
  }): Promise<string> {
    await this._ready;
    return getAuthUrl(params, {
      pool: this.pool,
      providers: this.providers,
      encryptionKey: this.encryptionKey,
      callbackUrl: this.callbackUrl,
      fetch: this.fetchFn,
    });
  }

  async handleCallback(query: Record<string, string>): Promise<CallbackResult> {
    await this._ready;
    const result = await handleCallback(query, {
      pool: this.pool,
      providers: this.providers,
      encryptionKey: this.encryptionKey,
      callbackUrl: this.callbackUrl,
      fetch: this.fetchFn,
    });
    if (result.success && result.connectionId) {
      this.emit('connection.created', {
        connectionId: result.connectionId,
        userId: result.userId,
        providerId: result.providerId,
      });
    }
    return result;
  }

  // ── Connections ─────────────────────────────────────────────────────────────

  async listConnections(params: { userId: string; providerId?: string }): Promise<Array<{ id: string; providerId: string; scopesGranted: string[]; status: string; createdAt: Date }>> {
    await this._ready;
    const all = await listConnections(this.pool, params.userId);
    if (params.providerId) return all.filter((c) => c.providerId === params.providerId);
    return all;
  }

  async getConnection(connectionId: string): Promise<ConnectionRecord | null> {
    await this._ready;
    return getConnectionById(this.pool, connectionId, this.encryptionKey);
  }

  async revokeConnection(connectionId: string): Promise<void> {
    await this._ready;
    const deleted = await revokeConn(this.pool, connectionId);
    if (deleted) this.emit('connection.revoked', { connectionId });
  }

  // ── Proxy ───────────────────────────────────────────────────────────────────

  async proxy(args: ProxyArgs): Promise<ProxyResponse> {
    await this._ready;
    return proxyFn(args, {
      pool: this.pool,
      providers: this.providers,
      encryptionKey: this.encryptionKey,
      fetch: this.fetchFn,
      onTokenRefreshed: (connectionId) => {
        this.emit('token.refreshed', { connectionId });
      },
    });
  }

  // ── Tools ───────────────────────────────────────────────────────────────────

  getToolDefinitions(providerId: string): ToolDefinition[] {
    return getToolDefinitions(this.toolkits, providerId);
  }

  getToolDefinition(providerId: string, toolId: string): ToolDefinition {
    return getToolDefinition(this.toolkits, providerId, toolId);
  }

  async executeTool(args: ExecuteToolArgs): Promise<ToolExecutionResult> {
    await this._ready;
    const result = await executeToolFn(args, {
      toolkits: this.toolkits,
      proxy: (req) => proxyFn(
        { ...req, params: req.params as Record<string, string | number | boolean> },
        {
          pool: this.pool,
          providers: this.providers,
          encryptionKey: this.encryptionKey,
          fetch: this.fetchFn,
          onTokenRefreshed: (connectionId) => {
            this.emit('token.refreshed', { connectionId });
          },
        },
      ),
    });
    this.emit('tool.executed', { userId: args.userId, providerId: args.providerId, toolId: args.toolId, result });
    return result;
  }

  listToolkits(): Array<{ provider: string; name: string; toolCount: number }> {
    return Object.values(this.toolkits).map((tk) => ({
      provider: tk.provider,
      name: tk.name,
      toolCount: tk.tools.length,
    }));
  }

  // ── Background tasks ────────────────────────────────────────────────────────

  /**
   * Starts the background token-refresh loop.
   * Call this after at least one awaited method (e.g. `configureProvider`) to
   * ensure the schema is bootstrapped before the first tick fires.
   * Listeners attached via `on()` must not throw — listener exceptions propagate
   * to the caller of the method that emits the event.
   */
  startBackgroundTasks(): void {
    if (this.bgHandle) return;
    this.bgHandle = startBackgroundTasks({
      pool: this.pool,
      providers: this.providers,
      encryptionKey: this.encryptionKey,
      fetch: this.fetchFn,
      tokenRefreshIntervalMs: this.tokenRefreshIntervalMs,
      tokenRefreshBufferMs: this.tokenRefreshBufferMs,
    });
  }

  stopBackgroundTasks(): void {
    this.bgHandle?.stop();
    this.bgHandle = null;
  }

  async shutdown(): Promise<void> {
    this.bgHandle?.stop();
    this.bgHandle = null;
    await this.pool.end();
  }
}
