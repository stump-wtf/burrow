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
  by row id; per-row fetch happens on first need and on TTL expiry (one
  gateway GET per expired row per request wave; off the request path since
  2026-10-12, see "Menu refresh off the request path"), with `nextTryAt`
  providing the failure cooldown (~TTL/4). `changeAt` advances the stored
  `updated_at` (single `UPDATE`, single process) only when the id set
  differs from the previously served set — the snapshot stays quiet otherwise.
  "Served" includes the empty menu a row goes out with while its first
  fetch is failing or still in flight: when that menu fills, the stamp
  advances. Served sets are per process; a restart forgets them.
- **ETag = quoted `updated_at` RFC3339Nano.** PUT is a full replace; a
  missing `If-Match` is `428 Precondition Required` rather than a silent
  clobber, because the whole point of the API is automation without lost
  updates. DELETE takes `If-Match` optionally: absent, it deletes by id
  (automation that only knows the id); present, `DeleteProvider` checks it
  inside the same row lock `UpdateProvider` uses and a stale one is `412`.
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
- **Delete confirm carries the etag.** The confirm page puts the row's etag
  in a hidden field and the confirm POST sends it as `If-Match`. Because the
  API's DELETE is unconditional without `If-Match`, the console itself
  refuses a confirm POST whose etag is missing or malformed rather than
  forwarding it (2026-10-12).
- **Browser session = the session token in a cookie.** The non-desktop OIDC
  callback mints a session and sets `burrow_session` (HttpOnly,
  SameSite=Lax, Secure on https, `SessionTTL`); the desktop deep-link flow
  is untouched. A member who can sign in could already mint a session by
  exchanging the page's grant, so this adds no capability.
- **Cross-origin writes are refused by the server, not by the cookie
  (2026-10-12).** This section first said Lax keeps the cookie off
  cross-site POSTs and so the console needs no CSRF defense. That was wrong:
  SameSite is scoped to the site, and a form on any sibling subdomain of
  burrow's registrable domain is same-site, so its POST carries the cookie.
  The whole `/admin` subtree is now mounted behind Go's
  `http.CrossOriginProtection` (`sameOriginConsole`): a non-safe request
  passes only with `Sec-Fetch-Site: same-origin`/`none`, or, from a browser
  too old to send it, an `Origin` whose host is the request's `Host`; a
  request with neither header passes (a non-browser client, with no ambient
  cookie to ride, or a browser years out of date). No token in the forms, so
  there is no state to mint, store or expire. `/api/den` stays outside it:
  bearer-only, nothing ambient to ride.
- **Sign-out is a console POST, open to non-admins (2026-10-12).** The
  cookie is HttpOnly and the API's sign-out reads a bearer, so a browser had
  no way to end its session short of the TTL. `POST /admin/sign-out` deletes
  the session row (the token dies as a bearer too), expires the cookie and
  redirects to `/`. It skips `requireAdminPage` because every web sign-in
  gets the cookie, admin or not, and the console's 403 page is where a
  non-admin lands; it stays behind the cross-origin check so another site
  cannot sign a member out.
- **Etags on forms.** The editor carries the fetched etag in a hidden field;
  a 412 re-renders with the API's current row, a 409/400 re-renders the
  typed values with the API's message, and a 428 (the form lost its etag)
  re-renders them asking for a reload. A re-rendered form reads the key
  light and the org-wide warning back from the stored row (2026-10-12).
- **Audience is one group per line.** The editor's `groups` field is a
  textarea split on newlines, each line taken whole, so every group the API
  accepts survives an untouched save. It was a comma-separated input, which
  split a group containing a comma (an LDAP DN) into several (2026-10-12).
- **Preview is the runtime's own answer.** `/admin/preview?group=` calls
  `RowsFor` with that single group; there is no second matching rule to
  drift.
- **Menus come from `Runtime.MenuFor`.** The same cache a listing serves, so
  counts and display names match what members see. Overrides for ids the
  current menu lacks ride as hidden inputs: a PUT is a full replace.
  `Runtime.MenuKnown` says whether that menu is an answer from the gateway;
  until one has answered (unreachable, key refused, failure cooldown, key
  variable unset) the list, the preview, and the delete confirm say the
  model count is unknown instead of printing 0 (2026-10-12).

## Menu refresh off the request path (2026-10-12)

- **Why.** The first cut fetched inline, serially, under the entry mutex and
  on the request's context, 10 s per row. A gateway that accepted and never
  answered then cost a listing 10 s per row and `/connect` 10 s — past the
  desktop client's 12 s request timeout — and a request cancelled mid-fetch
  recorded its own cancellation as a gateway failure for everyone.
- **One call per row, behind the requests.** A menu that is due a fetch gets
  one goroutine (`inFlight` on the entry: at most one per row) on
  `context.WithoutCancel` of the request that started it, bounded by the
  runtime HTTP client's 10 s timeout. The entry mutex guards fields only and
  is never held across the gateway call. The call records the menu, or the
  failure and a cooldown counted from when it returned, and writes the
  `updated_at` bump itself.
- **What a request waits for.** Nothing, when the row has a last-known
  answer — a menu, or a recorded failure: it is served and the call runs
  behind it (stale-while-revalidate). Only rows with no answer yet are
  waited for, and by `RowsFor`, `SnapshotFor` and `MenuFor` alone:
  `ConnectFor` never waits. Those calls are started together and the request
  waits for all of them once, 4 s at most. Concurrency rather than a shorter
  first-fetch timeout, because a per-row timeout still multiplies by the row
  count, while one shared wait is the same for two rows or twenty; and a
  wait cap rather than a shorter call, so a gateway that needs 6 s still
  fills the cache for the next poll instead of never being heard.
- **Tests wait, they do not sleep.** `export_test.go` exposes
  `WaitRefreshes` (the in-flight call count) and `SetTimeouts`.
