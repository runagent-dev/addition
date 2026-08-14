/**
 * Cross-language conformance suite.
 *
 * Loads every scenario from shared/test-fixtures/*.yaml and drives it through
 * the public Addition class API. Must pass 100% before tagging a release.
 * The same fixtures drive the Python implementation — any behavioural difference
 * between the two language tracks surfaces here.
 */
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { readdirSync, readFileSync } from 'node:fs';
import { parse as parseYaml } from 'yaml';
import { PostgreSqlContainer, type StartedPostgreSqlContainer } from '@testcontainers/postgresql';
import { Pool } from 'pg';
import { Addition } from '../../src/addition.js';
import { TokenRefreshError } from '../../src/errors.js';

const FIXTURES_DIR = join(__dirname, '..', '..', '..', 'shared', 'test-fixtures');
const TEST_KEY = '9'.repeat(64);
const CALLBACK_URL = 'http://localhost:3000/callback';

let container: StartedPostgreSqlContainer;
let databaseUrl: string;

beforeAll(async () => {
  container = await new PostgreSqlContainer('postgres:16-alpine')
    .withDatabase('addition_conformance')
    .start();
  databaseUrl = container.getConnectionUri();
});

afterAll(async () => {
  if (container) await container.stop();
});

async function withFreshDb<T>(fn: (dbUrl: string) => Promise<T>): Promise<T> {
  const dbName = `conf_${Math.random().toString(36).slice(2, 10)}`;
  const admin = new Pool({ connectionString: databaseUrl });
  try {
    await admin.query(`CREATE DATABASE ${dbName}`);
  } finally {
    await admin.end();
  }
  const u = new URL(databaseUrl);
  u.pathname = '/' + dbName;
  const freshUrl = u.toString();
  try {
    return await fn(freshUrl);
  } finally {
    const cleanup = new Pool({ connectionString: databaseUrl });
    try {
      await cleanup.query(
        `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, [dbName],
      );
      await cleanup.query(`DROP DATABASE IF EXISTS ${dbName}`);
    } finally {
      await cleanup.end();
    }
  }
}

function mkFetch(responses: Array<{ status: number; body: unknown }>): typeof fetch {
  let i = 0;
  return vi.fn(async () => {
    const r = responses[Math.min(i, responses.length - 1)];
    i++;
    return {
      status: r.status,
      ok: r.status >= 200 && r.status < 300,
      headers: new Headers({ 'content-type': 'application/json' }),
      text: async () => JSON.stringify(r.body),
      json: async () => r.body,
    } as unknown as Response;
  }) as unknown as typeof fetch;
}

async function makeAddition(dbUrl: string, fetchImpl: typeof fetch): Promise<{ addition: Addition; connId?: string }> {
  const addition = new Addition({ databaseUrl: dbUrl, encryptionKey: TEST_KEY, callbackUrl: CALLBACK_URL, fetch: fetchImpl });
  await addition.listConfiguredProviders(); // trigger bootstrap
  return { addition };
}

async function seedConnection(
  dbUrl: string,
  userId: string,
  providerId: string,
  accessToken: string,
  refreshToken: string | null,
): Promise<string> {
  const { encrypt } = await import('../../src/crypto.js');
  const pool = new Pool({ connectionString: dbUrl });
  try {
    const { rows } = await pool.query(
      `INSERT INTO _addition.connections
         (user_id, provider_id, access_token_encrypted, refresh_token_encrypted,
          token_expires_at, scopes_granted)
       VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
      [userId, providerId, encrypt(accessToken, TEST_KEY),
       refreshToken ? encrypt(refreshToken, TEST_KEY) : null, null, []],
    );
    return rows[0].id as string;
  } finally {
    await pool.end();
  }
}

// ─── Fixture loader ───────────────────────────────────────────────────────────

function loadFixtures(): Array<{ file: string; scenarios: unknown[] }> {
  return readdirSync(FIXTURES_DIR)
    .filter((f) => f.endsWith('.yaml') || f.endsWith('.yml'))
    .map((f) => {
      const raw = parseYaml(readFileSync(join(FIXTURES_DIR, f), 'utf8')) as Record<string, unknown>;
      return { file: f, hasScenarios: raw != null && 'scenarios' in raw, scenarios: (raw?.scenarios as unknown[]) ?? [] };
    })
    // Fixtures without a `scenarios:` key are non-scenario vectors (e.g. crypto_interop,
    // driven directly by the crypto unit tests) — skip them here rather than fail collection.
    .filter((x) => x.hasScenarios);
}

// ─── Proxy scenario driver ────────────────────────────────────────────────────

interface ProxyScenario {
  name: string;
  description: string;
  provider: string;
  method: string;
  path: string;
  params: Record<string, unknown>;
  body: unknown;
  mock_response: { status: number; body: unknown };
  expected: {
    status: number;
    auth_header?: string;
    auth_prefix?: string;
    url_query_params?: Record<string, string>;
    request_has_body?: boolean;
  };
}

function isProxyScenario(s: unknown): s is ProxyScenario {
  return typeof s === 'object' && s !== null && 'method' in s && 'path' in s;
}

// ─── Token refresh scenario driver ───────────────────────────────────────────

interface TokenRefreshScenario {
  name: string;
  description: string;
  provider: string;
  initial: { access_token: string; refresh_token: string };
  mock_response: { status: number; body: unknown };
  expected: { access_token: string; refresh_token: string; status: string };
}

function isTokenRefreshScenario(s: unknown): s is TokenRefreshScenario {
  return typeof s === 'object' && s !== null && 'initial' in s && 'mock_response' in s;
}

// ─── Conformance suite ────────────────────────────────────────────────────────

const fixtures = loadFixtures();

// Fail fast at collection time if a fixture file is empty or mis-keyed.
for (const { file, scenarios } of fixtures) {
  if (scenarios.length === 0) {
    throw new Error(`Fixture '${file}' has 0 scenarios — check for a parse error or wrong top-level key`);
  }
}

for (const { file, scenarios } of fixtures) {
  describe(`conformance — ${file}`, () => {
    for (const scenario of scenarios) {
      if (isProxyScenario(scenario)) {
        it(scenario.name, async () => {
          await withFreshDb(async (dbUrl) => {
            const spy = mkFetch([scenario.mock_response]);
            const { addition } = await makeAddition(dbUrl, spy);

            await addition.configureProvider({
              providerId: scenario.provider,
              clientId: 'test-client',
              clientSecret: 'test-secret',
            });
            await seedConnection(dbUrl, 'conformance-user', scenario.provider, 'conformance-token', null);

            const result = await addition.proxy({
              userId: 'conformance-user',
              providerId: scenario.provider,
              method: scenario.method,
              path: scenario.path,
              params: scenario.params as Record<string, string | number | boolean>,
              body: scenario.body ?? undefined,
            });

            expect(result.status).toBe(scenario.expected.status);

            const calls = (spy as unknown as { mock: { calls: unknown[][] } }).mock.calls;
            const url = calls[0][0] as string;
            const init = calls[0][1] as { headers: Record<string, string>; body?: string };

            if (scenario.expected.auth_header && scenario.expected.auth_prefix) {
              expect(init.headers[scenario.expected.auth_header]).toMatch(
                new RegExp(`^${scenario.expected.auth_prefix} `),
              );
            }
            if (scenario.expected.url_query_params) {
              const parsed = new URL(url);
              for (const [k, v] of Object.entries(scenario.expected.url_query_params)) {
                expect(parsed.searchParams.get(k)).toBe(v);
              }
            }
            if (scenario.expected.request_has_body) {
              expect(init.body).toBeTruthy();
            }

            await addition.shutdown();
          });
        });
      } else if (isTokenRefreshScenario(scenario)) {
        it(scenario.name, async () => {
          await withFreshDb(async (dbUrl) => {
            const isFailure = scenario.expected.status === 'refresh_failed';

            // Proxy flow: 401 triggers refresh. For failure, no retry follows.
            const fetchResponses = isFailure
              ? [
                  { status: 401, body: {} },
                  scenario.mock_response,
                ]
              : [
                  { status: 401, body: {} },
                  scenario.mock_response,
                  { status: 200, body: {} },
                ];

            const spy = mkFetch(fetchResponses);
            const { addition } = await makeAddition(dbUrl, spy);

            await addition.configureProvider({
              providerId: scenario.provider,
              clientId: 'test-client',
              clientSecret: 'test-secret',
            });
            const connId = await seedConnection(
              dbUrl,
              'conformance-user',
              scenario.provider,
              scenario.initial.access_token,
              scenario.initial.refresh_token,
            );

            if (isFailure) {
              await expect(
                addition.proxy({
                  userId: 'conformance-user',
                  providerId: scenario.provider,
                  method: 'GET',
                  path: '/oauth2/v1/userinfo',
                }),
              ).rejects.toThrow(TokenRefreshError);
            } else {
              await addition.proxy({
                userId: 'conformance-user',
                providerId: scenario.provider,
                method: 'GET',
                path: '/oauth2/v1/userinfo',
              });
            }

            const conn = await addition.getConnection(connId);
            expect(conn).not.toBeNull();
            expect(conn!.accessToken).toBe(scenario.expected.access_token);
            expect(conn!.refreshToken).toBe(scenario.expected.refresh_token);
            expect(conn!.status).toBe(scenario.expected.status);

            await addition.shutdown();
          });
        });
      } else {
        const name = (scenario as Record<string, unknown>).name ?? JSON.stringify(scenario);
        it(`UNRECOGNISED scenario: ${name}`, () => {
          throw new Error(
            `Scenario shape not handled by any driver — add a driver in conformance.test.ts or fix the fixture`,
          );
        });
      }
    }
  });
}
