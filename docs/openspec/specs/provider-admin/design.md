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

## Console (2026-10-11, stump.wtf/burrow#23)

- **Pages over the API, in-process.** `requireAdminPage` gates `/admin` with
  the same `holdsAny(user.Groups, BURROW_ADMIN_GROUPS)` check `requireAdmin`
  applies. Mutations build the write API's JSON body and drive the API's own
  handlers through the same mux (`consoleCall`), carrying the member's
  session token as the bearer, so the API's own middleware re-checks the
  gate and wires the audit actor; reads call the same store functions the
  API's handlers call. No loopback HTTP hop: the edge proxy owns the public
  origin, so a self-call could not reliably reach the process.
- **Browser session = the session token in a cookie.** The non-desktop OIDC
  callback mints a session and sets `burrow_session` (HttpOnly,
  SameSite=Lax, Secure on https, `SessionTTL`). Lax keeps the cookie off
  cross-site POSTs, so the console's POST-only mutations need no CSRF
  token; the desktop deep-link flow is untouched. A member who can sign in
  could already mint a session by exchanging the page's grant, so this adds
  no capability.
- **Etags on forms.** The editor carries the fetched etag in a hidden field;
  a 412 re-renders with the API's current row, a 409/400 re-renders the
  typed values with the API's message.
- **Preview is the runtime's own answer.** `/admin/preview?group=` calls
  `RowsFor` with that single group; there is no second matching rule to
  drift.
- **Menus come from `Runtime.MenuFor`.** The same cache a listing serves, so
  counts and display names match what members see. Overrides for ids the
  current menu lacks ride as hidden inputs: a PUT is a full replace.
