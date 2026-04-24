import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    environment: 'node',
    // Integration tests start ephemeral Postgres containers — allow a generous
    // startup + setup + teardown budget.
    testTimeout: 60_000,
    hookTimeout: 120_000,
  },
});
