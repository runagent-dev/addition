import { Pool } from 'pg';
import { findActiveConnection, getConnectionById, refreshConnection } from './tokens.js';
import { getAdapter } from './adapters/index.js';
import { ConnectionNotFoundError, ConnectionExpiredError, ProxyError, ProviderNotFoundError } from './errors.js';
import type { ProviderRegistry } from './types.js';

export interface ProxyDeps {
  pool: Pool;
  providers: ProviderRegistry;
  encryptionKey: string;
  fetch: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  onTokenRefreshed?: (connectionId: string) => void;
}

export interface ProxyArgs {
  userId: string;
  providerId: string;
  method: string;
  path: string;
  body?: unknown;
  params?: Record<string, string | number | boolean>;
  headers?: Record<string, string>;
  connectionId?: string;
}

export interface ProxyResponse {
  status: number;
  headers: Record<string, string>;
  body: unknown;
}

const DEFAULT_SLEEP = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));
const MAX_429_RETRIES = 3;

export async function proxy(args: ProxyArgs, deps: ProxyDeps): Promise<ProxyResponse> {
  const { userId, providerId, method, path, body, params, headers: callerHeaders, connectionId } = args;
  const { pool, providers, encryptionKey, fetch: fetchFn, sleep = DEFAULT_SLEEP } = deps;

  const provider = providers[providerId];
  if (!provider) throw new ProviderNotFoundError(providerId);

  const adapter = getAdapter(providerId);

  let conn = connectionId
    ? await getConnectionById(pool, connectionId, encryptionKey)
    : await findActiveConnection(pool, userId, providerId, encryptionKey);

  // Enforce multi-tenancy: explicit connectionId must belong to the requesting user.
  if (!conn || (connectionId && conn.userId !== userId)) {
    throw new ConnectionNotFoundError(connectionId ?? `${userId}/${providerId}`);
  }

  await pool.query(
    `INSERT INTO _addition.audit_log (event_type, user_id, provider_id, connection_id, detail)
     VALUES ($1, $2, $3, $4, $5)`,
    ['proxy.called', userId, providerId, conn.id, { method, path }],
  );

  const urlObj = new URL(provider.api.base_url.replace(/\/$/, '') + path);
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      urlObj.searchParams.set(k, String(v));
    }
  }
  const requestUrl = urlObj.toString();

  const makeInit = (accessToken: string): RequestInit => {
    const headers: Record<string, string> = {
      ...callerHeaders,
      [provider.api.auth_header]: `${provider.api.auth_prefix} ${accessToken}`,
    };
    if (body !== undefined && body !== null) {
      headers['Content-Type'] = 'application/json';
    }
    const init: RequestInit = { method, headers };
    if (body !== undefined && body !== null) {
      init.body = JSON.stringify(body);
    }
    return init;
  };

  const parseBody = async (res: Response): Promise<unknown> => {
    try { return await res.json(); } catch { return await res.text(); }
  };

  let networkRetried = false;
  const executeRequest = async (accessToken: string): Promise<{ res: Response; parsed: unknown }> => {
    let res: Response;
    try {
      res = await fetchFn(requestUrl, makeInit(accessToken));
    } catch (err) {
      if (!networkRetried) {
        networkRetried = true;
        await sleep(1000);
        res = await fetchFn(requestUrl, makeInit(accessToken));
      } else {
        throw err;
      }
    }
    return { res, parsed: await parseBody(res) };
  };

  let retries429 = 0;

  for (;;) {
    let { res, parsed } = await executeRequest(conn.accessToken);

    // 401: inline refresh+retry — does not loop back through the 401 check.
    if (res.status === 401) {
      if (!conn.refreshToken) throw new ConnectionExpiredError(conn.id);
      conn = await refreshConnection(conn.id, { pool, providers, encryptionKey, fetch: fetchFn });
      deps.onTokenRefreshed?.(conn.id);
      ({ res, parsed } = await executeRequest(conn.accessToken));
    }

    if (res.status === 429 && retries429 < MAX_429_RETRIES) {
      retries429++;
      const retryAfter = Math.min(parseInt(res.headers.get('Retry-After') ?? '1', 10), 60);
      await sleep(retryAfter * 1000);
      continue;
    }

    if (res.status !== 429 && adapter.isErrorResponse(res.status, parsed)) {
      const detail = adapter.extractErrorDetail(parsed);
      throw new ProxyError(res.status, detail.code, detail.description);
    }

    const responseHeaders: Record<string, string> = {};
    res.headers.forEach((v, k) => { responseHeaders[k] = v; });

    return { status: res.status, headers: responseHeaders, body: parsed };
  }
}
