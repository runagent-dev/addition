import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse as parseYaml } from 'yaml';
import { ProviderNotFoundError, AdditionError } from './errors.js';
import type { ProviderConfig, ProviderRegistry } from './types.js';

interface RawProvider extends Partial<ProviderConfig> {
  id: string;
  extends?: string;
}

function readRawProviders(dir: string): RawProvider[] {
  const files = readdirSync(dir).filter(
    (f) => (f.endsWith('.yaml') || f.endsWith('.yml')) && !f.startsWith('_'),
  );
  return files.map((f) => {
    const raw = parseYaml(readFileSync(join(dir, f), 'utf8')) as RawProvider;
    if (!raw || typeof raw !== 'object' || typeof raw.id !== 'string' || raw.id === '') {
      throw new AdditionError(`provider file ${f} is missing a non-empty 'id'`);
    }
    return raw;
  });
}

function topoSort(raws: RawProvider[]): RawProvider[] {
  const byId = new Map<string, RawProvider>();
  for (const r of raws) byId.set(r.id, r);

  const sorted: RawProvider[] = [];
  const state = new Map<string, 'visiting' | 'done'>();

  function visit(id: string, stack: string[]) {
    const mark = state.get(id);
    if (mark === 'done') return;
    if (mark === 'visiting') {
      throw new AdditionError(
        `extends cycle detected: ${[...stack, id].join(' -> ')}`,
      );
    }
    const node = byId.get(id);
    if (!node) {
      throw new AdditionError(
        `provider '${id}' extends unknown provider (missing YAML)`,
      );
    }
    state.set(id, 'visiting');
    if (node.extends) {
      if (!byId.has(node.extends)) {
        throw new AdditionError(
          `provider '${id}' extends unknown provider '${node.extends}'`,
        );
      }
      visit(node.extends, [...stack, id]);
    }
    state.set(id, 'done');
    sorted.push(node);
  }

  for (const r of raws) visit(r.id, []);
  return sorted;
}

/**
 * Deep-merge child config onto base. Rules (spec §6):
 * - scalars: child overrides base (including `null` — a child's explicit
 *   `null` overwrites the base's value. Used e.g. to say "this provider
 *   has no revoke_url even though the base does")
 * - plain objects: recurse
 * - arrays: child REPLACES base (no concatenation)
 * - id and name always come from child (enforced in `mergeConfigs`)
 */
function deepMerge(
  base: Record<string, unknown>,
  child: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...base };
  for (const [key, childVal] of Object.entries(child)) {
    if (key === 'extends') continue;
    const baseVal = base[key];
    if (
      baseVal &&
      typeof baseVal === 'object' &&
      !Array.isArray(baseVal) &&
      childVal &&
      typeof childVal === 'object' &&
      !Array.isArray(childVal)
    ) {
      out[key] = deepMerge(
        baseVal as Record<string, unknown>,
        childVal as Record<string, unknown>,
      );
    } else {
      out[key] = childVal;
    }
  }
  return out;
}

function mergeConfigs(base: RawProvider, child: RawProvider): RawProvider {
  const merged = deepMerge(
    base as unknown as Record<string, unknown>,
    child as unknown as Record<string, unknown>,
  );
  // id and name always come from child
  merged.id = child.id;
  if (child.name !== undefined) merged.name = child.name;
  return merged as unknown as RawProvider;
}

/**
 * Validates that a resolved provider config has all required string fields
 * populated. Catches YAML typos at load time rather than as `undefined`
 * runtime values deep inside the OAuth engine.
 */
function validateResolved(cfg: ProviderConfig): void {
  const id = cfg.id;
  const requireNonEmptyString = (path: string, value: unknown) => {
    if (typeof value !== 'string' || value === '') {
      throw new AdditionError(
        `provider '${id}' is missing required string field '${path}' after extends resolution`,
      );
    }
  };
  requireNonEmptyString('name', cfg.name);
  requireNonEmptyString('auth_type', cfg.auth_type);
  if (cfg.auth_type === 'oauth2') {
    if (!cfg.oauth2) {
      throw new AdditionError(`provider '${id}' has auth_type=oauth2 but no 'oauth2' block`);
    }
    requireNonEmptyString('oauth2.authorize_url', cfg.oauth2.authorize_url);
    requireNonEmptyString('oauth2.token_url', cfg.oauth2.token_url);
  }
  if (!cfg.api) {
    throw new AdditionError(`provider '${id}' has no 'api' block`);
  }
  requireNonEmptyString('api.base_url', cfg.api.base_url);
  requireNonEmptyString('api.auth_header', cfg.api.auth_header);
  requireNonEmptyString('api.auth_prefix', cfg.api.auth_prefix);
}

/**
 * Loads every `*.yaml` file in `dir` (skipping `_` -prefixed files), resolves
 * `extends` chains, and returns a typed provider registry.
 *
 * This function is intentionally **synchronous**: provider configs are static
 * data read once at application startup, not on a hot path. Do not call it
 * per-request. Call it once during `Addition` construction and reuse the
 * resulting registry for the process lifetime.
 */
export function loadProviders(dir: string): ProviderRegistry {
  const raws = readRawProviders(dir);
  const sorted = topoSort(raws);
  const resolved = new Map<string, ProviderConfig>();

  for (const node of sorted) {
    let merged: RawProvider = { ...node };
    if (node.extends) {
      const base = resolved.get(node.extends);
      if (!base) {
        // topoSort already guarantees this, but defense-in-depth
        throw new AdditionError(
          `provider '${node.id}' extends '${node.extends}' which was not resolved`,
        );
      }
      merged = mergeConfigs(base as unknown as RawProvider, node);
    }
    // strip 'extends' from the resolved config — it's a loader directive, not runtime state
    const { extends: _drop, ...final } = merged;
    const cfg = final as ProviderConfig;
    validateResolved(cfg);
    resolved.set(node.id, cfg);
  }

  const registry: ProviderRegistry = {};
  for (const [id, cfg] of resolved) registry[id] = cfg;
  return registry;
}

export function getProvider(
  registry: ProviderRegistry,
  providerId: string,
): ProviderConfig {
  const cfg = registry[providerId];
  if (!cfg) throw new ProviderNotFoundError(providerId);
  return cfg;
}
