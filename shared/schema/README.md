# `shared/schema/`

Raw SQL migrations for the `_addition` schema. Numbered by convention `NNN_name.sql` — the loader runs them in lexicographic order.

## Rules
- Each file is **self-contained** and **idempotent** (`CREATE TABLE IF NOT EXISTS`, `INSERT ... ON CONFLICT`).
- Each file **MUST** end with an `INSERT ... ON CONFLICT` into `_addition.meta` that sets `schema_version` to its own number.
- **Never edit a merged migration.** Add a new `NNN+1_*.sql` file instead.
- All tables live under the `_addition` schema — never `public`.

Consumed by `ts/src/schema.ts` (and `python/src/runagent_addition/schema.py` when that track lands).
