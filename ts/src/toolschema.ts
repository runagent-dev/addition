import { AdditionError } from './errors.js';
import type { ToolDefinition, ToolParamDefinition, ToolkitRegistry } from './types.js';

/**
 * Tool-schema projection (M14). Pure mappers from the shared toolkit catalog to
 * JSON-Schema input contracts and MCP tool descriptors. Both the REST endpoint
 * generator and the MCP server build on these — no projection re-derives an
 * input schema on its own.
 */

export interface JsonSchema {
  type: string;
  description?: string;
  enum?: (string | number)[];
  default?: unknown;
  items?: JsonSchema;
  properties?: Record<string, JsonSchema>;
  required?: string[];
  additionalProperties?: boolean;
}

export interface McpTool {
  name: string;
  description: string;
  inputSchema: JsonSchema;
}

export interface CatalogEntry {
  provider: string;
  toolId: string;
  mcpName: string;
  description: string;
  inputSchema: JsonSchema;
  method: string;
  path: string;
}

const MCP_NAME_SEP = '__';

/** MCP tool name for a (provider, tool) pair. Reversible when neither contains `__`. */
export function mcpToolName(provider: string, toolId: string): string {
  return `${provider}${MCP_NAME_SEP}${toolId}`;
}

/** A fresh base schema per call — never share nested objects (e.g. `items`) across params. */
function baseSchema(type: ToolParamDefinition['type']): JsonSchema {
  switch (type) {
    case 'string': return { type: 'string' };
    case 'integer': return { type: 'integer' };
    case 'boolean': return { type: 'boolean' };
    case 'string[]': return { type: 'array', items: { type: 'string' } };
    case 'object': return { type: 'object' };
  }
}

function paramToSchema(param: ToolParamDefinition): JsonSchema {
  const schema = baseSchema(param.type);
  schema.description = param.description;
  // Never alias the input catalog into the output — copy the enum array and
  // deep-copy the default (which may be an array/object) so the projection is pure.
  if (param.enum !== undefined) schema.enum = [...param.enum];
  if (param.default !== undefined) schema.default = structuredClone(param.default);
  return schema;
}

/**
 * JSON Schema describing a tool's caller inputs. Every declared param appears —
 * including path params interpolated into the URL at execution — because the
 * caller has to supply them.
 */
export function paramsToJsonSchema(params: ToolParamDefinition[]): JsonSchema {
  const properties: Record<string, JsonSchema> = {};
  const required: string[] = [];
  for (const param of params) {
    properties[param.name] = paramToSchema(param);
    if (param.required) required.push(param.name);
  }
  return { type: 'object', properties, required, additionalProperties: false };
}

/** MCP tool descriptor for a tool: name, description, and JSON-Schema inputs. */
export function toMcpTool(provider: string, tool: ToolDefinition): McpTool {
  return {
    name: mcpToolName(provider, tool.id),
    description: tool.description,
    inputSchema: paramsToJsonSchema(tool.params),
  };
}

function parseOperation(operation: string): { method: string; path: string } {
  const spaceIdx = operation.indexOf(' ');
  if (spaceIdx === -1) {
    throw new AdditionError(`Malformed tool operation '${operation}': expected "METHOD /path"`);
  }
  return {
    method: operation.slice(0, spaceIdx),
    path: operation.slice(spaceIdx + 1),
  };
}

/** Flat, normalized view of every tool across every toolkit — the projection base. */
export function toolCatalog(registry: ToolkitRegistry): CatalogEntry[] {
  const entries: CatalogEntry[] = [];
  for (const toolkit of Object.values(registry)) {
    for (const tool of toolkit.tools) {
      const { method, path } = parseOperation(tool.operation);
      entries.push({
        provider: toolkit.provider,
        toolId: tool.id,
        mcpName: mcpToolName(toolkit.provider, tool.id),
        description: tool.description,
        inputSchema: paramsToJsonSchema(tool.params),
        method,
        path,
      });
    }
  }
  return entries;
}
