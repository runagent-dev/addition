/**
 * M14 — Tool-schema projection (pure).
 *
 * Turns the shared toolkit catalog into JSON-Schema input contracts and MCP
 * tool descriptors. The golden fixture shared/test-fixtures/tool_schema_export.yaml
 * pins the exact output so the TS and Python SDKs project identically.
 */
import { beforeAll, describe, expect, it } from 'vitest';
import { join } from 'node:path';
import { readFileSync } from 'node:fs';
import { parse as parseYaml } from 'yaml';
import {
  paramsToJsonSchema,
  toMcpTool,
  toolCatalog,
  mcpToolName,
  type JsonSchema,
} from '../../src/toolschema.js';
import { loadToolkits, getToolDefinition } from '../../src/toolkits.js';
import { loadProviders } from '../../src/providers.js';
import { AdditionError } from '../../src/errors.js';
import type { ProviderRegistry, ToolkitRegistry, ToolParamDefinition } from '../../src/types.js';

const SHARED = join(__dirname, '..', '..', '..', 'shared');
const SHARED_TOOLKITS = join(SHARED, 'toolkits');
const SHARED_PROVIDERS = join(SHARED, 'providers');
const FIXTURE = join(SHARED, 'test-fixtures', 'tool_schema_export.yaml');

interface GoldenEntry {
  provider: string;
  toolId: string;
  name: string;
  description: string;
  method: string;
  path: string;
  inputSchema: JsonSchema;
}

let providers: ProviderRegistry;
let toolkits: ToolkitRegistry;
let golden: GoldenEntry[];

beforeAll(() => {
  providers = loadProviders(SHARED_PROVIDERS);
  toolkits = loadToolkits(SHARED_TOOLKITS, providers);
  golden = (parseYaml(readFileSync(FIXTURE, 'utf8')) as { mcp_tools: GoldenEntry[] }).mcp_tools;
});

describe('paramsToJsonSchema', () => {
  it('empty params → empty object schema', () => {
    expect(paramsToJsonSchema([])).toEqual({
      type: 'object',
      properties: {},
      required: [],
      additionalProperties: false,
    });
  });

  it('maps each param type', () => {
    const params: ToolParamDefinition[] = [
      { name: 's', type: 'string', required: false, description: 'a string' },
      { name: 'i', type: 'integer', required: false, description: 'an int' },
      { name: 'b', type: 'boolean', required: false, description: 'a bool' },
      { name: 'arr', type: 'string[]', required: false, description: 'a list' },
      { name: 'o', type: 'object', required: false, description: 'an object' },
    ];
    const schema = paramsToJsonSchema(params);
    expect(schema.properties).toEqual({
      s: { type: 'string', description: 'a string' },
      i: { type: 'integer', description: 'an int' },
      b: { type: 'boolean', description: 'a bool' },
      arr: { type: 'array', items: { type: 'string' }, description: 'a list' },
      o: { type: 'object', description: 'an object' },
    });
  });

  it('copies enum and default when present, omits them otherwise', () => {
    const params: ToolParamDefinition[] = [
      { name: 'plain', type: 'string', required: false, description: 'no extras' },
      { name: 'mode', type: 'string', required: false, description: 'with extras', enum: ['a', 'b'], default: 'a' },
    ];
    const schema = paramsToJsonSchema(params);
    expect(schema.properties!.plain).toEqual({ type: 'string', description: 'no extras' });
    expect(schema.properties!.mode).toEqual({
      type: 'string',
      description: 'with extras',
      enum: ['a', 'b'],
      default: 'a',
    });
  });

  it('lists only required params, in declaration order', () => {
    const params: ToolParamDefinition[] = [
      { name: 'first', type: 'string', required: true, description: '' },
      { name: 'skip', type: 'string', required: false, description: '' },
      { name: 'second', type: 'string', required: true, description: '' },
    ];
    expect(paramsToJsonSchema(params).required).toEqual(['first', 'second']);
  });

  it('includes path params in the schema (caller must supply them)', () => {
    const tool = getToolDefinition(toolkits, 'gmail', 'get_message');
    const schema = paramsToJsonSchema(tool.params);
    // messageId is interpolated into the URL at execution but is still a caller input.
    expect(schema.properties!.messageId).toEqual({
      type: 'string',
      description: 'The ID of the message to retrieve',
    });
    expect(schema.required).toContain('messageId');
  });
});

describe('mcpToolName', () => {
  it('joins provider and tool id with a double underscore', () => {
    expect(mcpToolName('gmail', 'send_message')).toBe('gmail__send_message');
  });

  it('is reversible for ids without a double underscore', () => {
    const name = mcpToolName('github', 'create_issue');
    const [provider, toolId] = name.split('__');
    expect([provider, toolId]).toEqual(['github', 'create_issue']);
  });
});

describe('toMcpTool', () => {
  it('produces name, description, and inputSchema', () => {
    const tool = getToolDefinition(toolkits, 'gmail', 'send_message');
    const mcp = toMcpTool('gmail', tool);
    expect(mcp.name).toBe('gmail__send_message');
    expect(mcp.description).toBe(tool.description);
    expect(mcp.inputSchema.type).toBe('object');
    expect(mcp.inputSchema.required).toEqual(['to', 'subject', 'body']);
  });
});

describe('purity — no aliasing of the input catalog (round-1 review)', () => {
  it('does not alias the input enum array (mutating output leaves the catalog intact)', () => {
    const tool = getToolDefinition(toolkits, 'gmail', 'get_message');
    const formatParam = tool.params.find((p) => p.name === 'format')!;
    const originalEnum = [...(formatParam.enum as string[])];

    const schema = paramsToJsonSchema(tool.params);
    (schema.properties!.format.enum as unknown[]).push('MUTATED');

    expect(formatParam.enum).toEqual(originalEnum);
  });

  it('does not alias array/object default values (round-2 review)', () => {
    const params: ToolParamDefinition[] = [
      { name: 'tags', type: 'string[]', required: false, description: '', default: ['a', 'b'] },
    ];
    const schema = paramsToJsonSchema(params);
    (schema.properties!.tags.default as unknown[]).push('MUTATED');

    expect(params[0].default).toEqual(['a', 'b']);
  });
});

describe('parseOperation guard (round-1 review)', () => {
  it('toolCatalog throws AdditionError on an operation with no METHOD/path space', () => {
    const badRegistry: ToolkitRegistry = {
      x: {
        provider: 'x',
        name: 'X',
        description: '',
        tools: [
          {
            id: 't',
            name: 'T',
            description: '',
            provider: 'x',
            operation: 'GETnoSpace',
            params: [],
            transform: null,
            pagination: null,
            response: null,
          },
        ],
      },
    };
    expect(() => toolCatalog(badRegistry)).toThrow(AdditionError);
  });
});

describe('golden fixture — tool_schema_export.yaml', () => {
  it('has entries', () => {
    expect(golden.length).toBeGreaterThan(0);
  });

  it('toMcpTool matches the golden descriptor for each tool', () => {
    for (const e of golden) {
      const tool = getToolDefinition(toolkits, e.provider, e.toolId);
      expect(toMcpTool(e.provider, tool)).toEqual({
        name: e.name,
        description: e.description,
        inputSchema: e.inputSchema,
      });
    }
  });

  it('toolCatalog exposes a matching entry (with method/path) for each tool', () => {
    const catalog = toolCatalog(toolkits);
    for (const e of golden) {
      const entry = catalog.find((c) => c.mcpName === e.name);
      expect(entry, `catalog entry for ${e.name}`).toBeDefined();
      expect(entry).toEqual({
        provider: e.provider,
        toolId: e.toolId,
        mcpName: e.name,
        description: e.description,
        inputSchema: e.inputSchema,
        method: e.method,
        path: e.path,
      });
    }
  });

  it('toolCatalog covers every tool in every toolkit', () => {
    const total = Object.values(toolkits).reduce((n, tk) => n + tk.tools.length, 0);
    expect(toolCatalog(toolkits)).toHaveLength(total);
  });
});
