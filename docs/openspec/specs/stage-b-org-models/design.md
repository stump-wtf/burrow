---
status: draft
date: 2026-10-09
implements: [ADR-0001]
---

# SPEC-0002 design: Stage B org models

- **A file, not a database table.** The org has a handful of providers that
  change with the gateway, not per-user state; a JSON file watched by mtime is
  the smallest thing that satisfies the endpoints. Rows are derived, so there
  is no store to keep in sync with the client's view. A later per-member
  credential model would graduate this to SQLite — noted in the spec's out of
  scope.
- **Keys by reference.** The definition file names an environment variable
  (`apiKeyEnv`); the value is read at request time from the process
  environment (OpenBao injects it in deployment). `hasApiKey` is computed per
  request — rotating the key needs no restart — and a set-but-empty variable
  counts as not provisioned.
- **Fail-fast validation.** The strict rules (`lpr_` ids, non-empty identity
  fields, absolute `baseURL`, complete model rows, no duplicate ids) are
  enforced in `providers.Load`, so `main` exits on a bad file instead of
  serving a row that would fail the client's whole sync.
- **Nil runtime = Stage A behavior.** With no file configured, the runtime is
  nil and every method is nil-safe: empty rows, empty snapshot, no connects.
  The Stage A stub behavior falls out of the same code path.
- **`updatedAt` from file mtime.** Touching the file (or editing it) changes
  the timestamp the resource snapshot carries, which is what triggers the
  client's re-sync after a burrow restart.
