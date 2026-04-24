import { gmailTransforms } from './gmail.js';
import { googleCalendarTransforms } from './google-calendar.js';

/**
 * A transform rewrites friendly, developer-facing tool params into the shape
 * the provider's HTTP API actually expects. It returns any combination of:
 *   - `body` — JSON body for POST/PUT/PATCH
 *   - `params` — query-string params
 *   - `headers` — additional request headers
 * The tool engine merges these onto the base request before proxying.
 */
export type TransformFunction = (params: Record<string, unknown>) => {
  body?: any;
  params?: Record<string, string>;
  headers?: Record<string, string>;
};

const registry: Record<string, TransformFunction> = {
  ...gmailTransforms,
  ...googleCalendarTransforms,
};

/**
 * Returns the transform function for a given id (`"{provider}:{name}"`), or
 * `null` when no transform is registered. The tool engine treats `null` as
 * "no transform configured — pass params straight through to the API."
 *
 * Lookup is case-sensitive — toolkit YAML references must match the
 * registered key exactly.
 */
export function getTransform(transformId: string): TransformFunction | null {
  return registry[transformId] ?? null;
}
