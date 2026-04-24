import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse as parseYaml } from 'yaml';

/**
 * CLAUDE.md §6 invariant #9:
 *   shared/spec.yaml version == ts/package.json version == python/pyproject.toml version
 *
 * This test pins the TS side of the invariant; Python adds a mirror test.
 */
describe('spec version parity', () => {
  it('shared/spec.yaml.version matches ts/package.json.version', () => {
    const specPath = join(__dirname, '..', '..', '..', 'shared', 'spec.yaml');
    const pkgPath = join(__dirname, '..', '..', 'package.json');
    const spec = parseYaml(readFileSync(specPath, 'utf8')) as { version: string };
    const pkg = JSON.parse(readFileSync(pkgPath, 'utf8')) as { version: string };
    expect(typeof spec.version).toBe('string');
    expect(typeof pkg.version).toBe('string');
    expect(pkg.version).toBe(spec.version);
  });
});
