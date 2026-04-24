import { DefaultAdapter } from './base.js';
import { GitHubAdapter } from './github.js';
import { SlackAdapter } from './slack.js';
import type { ProviderAdapter } from './base.js';

export { DefaultAdapter, GitHubAdapter, SlackAdapter };
export type { ProviderAdapter };

/**
 * Adapter singletons. One instance per provider id — adapters are stateless,
 * so sharing is safe and avoids per-request allocation.
 */
const adapters: Record<string, ProviderAdapter> = {
  github: new GitHubAdapter(),
  slack: new SlackAdapter(),
};

const fallback: ProviderAdapter = new DefaultAdapter();

/**
 * Returns the adapter for a given provider id. Providers without behavioral
 * quirks (standard OAuth2 + standard HTTP error semantics) get the
 * `DefaultAdapter` — this includes `google`, `gmail`, `google-calendar`, and
 * any unknown provider id.
 */
export function getAdapter(providerId: string): ProviderAdapter {
  return adapters[providerId] ?? fallback;
}
