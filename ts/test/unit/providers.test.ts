import { beforeAll, describe, expect, it } from 'vitest';
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { loadProviders, getProvider } from '../../src/providers.js';
import { ProviderNotFoundError } from '../../src/errors.js';
import type { ProviderRegistry } from '../../src/types.js';

const SHARED_PROVIDERS = join(__dirname, '..', '..', '..', 'shared', 'providers');

describe('providers — YAML registry', () => {
  let registry: ProviderRegistry;

  beforeAll(() => {
    registry = loadProviders(SHARED_PROVIDERS);
  });

  describe('loading', () => {
    it('loads all 5 providers from shared/providers/', () => {
      const ids = Object.keys(registry).sort();
      expect(ids).toEqual(['github', 'gmail', 'google', 'google-calendar', 'slack']);
    });

    it('ignores the _template.yaml file', () => {
      const ids = Object.keys(registry);
      expect(ids).not.toContain('');
      expect(ids).not.toContain('_template');
      expect(ids).toHaveLength(5);
    });

    it('getProvider returns the parsed config for a known id', () => {
      const google = getProvider(registry, 'google');
      expect(google.id).toBe('google');
      expect(google.name).toBe('Google');
      expect(google.auth_type).toBe('oauth2');
      expect(google.oauth2.authorize_url).toBe('https://accounts.google.com/o/oauth2/v2/auth');
    });

    it('getProvider throws ProviderNotFoundError for an unknown id', () => {
      expect(() => getProvider(registry, 'nonexistent')).toThrow(ProviderNotFoundError);
    });
  });

  describe('extends resolution', () => {
    it('gmail inherits google authorize_url and token_url', () => {
      const gmail = getProvider(registry, 'gmail');
      const google = getProvider(registry, 'google');
      expect(gmail.oauth2.authorize_url).toBe(google.oauth2.authorize_url);
      expect(gmail.oauth2.token_url).toBe(google.oauth2.token_url);
    });

    it('gmail inherits google scopes_delimiter and authorize_params', () => {
      const gmail = getProvider(registry, 'gmail');
      expect(gmail.oauth2.scopes_delimiter).toBe('space');
      expect(gmail.oauth2.authorize_params).toEqual({ access_type: 'offline', prompt: 'consent' });
    });

    it('array override: gmail.default_scopes REPLACES google.default_scopes (not merges)', () => {
      const gmail = getProvider(registry, 'gmail');
      const google = getProvider(registry, 'google');
      // gmail's scopes include gmail-specific URLs; must NOT contain google's profile scope
      // unless gmail explicitly re-lists it
      expect(gmail.oauth2.default_scopes).toContain(
        'https://www.googleapis.com/auth/gmail.send',
      );
      // Google's profile scope is not in gmail.yaml → must not appear via merge
      expect(gmail.oauth2.default_scopes).not.toContain(
        'https://www.googleapis.com/auth/userinfo.profile',
      );
      // Length comes from child definition alone
      expect(gmail.oauth2.default_scopes.length).toBeLessThanOrEqual(
        google.oauth2.default_scopes.length + 10,
      );
    });

    it('scalar override: gmail.api.base_url overrides google.api.base_url', () => {
      const gmail = getProvider(registry, 'gmail');
      expect(gmail.api.base_url).toBe('https://gmail.googleapis.com');
    });

    it('child inherits unspecified nested fields (api.auth_header from google)', () => {
      const gmail = getProvider(registry, 'gmail');
      expect(gmail.api.auth_header).toBe('Authorization');
      expect(gmail.api.auth_prefix).toBe('Bearer');
    });

    it('id and name always come from the child', () => {
      const gmail = getProvider(registry, 'gmail');
      expect(gmail.id).toBe('gmail');
      expect(gmail.name).toBe('Gmail');
    });

    it('google-calendar inherits google OAuth but overrides base_url', () => {
      const cal = getProvider(registry, 'google-calendar');
      expect(cal.id).toBe('google-calendar');
      expect(cal.oauth2.authorize_url).toBe('https://accounts.google.com/o/oauth2/v2/auth');
      expect(cal.api.base_url).toBe('https://www.googleapis.com/calendar/v3');
    });

    it('providers without extends are returned as-is', () => {
      const github = getProvider(registry, 'github');
      expect(github.oauth2.refresh_supported).toBe(false);
      expect(github.oauth2.default_scopes).toEqual(['repo', 'read:user', 'user:email']);
    });
  });

  describe('synthetic fixtures — topology and cycles', () => {
    let tmp: string;

    function writeYaml(dir: string, filename: string, content: string) {
      writeFileSync(join(dir, filename), content, 'utf8');
    }

    function makeTmp(): string {
      const t = mkdtempSync(join(tmpdir(), 'addition-providers-'));
      return t;
    }

    it('topological sort: loads children regardless of filesystem order', () => {
      // Load child-first-alphabetically by naming: aaa extends zzz.
      tmp = makeTmp();
      try {
        writeYaml(
          tmp,
          'aaa.yaml',
          `id: "aaa"
name: "Aaa"
auth_type: "oauth2"
extends: "zzz"
api:
  base_url: "https://aaa.example.com"
`,
        );
        writeYaml(
          tmp,
          'zzz.yaml',
          `id: "zzz"
name: "Zzz"
auth_type: "oauth2"
oauth2:
  authorize_url: "https://zzz.example.com/auth"
  token_url: "https://zzz.example.com/token"
  refresh_supported: true
  default_scopes: []
  scopes_delimiter: "space"
api:
  base_url: "https://zzz.example.com"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
`,
        );
        const reg = loadProviders(tmp);
        expect(reg['aaa'].oauth2.authorize_url).toBe('https://zzz.example.com/auth');
        expect(reg['aaa'].api.base_url).toBe('https://aaa.example.com');
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });

    it('throws a clear error on extends cycles', () => {
      tmp = makeTmp();
      try {
        writeYaml(
          tmp,
          'a.yaml',
          `id: "a"
name: "A"
auth_type: "oauth2"
extends: "b"
`,
        );
        writeYaml(
          tmp,
          'b.yaml',
          `id: "b"
name: "B"
auth_type: "oauth2"
extends: "a"
`,
        );
        expect(() => loadProviders(tmp)).toThrow(/cycle/i);
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });

    it('throws when a provider extends an unknown id', () => {
      tmp = makeTmp();
      try {
        writeYaml(
          tmp,
          'orphan.yaml',
          `id: "orphan"
name: "Orphan"
auth_type: "oauth2"
extends: "ghost"
`,
        );
        expect(() => loadProviders(tmp)).toThrow(/ghost/);
      } finally {
        rmSync(tmp, { recursive: true, force: true });
      }
    });
  });
});
