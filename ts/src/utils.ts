/**
 * Extracts a value from a nested object by a dot-delimited path.
 * Returns `undefined` when any segment in the path is missing or non-object.
 * Used for `token_response_mapping` and `cursor_response_field` lookups.
 */
export function getByPath(obj: unknown, path: string): unknown {
  if (obj == null || typeof obj !== 'object') return undefined;
  let cur: unknown = obj;
  for (const seg of path.split('.')) {
    if (cur == null || typeof cur !== 'object') return undefined;
    cur = (cur as Record<string, unknown>)[seg];
  }
  return cur;
}
