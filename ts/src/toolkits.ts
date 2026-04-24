import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse as parseYaml } from 'yaml';
import { AdditionError, ToolNotFoundError } from './errors.js';
import type {
  ProviderRegistry,
  ToolDefinition,
  ToolParamDefinition,
  ToolParamType,
  ToolkitConfig,
  ToolkitRegistry,
  ToolPaginationConfig,
  ToolResponseConfig,
} from './types.js';

const VALID_PARAM_TYPES: ToolParamType[] = [
  'string',
  'integer',
  'boolean',
  'string[]',
  'object',
];

const VALID_HTTP_METHODS = new Set([
  'GET',
  'POST',
  'PUT',
  'PATCH',
  'DELETE',
]);

interface RawParam {
  name?: unknown;
  type?: unknown;
  required?: unknown;
  default?: unknown;
  enum?: unknown;
  description?: unknown;
}

interface RawTool {
  id?: unknown;
  name?: unknown;
  description?: unknown;
  operation?: unknown;
  params?: unknown;
  transform?: unknown;
  pagination?: unknown;
  response?: unknown;
}

interface RawToolkit {
  provider?: unknown;
  name?: unknown;
  description?: unknown;
  tools?: unknown;
}

function requireString(value: unknown, path: string, context: string): string {
  if (typeof value !== 'string' || value === '') {
    throw new AdditionError(`${context}: missing required string field '${path}'`);
  }
  return value;
}

function parseParam(raw: RawParam, context: string): ToolParamDefinition {
  const name = requireString(raw.name, 'name', context);
  const typeStr = requireString(raw.type, 'type', `${context} param '${name}'`);
  if (!VALID_PARAM_TYPES.includes(typeStr as ToolParamType)) {
    throw new AdditionError(
      `${context} param '${name}': invalid type '${typeStr}' (must be one of ${VALID_PARAM_TYPES.join(', ')})`,
    );
  }
  if (typeof raw.required !== 'boolean') {
    throw new AdditionError(`${context} param '${name}': 'required' must be a boolean`);
  }
  const description = requireString(
    raw.description,
    'description',
    `${context} param '${name}'`,
  );
  const out: ToolParamDefinition = {
    name,
    type: typeStr as ToolParamType,
    required: raw.required,
    description,
  };
  if (raw.default !== undefined) out.default = raw.default;
  if (raw.enum !== undefined) {
    if (!Array.isArray(raw.enum)) {
      throw new AdditionError(`${context} param '${name}': 'enum' must be an array`);
    }
    // enum values must be assignable to the declared type — otherwise every
    // call is doomed to fail validation and the author never finds out until
    // someone invokes the tool.
    const expected = out.type;
    const okEnumType: Record<string, (v: unknown) => boolean> = {
      string: (v) => typeof v === 'string',
      integer: (v) => typeof v === 'number' && Number.isInteger(v),
    };
    const check = okEnumType[expected];
    if (!check) {
      throw new AdditionError(
        `${context} param '${name}': 'enum' is only allowed on 'string' or 'integer' params, not '${expected}'`,
      );
    }
    const bad = raw.enum.find((v) => !check(v));
    if (bad !== undefined) {
      throw new AdditionError(
        `${context} param '${name}': enum value ${JSON.stringify(bad)} is not a ${expected}`,
      );
    }
    out.enum = raw.enum as (string | number)[];
  }
  return out;
}

function parseOperation(op: string, context: string): void {
  const [method] = op.split(' ', 1);
  if (!VALID_HTTP_METHODS.has(method)) {
    throw new AdditionError(
      `${context}: operation '${op}' must start with a valid HTTP method (${[...VALID_HTTP_METHODS].join('|')})`,
    );
  }
  if (!op.includes(' ') || op.split(' ').length < 2 || !op.split(' ')[1].startsWith('/')) {
    throw new AdditionError(
      `${context}: operation '${op}' must be of the form 'METHOD /path'`,
    );
  }
}

function parsePagination(raw: unknown, context: string): ToolPaginationConfig | null {
  if (raw === undefined || raw === null) return null;
  if (typeof raw !== 'object') {
    throw new AdditionError(`${context}: 'pagination' must be an object`);
  }
  const p = raw as Record<string, unknown>;
  const type = requireString(p.type, 'pagination.type', context);
  if (type !== 'cursor' && type !== 'offset' && type !== 'page') {
    throw new AdditionError(
      `${context}: pagination.type '${type}' must be cursor, offset, or page`,
    );
  }
  const cursor_param = requireString(p.cursor_param, 'pagination.cursor_param', context);
  const cursor_response_field =
    p.cursor_response_field === null || p.cursor_response_field === undefined
      ? null
      : String(p.cursor_response_field);
  return { type, cursor_param, cursor_response_field };
}

function parseResponse(raw: unknown): ToolResponseConfig | null {
  if (raw === undefined || raw === null) return null;
  if (typeof raw !== 'object') return null;
  const r = raw as Record<string, unknown>;
  const type = r.type === 'array' ? 'array' : 'object';
  const fields = (r.fields && typeof r.fields === 'object' ? r.fields : {}) as Record<
    string,
    string
  >;
  return { type, fields };
}

function parseTool(raw: RawTool, providerId: string, idx: number): ToolDefinition {
  const baseCtx = `toolkit '${providerId}' tool [${idx}]`;
  const id = requireString(raw.id, 'id', baseCtx);
  const ctx = `toolkit '${providerId}' tool '${id}'`;
  const name = requireString(raw.name, 'name', ctx);
  const description = requireString(raw.description, 'description', ctx);
  const operation = requireString(raw.operation, 'operation', ctx);
  parseOperation(operation, ctx);

  if (!Array.isArray(raw.params)) {
    throw new AdditionError(`${ctx}: 'params' must be an array (use [] for no params)`);
  }
  const params = raw.params.map((p) => parseParam(p as RawParam, ctx));

  let transform: string | null = null;
  if (raw.transform !== undefined && raw.transform !== null) {
    if (typeof raw.transform !== 'string' || !raw.transform.includes(':')) {
      throw new AdditionError(
        `${ctx}: 'transform' must be of the form '{provider}:{transform_name}' or null`,
      );
    }
    transform = raw.transform;
  }

  return {
    id,
    name,
    description,
    provider: providerId,
    operation,
    params,
    transform,
    pagination: parsePagination(raw.pagination, ctx),
    response: parseResponse(raw.response),
  };
}

function parseToolkit(raw: RawToolkit, filename: string): ToolkitConfig {
  const ctx = `toolkit ${filename}`;
  const provider = requireString(raw.provider, 'provider', ctx);
  const name = requireString(raw.name, 'name', ctx);
  const description = requireString(raw.description, 'description', ctx);
  if (!Array.isArray(raw.tools)) {
    throw new AdditionError(`${ctx}: 'tools' must be an array`);
  }
  const tools = raw.tools.map((t, i) => parseTool(t as RawTool, provider, i));
  return { provider, name, description, tools };
}

/**
 * Loads every `*.yaml` file in `dir` (skipping `_`-prefixed files) and
 * returns a registry keyed by the toolkit's `provider` id. Each toolkit is
 * cross-checked against `providers`: referencing an unknown provider is a
 * load-time error.
 *
 * Like `loadProviders`, this function is intentionally **synchronous** —
 * call once at startup and reuse.
 */
export function loadToolkits(
  dir: string,
  providers: ProviderRegistry,
): ToolkitRegistry {
  const files = readdirSync(dir).filter(
    (f) => (f.endsWith('.yaml') || f.endsWith('.yml')) && !f.startsWith('_'),
  );
  const registry: ToolkitRegistry = {};
  for (const f of files) {
    const raw = parseYaml(readFileSync(join(dir, f), 'utf8')) as RawToolkit;
    const cfg = parseToolkit(raw, f);
    if (!providers[cfg.provider]) {
      throw new AdditionError(
        `toolkit ${f} references provider '${cfg.provider}' which is not defined in the provider registry`,
      );
    }
    if (registry[cfg.provider]) {
      throw new AdditionError(
        `duplicate toolkit for provider '${cfg.provider}' (${f})`,
      );
    }
    registry[cfg.provider] = cfg;
  }
  return registry;
}

export function getToolDefinitions(
  registry: ToolkitRegistry,
  providerId: string,
): ToolDefinition[] {
  const toolkit = registry[providerId];
  if (!toolkit) throw new ToolNotFoundError(providerId);
  return toolkit.tools;
}

export function getToolDefinition(
  registry: ToolkitRegistry,
  providerId: string,
  toolId: string,
): ToolDefinition {
  const toolkit = registry[providerId];
  if (!toolkit) throw new ToolNotFoundError(providerId);
  const tool = toolkit.tools.find((t) => t.id === toolId);
  if (!tool) throw new ToolNotFoundError(providerId, toolId);
  return tool;
}
