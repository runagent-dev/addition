import { createHash, randomBytes } from 'node:crypto';
import { Pool } from 'pg';
import { encrypt, decrypt } from './crypto.js';
import { getProvider } from './providers.js';
import { getAdapter } from './adapters/index.js';
import { ProviderNotFoundError, OAuthError } from './errors.js';
import type {
  ProviderConfig,
  ProviderRegistry,
  TokenExchangeRequest,
} from './types.js';

// ---------- Public types ----------

export interface GetAuthUrlArgs {
  userId: string;
  providerId: string;
  scopes?: string[];
  redirectAfter?: string;
  metadata?: Record<string, unknown>;
}

export interface CallbackResult {
  success: boolean;
  connectionId?: string;
  userId?: string;
  providerId?: string;
  redirectAfter?: string;
  error?: string;
}

export interface OAuthDeps {
  pool: Pool;
  providers: ProviderRegistry;
  encryptionKey: string;
  callbackUrl: string;
  /** Injected so tests can mock network calls without touching internal modules. */
  fetch: typeof fetch;
}

// ---------- PKCE helpers ----------

/** 43–128 unreserved chars. We use 32 random bytes → 43 char base64url. */
function genCodeVerifier(): string {
  return randomBytes(32).toString('base64url');
}

function codeChallenge(verifier: string): string {
  return createHash('sha256').update(verifier).digest('base64url');
}

// ---------- Scope join ----------

function joinScopes(scopes: string[], delimiter: 'space' | 'comma'): string {
  return scopes.join(delimiter === 'comma' ? ',' : ' ');
}

// ---------- Dot-path extraction (CLAUDE.md §6 invariant #10) ----------

function getByPath(obj: unknown, path: string): unknown {
  if (obj == null || typeof obj !== 'object') return undefined;
  let cur: any = obj;
  for (const seg of path.split('.')) {
    if (cur == null || typeof cur !== 'object') return undefined;
    cur = cur[seg];
  }
  return cur;
}

// ---------- DB helpers ----------

interface ProviderCredentials {
  clientId: string;
  clientSecret: string;
}

async function loadCredentials(
  pool: Pool,
  providerId: string,
  encryptionKey: string,
): Promise<ProviderCredentials> {
  const { rows } = await pool.query(
    `SELECT client_id, client_secret_encrypted FROM _addition.provider_configs
       WHERE provider_id = $1 AND enabled = true`,
    [providerId],
  );
  if (rows.length === 0) {
    throw new ProviderNotFoundError(providerId);
  }
  return {
    clientId: rows[0].client_id,
    clientSecret: decrypt(rows[0].client_secret_encrypted, encryptionKey),
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
    `INSERT INTO _addition.audit_log
       (event_type, user_id, provider_id, connection_id, detail)
     VALUES ($1, $2, $3, $4, $5)`,
    [eventType, userId, providerId, connectionId, detail],
  );
}

// ---------- getAuthUrl ----------

export async function getAuthUrl(
  args: GetAuthUrlArgs,
  deps: OAuthDeps,
): Promise<string> {
  const provider = getProvider(deps.providers, args.providerId);
  const creds = await loadCredentials(deps.pool, args.providerId, deps.encryptionKey);

  const scopes = args.scopes ?? provider.oauth2.default_scopes;
  const state = randomBytes(32).toString('hex');

  const verifier = provider.oauth2.pkce_supported ? genCodeVerifier() : null;

  const params = new URLSearchParams();
  params.set('client_id', creds.clientId);
  params.set('redirect_uri', deps.callbackUrl);
  params.set('response_type', 'code');
  params.set('state', state);
  params.set('scope', joinScopes(scopes, provider.oauth2.scopes_delimiter));
  if (verifier) {
    params.set('code_challenge', codeChallenge(verifier));
    params.set('code_challenge_method', 'S256');
  }
  for (const [k, v] of Object.entries(provider.oauth2.authorize_params ?? {})) {
    params.set(k, v);
  }

  const metadata: Record<string, unknown> = { ...(args.metadata ?? {}) };
  if (args.redirectAfter !== undefined) metadata.redirect_after = args.redirectAfter;

  await deps.pool.query(
    `INSERT INTO _addition.oauth_state
       (state, user_id, provider_id, code_verifier, redirect_uri, scopes_requested, metadata)
     VALUES ($1, $2, $3, $4, $5, $6, $7)`,
    [state, args.userId, args.providerId, verifier, deps.callbackUrl, scopes, metadata],
  );

  await writeAudit(deps.pool, 'oauth.authorize_url_issued', args.userId, args.providerId, null, {
    scopes,
    pkce: verifier !== null,
  });

  return `${provider.oauth2.authorize_url}?${params.toString()}`;
}

// ---------- handleCallback ----------

interface StoredState {
  user_id: string;
  provider_id: string;
  code_verifier: string | null;
  redirect_uri: string;
  scopes_requested: string[];
  metadata: Record<string, unknown>;
}

async function consumeState(
  pool: Pool,
  state: string,
): Promise<StoredState | null> {
  const { rows } = await pool.query(
    `DELETE FROM _addition.oauth_state
       WHERE state = $1 AND expires_at > NOW()
       RETURNING user_id, provider_id, code_verifier, redirect_uri, scopes_requested, metadata`,
    [state],
  );
  return rows.length === 0 ? null : (rows[0] as StoredState);
}

async function deleteState(pool: Pool, state: string): Promise<void> {
  await pool.query(`DELETE FROM _addition.oauth_state WHERE state = $1`, [state]);
}

function buildTokenExchangeBody(
  provider: ProviderConfig,
  creds: ProviderCredentials,
  code: string,
  redirectUri: string,
  codeVerifier: string | null,
): Record<string, string> {
  const body: Record<string, string> = {
    grant_type: 'authorization_code',
    code,
    redirect_uri: redirectUri,
    client_id: creds.clientId,
  };
  if (provider.oauth2.client_auth_method === 'body') {
    body.client_secret = creds.clientSecret;
  }
  if (codeVerifier) {
    body.code_verifier = codeVerifier;
  }
  for (const [k, v] of Object.entries(provider.oauth2.token_params ?? {})) {
    body[k] = v;
  }
  return body;
}

async function parseResponse(
  response: Response,
  format: 'json' | 'form',
): Promise<unknown> {
  const text = await response.text();
  if (format === 'json') {
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  }
  // form: application/x-www-form-urlencoded
  const params = new URLSearchParams(text);
  const obj: Record<string, string> = {};
  for (const [k, v] of params.entries()) obj[k] = v;
  return obj;
}

interface ExtractedTokens {
  accessToken: string;
  refreshToken: string | null;
  expiresIn: number | null;
}

function extractTokens(
  provider: ProviderConfig,
  body: unknown,
): ExtractedTokens {
  const mapping = provider.oauth2.token_response_mapping;
  const accessPath = mapping?.access_token ?? 'access_token';
  const refreshPath = mapping?.refresh_token ?? 'refresh_token';
  const expiresPath = mapping?.expires_in ?? 'expires_in';

  const access = getByPath(body, accessPath);
  if (typeof access !== 'string' || access.length === 0) {
    throw new OAuthError(
      'malformed_token_response',
      `access_token not found at '${accessPath}'`,
    );
  }
  const refresh = getByPath(body, refreshPath);
  const expires = getByPath(body, expiresPath);

  return {
    accessToken: access,
    refreshToken: typeof refresh === 'string' && refresh.length > 0 ? refresh : null,
    expiresIn: typeof expires === 'number' ? expires : null,
  };
}

export async function handleCallback(
  query: Record<string, string>,
  deps: OAuthDeps,
): Promise<CallbackResult> {
  const { state } = query;

  // Provider-side errors arrive as ?error=... — respect them first if we can
  // still associate them with a state row.
  if (query.error) {
    if (state) await deleteState(deps.pool, state);
    await writeAudit(deps.pool, 'oauth.callback_error', null, null, null, {
      error: query.error,
      error_description: query.error_description,
    });
    return { success: false, error: query.error };
  }

  if (!state) {
    return { success: false, error: 'invalid_state' };
  }

  const stored = await consumeState(deps.pool, state);
  if (!stored) {
    await writeAudit(deps.pool, 'oauth.invalid_state', null, null, null, { state });
    return { success: false, error: 'invalid_state' };
  }

  const provider = getProvider(deps.providers, stored.provider_id);
  const adapter = getAdapter(stored.provider_id);
  const creds = await loadCredentials(deps.pool, stored.provider_id, deps.encryptionKey);

  const bodyParams = buildTokenExchangeBody(
    provider,
    creds,
    query.code ?? '',
    stored.redirect_uri,
    stored.code_verifier,
  );

  const baseHeaders: Record<string, string> = {
    'Content-Type': 'application/x-www-form-urlencoded',
  };
  if (provider.oauth2.client_auth_method === 'header') {
    const basic = Buffer.from(`${creds.clientId}:${creds.clientSecret}`).toString('base64');
    baseHeaders.Authorization = `Basic ${basic}`;
  }

  const tokenReq: TokenExchangeRequest = {
    url: provider.oauth2.token_url,
    method: 'POST',
    headers: baseHeaders,
    body: bodyParams,
  };
  const finalReq = adapter.beforeTokenExchange(tokenReq);
  const formBody = new URLSearchParams(finalReq.body).toString();

  const response = await deps.fetch(finalReq.url, {
    method: finalReq.method,
    headers: finalReq.headers,
    body: formBody,
  });

  const parsed = await parseResponse(response, provider.oauth2.token_response_format);

  if (adapter.isErrorResponse(response.status, parsed)) {
    const detail = adapter.extractErrorDetail(parsed);
    await writeAudit(deps.pool, 'oauth.token_exchange_failed', stored.user_id, stored.provider_id, null, {
      code: detail.code,
      description: detail.description,
      status: response.status,
    });
    return { success: false, error: detail.code };
  }

  let tokens: ExtractedTokens;
  try {
    tokens = extractTokens(provider, parsed);
  } catch (err) {
    const code = err instanceof OAuthError ? err.code : 'malformed_token_response';
    await writeAudit(deps.pool, 'oauth.token_exchange_failed', stored.user_id, stored.provider_id, null, {
      code,
    });
    return { success: false, error: code };
  }

  const accessEnc = encrypt(tokens.accessToken, deps.encryptionKey);
  const refreshEnc = tokens.refreshToken ? encrypt(tokens.refreshToken, deps.encryptionKey) : null;
  const rawEnc = encrypt(
    JSON.stringify(parsed),
    deps.encryptionKey,
  );
  const expiresAt = tokens.expiresIn != null ? new Date(Date.now() + tokens.expiresIn * 1000) : null;

  const { rows } = await deps.pool.query(
    `INSERT INTO _addition.connections
       (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
        token_expires_at, raw_token_response_encrypted, scopes_granted)
     VALUES ($1, $2, $3, $4, $5, $6, $7)
     RETURNING id`,
    [
      stored.user_id,
      stored.provider_id,
      accessEnc,
      refreshEnc,
      expiresAt,
      rawEnc,
      stored.scopes_requested,
    ],
  );
  const connectionId = rows[0].id as string;

  await writeAudit(deps.pool, 'connection.created', stored.user_id, stored.provider_id, connectionId, {
    pkce: stored.code_verifier !== null,
    has_refresh_token: refreshEnc !== null,
  });

  const result: CallbackResult = {
    success: true,
    connectionId,
    userId: stored.user_id,
    providerId: stored.provider_id,
  };
  const redirectAfter = stored.metadata?.redirect_after;
  if (typeof redirectAfter === 'string') result.redirectAfter = redirectAfter;
  return result;
}
