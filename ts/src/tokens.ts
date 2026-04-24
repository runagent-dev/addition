import { Pool } from 'pg';
import { encrypt, decrypt } from './crypto.js';
import { getAdapter } from './adapters/index.js';
import { ConnectionNotFoundError, ProviderNotFoundError, TokenRefreshError } from './errors.js';
import { getByPath } from './utils.js';
import type { ProviderRegistry } from './types.js';

// ---------- Public types ----------

export interface StoredTokens {
  accessToken: string;
  refreshToken: string | null;
  expiresIn: number | null;
  scopesGranted: string[];
}

export interface ConnectionRecord {
  id: string;
  userId: string;
  providerId: string;
  accessToken: string;
  refreshToken: string | null;
  tokenExpiresAt: Date | null;
  scopesGranted: string[];
  status: string;
}

export interface TokenDeps {
  pool: Pool;
  providers: ProviderRegistry;
  encryptionKey: string;
  fetch: typeof fetch;
}

// ---------- Internal helpers ----------

function expiresAtFromIn(expiresIn: number): Date {
  return new Date(Date.now() + expiresIn * 1000);
}

function decryptRow(row: Record<string, unknown>, encryptionKey: string): ConnectionRecord {
  return {
    id: row.id as string,
    userId: row.user_id as string,
    providerId: row.provider_id as string,
    accessToken: decrypt(row.access_token_encrypted as string, encryptionKey),
    refreshToken: row.refresh_token_encrypted
      ? decrypt(row.refresh_token_encrypted as string, encryptionKey)
      : null,
    tokenExpiresAt: row.token_expires_at as Date | null,
    scopesGranted: row.scopes_granted as string[],
    status: row.status as string,
  };
}

async function loadClientSecret(
  pool: Pool,
  providerId: string,
  encryptionKey: string,
): Promise<{ clientId: string; clientSecret: string }> {
  const { rows } = await pool.query(
    `SELECT client_id, client_secret_encrypted FROM _addition.provider_configs WHERE provider_id = $1`,
    [providerId],
  );
  if (rows.length === 0) throw new ProviderNotFoundError(providerId);
  return {
    clientId: rows[0].client_id as string,
    clientSecret: decrypt(rows[0].client_secret_encrypted as string, encryptionKey),
  };
}

async function writeAudit(
  pool: Pool,
  eventType: string,
  userId: string | null,
  providerId: string | null,
  connectionId: string | null,
  detail: Record<string, unknown>,
): Promise<void> {
  await pool.query(
    `INSERT INTO _addition.audit_log (event_type, user_id, provider_id, connection_id, detail)
     VALUES ($1, $2, $3, $4, $5)`,
    [eventType, userId, providerId, connectionId, detail],
  );
}

// ---------- Public API ----------

export async function storeConnection(
  pool: Pool,
  userId: string,
  providerId: string,
  tokens: StoredTokens,
  encryptionKey: string,
): Promise<string> {
  const accessEnc = encrypt(tokens.accessToken, encryptionKey);
  const refreshEnc = tokens.refreshToken ? encrypt(tokens.refreshToken, encryptionKey) : null;
  const expiresAt = tokens.expiresIn != null ? expiresAtFromIn(tokens.expiresIn) : null;
  const { rows } = await pool.query(
    `INSERT INTO _addition.connections
       (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
        token_expires_at, scopes_granted)
     VALUES ($1, $2, $3, $4, $5, $6)
     RETURNING id`,
    [userId, providerId, accessEnc, refreshEnc, expiresAt, tokens.scopesGranted],
  );
  const connectionId = rows[0].id as string;
  await writeAudit(pool, 'connection.created', userId, providerId, connectionId, {
    has_refresh_token: refreshEnc !== null,
    scopes: tokens.scopesGranted,
  });
  return connectionId;
}

/**
 * Looks up a connection by its UUID. Spec-compliant signature for the M12
 * public `getConnection` method.
 */
export async function getConnectionById(
  pool: Pool,
  connectionId: string,
  encryptionKey: string,
): Promise<ConnectionRecord | null> {
  const { rows } = await pool.query(
    `SELECT id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
            token_expires_at, scopes_granted, status
     FROM _addition.connections WHERE id = $1`,
    [connectionId],
  );
  if (rows.length === 0) return null;
  return decryptRow(rows[0], encryptionKey);
}

/**
 * Finds the most recently created active connection for a user+provider pair.
 * Used internally by the proxy and executeTool when no explicit connectionId
 * is provided. Returns null when no active connection exists.
 */
export async function findActiveConnection(
  pool: Pool,
  userId: string,
  providerId: string,
  encryptionKey: string,
): Promise<ConnectionRecord | null> {
  const { rows } = await pool.query(
    `SELECT id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
            token_expires_at, scopes_granted, status
     FROM _addition.connections
     WHERE user_id = $1 AND provider_id = $2 AND status = 'active'
     ORDER BY created_at DESC LIMIT 1`,
    [userId, providerId],
  );
  if (rows.length === 0) return null;
  return decryptRow(rows[0], encryptionKey);
}

export async function refreshConnection(
  connectionId: string,
  deps: TokenDeps,
): Promise<ConnectionRecord> {
  const { pool, providers, encryptionKey, fetch: fetchFn } = deps;

  const { rows: connRows } = await pool.query(
    `SELECT id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
            token_expires_at, scopes_granted, status
     FROM _addition.connections WHERE id = $1`,
    [connectionId],
  );
  if (connRows.length === 0) throw new ConnectionNotFoundError(connectionId);

  const row = connRows[0];
  const providerId: string = row.provider_id as string;
  const userId: string = row.user_id as string;
  const provider = providers[providerId];

  // No-op: provider doesn't support refresh or there is no refresh token stored.
  if (!provider.oauth2.refresh_supported || !row.refresh_token_encrypted) {
    return decryptRow(row, encryptionKey);
  }

  const refreshToken = decrypt(row.refresh_token_encrypted as string, encryptionKey);
  const creds = await loadClientSecret(pool, providerId, encryptionKey);
  const adapter = getAdapter(providerId);

  const body: Record<string, string> = {
    grant_type: 'refresh_token',
    refresh_token: refreshToken,
    client_id: creds.clientId,
  };
  if (provider.oauth2.client_auth_method === 'body') {
    body.client_secret = creds.clientSecret;
  }

  const headers: Record<string, string> = {
    'Content-Type': 'application/x-www-form-urlencoded',
  };
  if (provider.oauth2.client_auth_method === 'header') {
    const basic = Buffer.from(`${creds.clientId}:${creds.clientSecret}`).toString('base64');
    headers.Authorization = `Basic ${basic}`;
  }

  const shaped = adapter.beforeTokenExchange({
    url: provider.oauth2.token_url,
    method: 'POST',
    headers,
    body,
  });

  const response = await fetchFn(shaped.url, {
    method: shaped.method,
    headers: shaped.headers,
    body: new URLSearchParams(shaped.body).toString(),
  });

  let parsed: unknown;
  const text = await response.text();
  if (provider.oauth2.token_response_format === 'json') {
    try { parsed = JSON.parse(text); } catch { parsed = text; }
  } else {
    const p = new URLSearchParams(text);
    const obj: Record<string, string> = {};
    for (const [k, v] of p.entries()) obj[k] = v;
    parsed = obj;
  }

  if (adapter.isErrorResponse(response.status, parsed)) {
    const detail = adapter.extractErrorDetail(parsed);
    await pool.query(
      `UPDATE _addition.connections
         SET status = 'refresh_failed', status_detail = $2, updated_at = NOW()
       WHERE id = $1`,
      [connectionId, detail.code],
    );
    await writeAudit(pool, 'token.refresh_failed', userId, providerId, connectionId, {
      code: detail.code,
      description: detail.description ?? null,
    });
    throw new TokenRefreshError(detail.code, detail.description);
  }

  const mapping = provider.oauth2.token_response_mapping;
  const accessPath = mapping?.access_token ?? 'access_token';
  const refreshPath = mapping?.refresh_token ?? 'refresh_token';
  const expiresPath = mapping?.expires_in ?? 'expires_in';

  const newAccess = getByPath(parsed, accessPath);
  if (typeof newAccess !== 'string' || newAccess.length === 0) {
    await pool.query(
      `UPDATE _addition.connections
         SET status = 'refresh_failed', status_detail = $2, updated_at = NOW()
       WHERE id = $1`,
      [connectionId, 'malformed_token_response'],
    );
    await writeAudit(pool, 'token.refresh_failed', userId, providerId, connectionId, {
      code: 'malformed_token_response',
    });
    throw new TokenRefreshError('malformed_token_response');
  }

  const newRefresh = getByPath(parsed, refreshPath);
  const newExpiresIn = getByPath(parsed, expiresPath);

  const newAccessEnc = encrypt(newAccess, encryptionKey);
  // Keep the existing refresh token when the provider does not issue a new one.
  const newRefreshEnc = (typeof newRefresh === 'string' && newRefresh.length > 0)
    ? encrypt(newRefresh, encryptionKey)
    : (row.refresh_token_encrypted as string | null);
  // Use COALESCE: only update expires_at when the provider supplies a new value;
  // never write null over an existing expiry.
  const newExpiresAt = typeof newExpiresIn === 'number' ? expiresAtFromIn(newExpiresIn) : null;

  const { rows: updated } = await pool.query(
    `UPDATE _addition.connections
       SET access_token_encrypted = $2,
           refresh_token_encrypted = $3,
           token_expires_at = COALESCE($4, token_expires_at),
           status = 'active',
           status_detail = NULL,
           updated_at = NOW()
     WHERE id = $1
     RETURNING id, user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
               token_expires_at, scopes_granted, status`,
    [connectionId, newAccessEnc, newRefreshEnc, newExpiresAt],
  );

  await writeAudit(pool, 'token.refresh_succeeded', userId, providerId, connectionId, {
    has_new_refresh_token: typeof newRefresh === 'string' && newRefresh.length > 0,
  });

  return decryptRow(updated[0], encryptionKey);
}

export async function listConnections(
  pool: Pool,
  userId: string,
): Promise<Array<{ id: string; providerId: string; scopesGranted: string[]; status: string; createdAt: Date }>> {
  const { rows } = await pool.query(
    `SELECT id, provider_id, scopes_granted, status, created_at
     FROM _addition.connections
     WHERE user_id = $1
     ORDER BY created_at DESC`,
    [userId],
  );
  return rows.map((r) => ({
    id: r.id as string,
    providerId: r.provider_id as string,
    scopesGranted: r.scopes_granted as string[],
    status: r.status as string,
    createdAt: r.created_at as Date,
  }));
}

/**
 * Revokes a connection by its UUID. Spec-compliant (returns void, idempotent
 * on a non-existent ID). Writes a `connection.revoked` audit row when a row
 * is actually deleted.
 */
export async function revokeConnection(
  pool: Pool,
  connectionId: string,
): Promise<void> {
  const { rows } = await pool.query(
    `DELETE FROM _addition.connections WHERE id = $1
     RETURNING user_id, provider_id`,
    [connectionId],
  );
  if (rows.length > 0) {
    await writeAudit(pool, 'connection.revoked', rows[0].user_id as string, rows[0].provider_id as string, connectionId, {});
  }
}
