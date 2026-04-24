import { getToolDefinition } from './toolkits.js';
import { getTransform } from './transforms/index.js';
import { ToolValidationError, ToolNotFoundError } from './errors.js';
import type {
  ToolDefinition,
  ToolkitRegistry,
  ToolParamDefinition,
} from './types.js';

// ---------- Public types ----------

export interface ExecuteToolArgs {
  userId: string;
  providerId: string;
  toolId: string;
  params: Record<string, unknown>;
  connectionId?: string;
}

export interface ProxyRequest {
  userId: string;
  providerId: string;
  method: string;
  path: string;
  body?: unknown;
  params?: Record<string, string>;
  headers?: Record<string, string>;
  connectionId?: string;
}

export interface ProxyResponse {
  status: number;
  body: unknown;
}

export type ProxyFn = (req: ProxyRequest) => Promise<ProxyResponse>;

export interface ToolExecutionResult {
  success: boolean;
  data: unknown;
  status: number;
  error?: string;
}

export interface ToolDeps {
  toolkits: ToolkitRegistry;
  proxy: ProxyFn;
}

// ---------- Validation ----------

function validate(
  tool: ToolDefinition,
  input: Record<string, unknown>,
): Record<string, unknown> {
  const missing: string[] = [];
  const invalid: Array<{ name: string; reason: string }> = [];
  const resolved: Record<string, unknown> = {};

  for (const p of tool.params) {
    const hasValue = p.name in input && input[p.name] !== undefined;
    if (!hasValue) {
      if (p.required) missing.push(p.name);
      else if (p.default !== undefined) resolved[p.name] = p.default;
      continue;
    }
    const value = input[p.name];
    const typeErr = checkType(p, value);
    if (typeErr) {
      invalid.push({ name: p.name, reason: typeErr });
      continue;
    }
    if (p.enum && !p.enum.includes(value as string | number)) {
      invalid.push({
        name: p.name,
        reason: `must be one of ${p.enum.join(', ')}`,
      });
      continue;
    }
    resolved[p.name] = value;
  }

  if (missing.length || invalid.length) {
    throw new ToolValidationError(tool.id, missing, invalid);
  }
  return resolved;
}

function checkType(p: ToolParamDefinition, value: unknown): string | null {
  switch (p.type) {
    case 'string':
      return typeof value === 'string' ? null : `must be a string`;
    case 'integer':
      return Number.isInteger(value) ? null : `must be an integer`;
    case 'boolean':
      return typeof value === 'boolean' ? null : `must be a boolean`;
    case 'string[]':
      if (!Array.isArray(value)) return 'must be an array of strings';
      return value.every((v) => typeof v === 'string')
        ? null
        : 'must be an array of strings';
    case 'object':
      return value && typeof value === 'object' && !Array.isArray(value)
        ? null
        : 'must be an object';
    default:
      return `unknown param type`;
  }
}

// ---------- Path interpolation ----------

interface Interpolated {
  method: string;
  path: string;
  /** Params left after stripping out those consumed by `{...}` placeholders. */
  remainingParams: Record<string, unknown>;
}

function interpolate(
  tool: ToolDefinition,
  params: Record<string, unknown>,
): Interpolated {
  const spaceIdx = tool.operation.indexOf(' ');
  const method = tool.operation.slice(0, spaceIdx);
  const template = tool.operation.slice(spaceIdx + 1);
  const remaining: Record<string, unknown> = { ...params };
  const missingPathParams: string[] = [];
  const path = template.replace(/\{([^}]+)\}/g, (_, name) => {
    if (!(name in remaining) || remaining[name] === undefined || remaining[name] === null) {
      missingPathParams.push(name);
      return '';
    }
    const value = String(remaining[name]);
    delete remaining[name];
    return encodeURIComponent(value);
  });
  if (missingPathParams.length) {
    throw new ToolValidationError(tool.id, missingPathParams, []);
  }
  return { method, path, remainingParams: remaining };
}

// ---------- Query stringification ----------

function stringifyQuery(params: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null) continue;
    if (Array.isArray(v)) {
      if (v.length === 0) continue;
      out[k] = v.join(',');
    } else if (typeof v === 'object') {
      out[k] = JSON.stringify(v);
    } else {
      out[k] = String(v);
    }
  }
  return out;
}

// ---------- Main ----------

const METHODS_WITH_QUERY = new Set(['GET', 'DELETE']);

export async function executeTool(
  args: ExecuteToolArgs,
  deps: ToolDeps,
): Promise<ToolExecutionResult> {
  const { toolkits, proxy } = deps;

  // 1. Find tool (ToolNotFoundError for unknown provider/tool)
  const tool = getToolDefinition(toolkits, args.providerId, args.toolId);

  // 2. Validate + apply defaults
  const validated = validate(tool, args.params);

  // 3. Interpolate path — consumes path params out of `validated`.
  const { method, path, remainingParams } = interpolate(tool, validated);

  // 4. Transform or default param placement.
  let body: unknown | undefined;
  let query: Record<string, string> | undefined;
  const headers: Record<string, string> = {};

  if (tool.transform) {
    const transform = getTransform(tool.transform);
    if (!transform) {
      throw new ToolValidationError(tool.id, [], [
        { name: 'transform', reason: `unknown transform '${tool.transform}'` },
      ]);
    }
    const out = transform(remainingParams);
    if (out.body != null) body = out.body;
    if (out.params) query = out.params;
    if (out.headers) Object.assign(headers, out.headers);
  } else if (METHODS_WITH_QUERY.has(method)) {
    query = stringifyQuery(remainingParams);
  } else {
    body = remainingParams;
  }

  // 5. Dispatch via injected proxy.
  const req: ProxyRequest = {
    userId: args.userId,
    providerId: args.providerId,
    method,
    path,
  };
  if (body !== undefined) req.body = body;
  if (query !== undefined) req.params = query;
  if (Object.keys(headers).length) req.headers = headers;
  if (args.connectionId !== undefined) req.connectionId = args.connectionId;

  const response = await proxy(req);
  return {
    success: response.status >= 200 && response.status < 300,
    data: response.body,
    status: response.status,
  };
}
