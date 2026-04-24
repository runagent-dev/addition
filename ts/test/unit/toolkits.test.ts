import { beforeAll, describe, expect, it } from 'vitest';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { loadToolkits, getToolDefinitions, getToolDefinition } from '../../src/toolkits.js';
import { loadProviders } from '../../src/providers.js';
import { ToolNotFoundError, AdditionError } from '../../src/errors.js';
import type { ToolkitRegistry, ProviderRegistry } from '../../src/types.js';

const SHARED_TOOLKITS = join(__dirname, '..', '..', '..', 'shared', 'toolkits');
const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');

describe('toolkits — YAML toolkit registry', () => {
  let providers: ProviderRegistry;
  let toolkits: ToolkitRegistry;

  beforeAll(() => {
    providers = loadProviders(SHARED_PROVIDERS);
    toolkits = loadToolkits(SHARED_TOOLKITS, providers);
  });

  describe('loading', () => {
    it('loads all 4 toolkits', () => {
      const providerIds = Object.keys(toolkits).sort();
      expect(providerIds).toEqual(['github', 'gmail', 'google-calendar', 'slack']);
    });

    it('ignores the _template.yaml file', () => {
      const ids = Object.keys(toolkits);
      expect(ids).not.toContain('');
      expect(ids).not.toContain('_template');
      expect(ids).toHaveLength(4);
    });

    it('toolkit metadata is loaded (name, description)', () => {
      expect(toolkits.gmail.name).toBe('Gmail');
      expect(toolkits.gmail.description).toMatch(/email/i);
      expect(toolkits.github.name).toBe('GitHub');
    });
  });

  describe('getToolDefinitions', () => {
    it('gmail has 5 tools', () => {
      const tools = getToolDefinitions(toolkits, 'gmail');
      expect(tools).toHaveLength(5);
      const ids = tools.map((t) => t.id).sort();
      expect(ids).toEqual([
        'get_message',
        'list_messages',
        'modify_message',
        'search_messages',
        'send_message',
      ]);
    });

    it('google-calendar has 5 tools', () => {
      const tools = getToolDefinitions(toolkits, 'google-calendar');
      expect(tools).toHaveLength(5);
      const ids = tools.map((t) => t.id).sort();
      expect(ids).toEqual([
        'create_event',
        'delete_event',
        'get_event',
        'list_events',
        'update_event',
      ]);
    });

    it('github has 5 tools', () => {
      const tools = getToolDefinitions(toolkits, 'github');
      expect(tools).toHaveLength(5);
    });

    it('slack has 3 tools', () => {
      const tools = getToolDefinitions(toolkits, 'slack');
      expect(tools).toHaveLength(3);
    });

    it('unknown toolkit throws ToolNotFoundError', () => {
      expect(() => getToolDefinitions(toolkits, 'nonexistent')).toThrow(ToolNotFoundError);
    });

    it('every tool carries the provider id for back-reference', () => {
      const tools = getToolDefinitions(toolkits, 'gmail');
      for (const t of tools) expect(t.provider).toBe('gmail');
    });
  });

  describe('getToolDefinition', () => {
    it('returns a single tool by id', () => {
      const send = getToolDefinition(toolkits, 'gmail', 'send_message');
      expect(send.id).toBe('send_message');
      expect(send.name).toBe('Send email');
      expect(send.provider).toBe('gmail');
    });

    it('unknown provider throws ToolNotFoundError', () => {
      expect(() => getToolDefinition(toolkits, 'nope', 'anything')).toThrow(ToolNotFoundError);
    });

    it('unknown tool throws ToolNotFoundError', () => {
      expect(() => getToolDefinition(toolkits, 'gmail', 'bogus')).toThrow(ToolNotFoundError);
    });
  });

  describe('ToolDefinition shape', () => {
    it('send_message has correct param schema (types, required, descriptions)', () => {
      const send = getToolDefinition(toolkits, 'gmail', 'send_message');
      const byName = Object.fromEntries(send.params.map((p) => [p.name, p]));

      expect(byName.to.type).toBe('string');
      expect(byName.to.required).toBe(true);
      expect(byName.to.description).toMatch(/recipient/i);

      expect(byName.subject.required).toBe(true);
      expect(byName.body.required).toBe(true);

      expect(byName.cc.required).toBe(false);
      expect(byName.bcc.required).toBe(false);
    });

    it('get_message exposes enum values on the format param', () => {
      const get = getToolDefinition(toolkits, 'gmail', 'get_message');
      const format = get.params.find((p) => p.name === 'format');
      expect(format).toBeDefined();
      expect(format!.enum).toEqual(['minimal', 'full', 'raw', 'metadata']);
      expect(format!.default).toBe('full');
    });

    it('list_messages has maxResults integer with default 10 and optional string[] labelIds', () => {
      const list = getToolDefinition(toolkits, 'gmail', 'list_messages');
      const byName = Object.fromEntries(list.params.map((p) => [p.name, p]));
      expect(byName.maxResults.type).toBe('integer');
      expect(byName.maxResults.default).toBe(10);
      expect(byName.labelIds.type).toBe('string[]');
      expect(byName.labelIds.required).toBe(false);
    });

    it('operation field has METHOD and path', () => {
      const send = getToolDefinition(toolkits, 'gmail', 'send_message');
      expect(send.operation).toBe('POST /gmail/v1/users/me/messages/send');
      const list = getToolDefinition(toolkits, 'gmail', 'list_messages');
      expect(list.operation).toBe('GET /gmail/v1/users/me/messages');
    });

    it('transform field is populated when defined (gmail:rfc2822 for send_message)', () => {
      const send = getToolDefinition(toolkits, 'gmail', 'send_message');
      expect(send.transform).toBe('gmail:rfc2822');
    });

    it('transform field is null when undefined', () => {
      const list = getToolDefinition(toolkits, 'gmail', 'list_messages');
      expect(list.transform).toBeNull();
    });

    it('github create_issue has path params owner, repo and required title', () => {
      const ci = getToolDefinition(toolkits, 'github', 'create_issue');
      expect(ci.operation).toBe('POST /repos/{owner}/{repo}/issues');
      const byName = Object.fromEntries(ci.params.map((p) => [p.name, p]));
      expect(byName.owner.required).toBe(true);
      expect(byName.repo.required).toBe(true);
      expect(byName.title.required).toBe(true);
      expect(byName.body.required).toBe(false);
    });
  });

  describe('cross-reference to providers', () => {
    it('every toolkit provider id maps to a loaded provider', () => {
      for (const [id] of Object.entries(toolkits)) {
        expect(providers[id]).toBeDefined();
      }
    });

    it('rejects a toolkit whose provider id is not in the registry', () => {
      const tmp = mkdtempSync(join(tmpdir(), 'addition-toolkits-'));
      try {
        writeFileSync(
          join(tmp, 'phantom.yaml'),
          `provider: "phantom-provider-no-yaml"
name: "Phantom"
description: "No backing provider"
tools:
  - id: noop
    name: "Noop"
    description: "does nothing"
    operation: "GET /noop"
    params: []
`,
          'utf8',
        );
        expect(() => loadToolkits(tmp, providers)).toThrow(/phantom-provider-no-yaml/);
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });
  });

  describe('shape validation', () => {
    let tmp: string;
    const write = (f: string, c: string) => writeFileSync(join(tmp, f), c, 'utf8');
    const makeTmp = () => (tmp = mkdtempSync(join(tmpdir(), 'addition-toolkits-v-')));

    it('rejects a toolkit missing the provider field', () => {
      makeTmp();
      try {
        write(
          'bad.yaml',
          `name: "Bad"
description: "no provider"
tools: []
`,
        );
        expect(() => loadToolkits(tmp, providers)).toThrow(AdditionError);
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });

    it('rejects a tool missing required fields (id or operation)', () => {
      makeTmp();
      try {
        write(
          'bad.yaml',
          `provider: "gmail"
name: "Broken Gmail"
description: "missing operation"
tools:
  - id: foo
    name: "Foo"
    description: "x"
    params: []
`,
        );
        expect(() => loadToolkits(tmp, providers)).toThrow(/operation/i);
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });

    it('rejects a param with an unknown type', () => {
      makeTmp();
      try {
        write(
          'bad.yaml',
          `provider: "gmail"
name: "Bad"
description: "bad param type"
tools:
  - id: foo
    name: "Foo"
    description: "x"
    operation: "GET /foo"
    params:
      - name: q
        type: blobstring
        required: true
        description: "bad"
`,
        );
        expect(() => loadToolkits(tmp, providers)).toThrow(/blobstring|type/i);
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });
  });
});
