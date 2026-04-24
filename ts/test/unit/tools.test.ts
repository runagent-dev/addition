import { beforeAll, describe, expect, it, vi } from 'vitest';
import { join } from 'node:path';
import { loadProviders } from '../../src/providers.js';
import { loadToolkits } from '../../src/toolkits.js';
import { executeTool } from '../../src/tools.js';
import { ToolNotFoundError, ToolValidationError } from '../../src/errors.js';
import type { ToolkitRegistry, ProviderRegistry } from '../../src/types.js';
import type { ProxyFn } from '../../src/tools.js';

const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');
const SHARED_TOOLKITS = join(__dirname, '..', '..', '..', 'shared', 'toolkits');

let providers: ProviderRegistry;
let toolkits: ToolkitRegistry;

beforeAll(() => {
  providers = loadProviders(SHARED_PROVIDERS);
  toolkits = loadToolkits(SHARED_TOOLKITS, providers);
});

function mkProxy(response = { status: 200, body: {} }): ProxyFn & { mock: ReturnType<typeof vi.fn> } {
  const fn = vi.fn().mockResolvedValue(response);
  return fn as unknown as ProxyFn & { mock: ReturnType<typeof vi.fn> };
}

describe('tool engine — param validation', () => {
  it('missing required param throws ToolValidationError listing the missing name', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'github',
          toolId: 'create_issue',
          params: { owner: 'o' }, // missing: repo, title (both required)
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(ToolValidationError);

    try {
      await executeTool(
        {
          userId: 'u',
          providerId: 'github',
          toolId: 'create_issue',
          params: { owner: 'o' },
        },
        { toolkits, proxy },
      );
    } catch (err) {
      expect((err as ToolValidationError).missing).toContain('repo');
      expect((err as ToolValidationError).missing).toContain('title');
      expect((err as Error).message).toMatch(/repo/);
      expect((err as Error).message).toMatch(/title/);
    }
  });

  it('wrong type (string where integer expected) throws ToolValidationError', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'gmail',
          toolId: 'list_messages',
          params: { maxResults: 'not a number' },
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(/maxResults/);
  });

  it('non-integer number (e.g. 1.5) rejected when type is integer', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'gmail',
          toolId: 'list_messages',
          params: { maxResults: 1.5 },
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(ToolValidationError);
  });

  it('string[] rejected when any element is not a string', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'gmail',
          toolId: 'list_messages',
          params: { labelIds: ['INBOX', 42] },
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(/labelIds/);
  });

  it('enum: invalid value rejected', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'gmail',
          toolId: 'get_message',
          params: { messageId: 'abc', format: 'invalid' },
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(/format/);
  });

  it('enum: valid value accepted', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'get_message',
        params: { messageId: 'abc', format: 'metadata' },
      },
      { toolkits, proxy },
    );
    expect(proxy).toHaveBeenCalled();
  });

  it('defaults are applied for omitted optional params', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: {},
      },
      { toolkits, proxy },
    );
    // list_messages default maxResults=10 (per gmail.yaml)
    expect(proxy).toHaveBeenCalledWith(
      expect.objectContaining({
        params: expect.objectContaining({ maxResults: '10' }),
      }),
    );
  });
});

describe('tool engine — path interpolation', () => {
  it('interpolates path params and REMOVES them from the body (CLAUDE.md §6 invariant #4)', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'github',
        toolId: 'create_issue',
        params: { owner: 'myorg', repo: 'myrepo', title: 'Bug', body: 'details' },
      },
      { toolkits, proxy },
    );

    const [call] = proxy.mock.calls;
    const req = call[0];
    expect(req.method).toBe('POST');
    expect(req.path).toBe('/repos/myorg/myrepo/issues');

    // owner and repo MUST NOT appear in the body
    expect(req.body).toBeDefined();
    expect(req.body).not.toHaveProperty('owner');
    expect(req.body).not.toHaveProperty('repo');
    // But the non-path params should
    expect(req.body.title).toBe('Bug');
    expect(req.body.body).toBe('details');
  });

  it('interpolates path params into GET query path and removes from query too', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'get_message',
        params: { messageId: 'abc123' },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.path).toBe('/gmail/v1/users/me/messages/abc123');
    // messageId must not leak into query
    expect(req.params).not.toHaveProperty('messageId');
  });

  it('handles multiple path params in the same operation', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'github',
        toolId: 'list_issues',
        params: { owner: 'o', repo: 'r' },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.path).toBe('/repos/o/r/issues');
    expect(req.params).not.toHaveProperty('owner');
    expect(req.params).not.toHaveProperty('repo');
  });

  it('throws ToolValidationError if a path param is missing from user input', async () => {
    const proxy = mkProxy();
    // list_issues requires owner+repo as path params
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'github',
          toolId: 'list_issues',
          params: { repo: 'r' }, // missing owner
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(/owner/);
  });
});

describe('tool engine — method routing', () => {
  it('GET: non-path params go to query, body is undefined', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: { query: 'is:unread', maxResults: 5 },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.method).toBe('GET');
    expect(req.params).toEqual({ query: 'is:unread', maxResults: '5' });
    expect(req.body).toBeUndefined();
  });

  it('DELETE: params go to query, body undefined', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'google-calendar',
        toolId: 'delete_event',
        params: { eventId: 'evt1' },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.method).toBe('DELETE');
    expect(req.path).toBe('/calendars/primary/events/evt1');
    expect(req.body).toBeUndefined();
  });

  it('POST without transform: params go to JSON body, query empty or undefined', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'slack',
        toolId: 'send_message',
        params: { channel: 'C123', text: 'hi' },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.method).toBe('POST');
    expect(req.body).toEqual({ channel: 'C123', text: 'hi' });
    expect(req.params ?? {}).toEqual({});
  });

  it('query params are stringified (integer 5 → "5", boolean true → "true")', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'google-calendar',
        toolId: 'list_events',
        params: { maxResults: 5, singleEvents: true },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.params!.maxResults).toBe('5');
    expect(req.params!.singleEvents).toBe('true');
  });

  it('string[] query params are joined with commas', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: { labelIds: ['INBOX', 'UNREAD'] },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.params!.labelIds).toBe('INBOX,UNREAD');
  });
});

describe('tool engine — transforms', () => {
  it('gmail send_message: transform produces {body:{raw}} base64url string', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'send_message',
        params: { to: 'alice@example.com', subject: 'Hello', body: 'Hi Alice' },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.method).toBe('POST');
    expect(req.path).toBe('/gmail/v1/users/me/messages/send');
    expect(req.body).toHaveProperty('raw');
    expect(typeof req.body.raw).toBe('string');

    const decoded = Buffer.from(req.body.raw, 'base64url').toString('utf8');
    expect(decoded).toContain('To: alice@example.com');
    expect(decoded).toContain('Subject: Hello');
    expect(decoded).toContain('Hi Alice');
  });

  it('google-calendar create_event: transform wraps dateTime', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'google-calendar',
        toolId: 'create_event',
        params: {
          summary: 'Meeting',
          startDateTime: '2024-06-15T09:00:00Z',
          endDateTime: '2024-06-15T10:00:00Z',
        },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.body.summary).toBe('Meeting');
    expect(req.body.start).toEqual({ dateTime: '2024-06-15T09:00:00Z', timeZone: 'UTC' });
    expect(req.body.end).toEqual({ dateTime: '2024-06-15T10:00:00Z', timeZone: 'UTC' });
  });

  it('google-calendar update_event: transform does NOT leak eventId (path param) into body', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'google-calendar',
        toolId: 'update_event',
        params: { eventId: 'evt-1', summary: 'renamed' },
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.path).toBe('/calendars/primary/events/evt-1');
    expect(req.body).not.toHaveProperty('eventId');
    expect(req.body.summary).toBe('renamed');
  });
});

describe('tool engine — dispatch shape', () => {
  it('passes userId, providerId, connectionId through to the proxy', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'user-42',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: {},
        connectionId: 'conn-abc',
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.userId).toBe('user-42');
    expect(req.providerId).toBe('gmail');
    expect(req.connectionId).toBe('conn-abc');
  });

  it('connectionId is omitted when not provided', async () => {
    const proxy = mkProxy();
    await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: {},
      },
      { toolkits, proxy },
    );
    const req = proxy.mock.calls[0][0];
    expect(req.connectionId).toBeUndefined();
  });

  it('returns a ToolExecutionResult reflecting the proxy response', async () => {
    const proxy = mkProxy({ status: 200, body: { messages: [{ id: 'm1' }] } });
    const result = await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: {},
      },
      { toolkits, proxy },
    );
    expect(result.success).toBe(true);
    expect(result.status).toBe(200);
    expect(result.data).toEqual({ messages: [{ id: 'm1' }] });
  });

  it('non-2xx response produces success: false', async () => {
    const proxy = mkProxy({ status: 500, body: { error: 'server exploded' } });
    const result = await executeTool(
      {
        userId: 'u',
        providerId: 'gmail',
        toolId: 'list_messages',
        params: {},
      },
      { toolkits, proxy },
    );
    expect(result.success).toBe(false);
    expect(result.status).toBe(500);
  });
});

describe('tool engine — errors', () => {
  it('unknown tool throws ToolNotFoundError', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'gmail',
          toolId: 'bogus_tool',
          params: {},
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(ToolNotFoundError);
  });

  it('unknown provider throws ToolNotFoundError', async () => {
    const proxy = mkProxy();
    await expect(
      executeTool(
        {
          userId: 'u',
          providerId: 'no-such-provider',
          toolId: 'anything',
          params: {},
        },
        { toolkits, proxy },
      ),
    ).rejects.toThrow(ToolNotFoundError);
  });

  it('validation error never reaches the proxy', async () => {
    const proxy = mkProxy();
    try {
      await executeTool(
        {
          userId: 'u',
          providerId: 'github',
          toolId: 'create_issue',
          params: { owner: 'o' }, // missing required repo, title
        },
        { toolkits, proxy },
      );
    } catch {
      // expected
    }
    expect(proxy).not.toHaveBeenCalled();
  });
});
