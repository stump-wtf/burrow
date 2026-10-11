---
status: draft
date: 2026-10-10
implements: [ADR-0004]
---

# SPEC-0004 design: provider admin

- **Store-backed Runtime replaces the file Runtime.** `providers.NewRuntime`
  takes the `*store.Store` and options; the boot-time `Load(file)` survives
  only as the seed path (`SeedFromStore-empty` semantics in main.go).
  `RowsFor/ConnectFor/SnapshotFor` take a `context.Context` for store reads.
  The in-memory constructors used by tests go away; tests seed the store via
  `storetest.Open` like the httpapi tests do.
- **Model cache inside the providers package.** Per-row entry
  `{ids []string, fetchedAt, changeAt, nextTryAt, err}` under a mutex, keyed
  by row id; per-row fetch happens on first need and on TTL expiry, inline
  (one gateway GET per expired row per request wave), with `nextTryAt`
  providing the failure cooldown (~TTL/4). `changeAt` advances the stored
  `updated_at` (single `UPDATE`, single process) only when the id set
  differs from the previously served set — the snapshot stays quiet otherwise.
- **ETag = quoted `updated_at` RFC3339Nano.** PUT is a full replace; a
  missing `If-Match` is `428 Precondition Required` rather than a silent
  clobber, because the whole point of the API is automation without lost
  updates.
- **One middleware, two credentials.** `requireAdmin` runs before the
  handler: API-token bearer first (its hash exists in `api_tokens` and holds
  `llm-admin`), else the normal session lookup plus the `BURROW_ADMIN_GROUPS`
  check. The session error codes keep the contract's session-terminating
  vocabulary for sessions only; token failures on admin routes answer plain
  `401 invalid_token` — automation must never mistake a revoked token for a
  dead session.
- **Audit in-transaction.** The store exposes `CreateProvider/UpdateProvider/
  DeleteProvider`, each writing its `llm_provider_audit` row in the same
  tx; handlers never audit separately, so a crashed audit cannot desync.
- **Groups scope at sign-in.** `oidc.New` always adds the `groups` scope
  unless the new `BURROW_OIDC_GROUPS_SCOPE=false`; `UsesGroups()` retires
  (rows are mutable at runtime; boot-time detection is unsound).
- **Seed.** `main.go`: store empty + `BURROW_PROVIDERS_FILE` set → validate +
  insert each row (models → nameOverrides; model-level groups dropped with a
  warning). Store non-empty → file ignored entirely (log once at info level).
- **Config additions.** `BURROW_MODELS_TTL` (60s), `BURROW_ADMIN_GROUPS`
  (comma list, default empty), `BURROW_API_TOKEN_TTL` (2160h),
  `BURROW_OIDC_GROUPS_SCOPE` (true).
