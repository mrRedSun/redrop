# redrop architecture

redrop shares agent-produced HTML, files, and static bundles through expiring links. The Go server embeds the React management dashboard. The Go CLI and `pkg/client` use the same JSON API.

`DESIGN.md` remains the product contract. This change does not alter the two-domain isolation model, gates, view counting, account roles, invite-only registration, token permissions, or the rule that administrators see other users' artifact metadata but not content or secret links.

## Usage first

A component submits form values, not an HTTP request:

```tsx
const form = useForm({
  resolver: schemaResolver(ArtifactSettingsForm),
  defaultValues: settingsDefaults(artifact, currentVersion.spa),
})
const onSubmit = useFormSubmit(form, artifacts.saveSettings(artifact.id), {
  toArg: (values) => ({ values, kind: artifact.kind }),
  onSuccess: (saved, values) => form.reset(settingsDefaults(saved, values.spa)),
})
```

A filtered list reads a feature atom. Its component does not know the endpoint, response envelope, or invalidation rules:

```tsx
const filter = filterFromSearch(searchParams)
const result = useKeepPrevious(useAtomValue(artifacts.list(filter)))
```

An admin handler can reach only the administrative service ports. Its return type has no secret link to copy:

```go
func (h adminAPI) listArtifacts(c fuego.ContextNoBody) (api.ArtifactList, error) {
    list, err := h.moderation.List(c.Context(), c.QueryParam("q"), c.QueryParam("status"))
    if err != nil {
        return api.ArtifactList{}, err
    }
    return api.ArtifactList{Items: mapItems(list, adminArtifactDTO)}, nil
}
```

## Backend module map

| Module | Owns |
| --- | --- |
| `internal/app` | Constructs storage, services, and transports; dispatches the app and content hosts; starts HTTP and maintenance. |
| `internal/accounts` | Authentication, sessions, named API tokens, profile/password/TOTP, explicit OIDC identity linking, setup, invite acceptance, and password resets. |
| `internal/accounts/admin.go` | The `Administration` port for user edits, invitations, and reset links. Invite lifetime parsing belongs here, not in HTTP handlers. |
| `internal/sharing` | Upload verification and extraction, publishing, ownership, versions, artifact settings, access grants, lifecycle, content resolution, and maintenance policy. |
| `internal/sharing/views.go` | Safe owner/admin results, version/file/access-log results, and the status/link/purge-time calculation. |
| `internal/sharing/moderation.go` | The `Moderation` port for metadata listing, revoke/restore, deletion, and server usage. |
| `internal/httpapi` | Fuego routes, authentication and CSRF middleware, cookie handling, input decoding, pure DTO mapping, and RFC 9457 responses. |
| `internal/content` | HTTP and HTML presentation on per-artifact subdomains: neutral gates, visitors' cookies, file streaming, directory listings, viewers, and the uniform miss response with its per-client miss limiter. |
| `internal/store` | SQLite records, queries, transactions, quotas, atomic view consumption, and consistent database snapshots. |
| `internal/blob` | Content-addressed file storage and garbage collection. |
| `internal/security`, `internal/netutil` | Password hashing, signed grants, fixed-window and token-bucket limiters, and trusted-proxy client resolution. |
| `internal/ids` | Random identifiers and the artifact slug format: the embedded EFF wordlist, `Slug` (mints four-word slugs), and `ValidSlug`, the only slug syntax check (word and legacy formats). |
| `internal/apperr` | Transport-independent error kinds and user-safe messages, with private causes for logging. |
| `internal/dashpaths` | Dashboard destinations minted or used by the server. Its test checks the corresponding declarations in `web/src/app/paths.ts`. |
| `internal/web`, `internal/webui` | SPA/install/download delivery and the embedded Vite output. |
| `internal/brand` | The redrop icons and their HTTP serving. `iconsrc` describes the mark once and generates the SVG, ICO, and Apple touch icon using only the standard library. |
| `internal/janitor` | Maintenance scheduling and daily SQLite snapshots. |
| `internal/markdown` | Client-only Markdown conversion, escaped raw HTML, parsed-output sanitization, local-reference reporting, and a portable document template. Never used by the content server. |
| `pkg/api`, `pkg/client`, `cmd/redrop` | Public JSON DTOs and reserved upload-path constant, the Go API client, typed publishing options/errors, shared confined `fs.FS` bundle packing, and CLI behavior. |

### Request flow

```text
app host router
  -> httpapi security/logging middleware
  -> authentication + CSRF + route capability check
  -> handler: decode -> service call -> DTO conversion
  -> accounts or sharing
  -> SQLite and blobs
```

Services return `apperr` errors. `httpapi/errors.go` maps their kinds to HTTP status and problem details once. Fuego decoding/validation errors retain their framework mapping. The content transport maps the same application errors to gate or error pages rather than JSON.

### Domain results and privacy

`httpapi` no longer imports storage records. Its `dto.go` is a pure adapter without clocks or service callbacks.

```go
type Artifact struct {
    ArtifactMeta
    URL string
}
type AdminArtifact struct {
    ArtifactMeta
    OwnerUsername string
}
type Moderation struct { s *Service }
type Administration struct { s *Service }
```

`ArtifactMeta` contains safe metadata, including computed status and purge time. It contains no slug, password hash, or access generation. Owner results add the secret URL. Moderation methods return only `AdminArtifact`.

The `adminAPI` receiver holds `*accounts.Administration` and `*sharing.Moderation`, not the owner services. An admin endpoint therefore cannot accidentally call an owner result mapper with an `adminView` flag omitted. Reflection tests pin the admin result fields and receiver dependencies. Integration tests check that the HTTP result's URL remains empty, as required by the existing JSON contract.

`accounts.Principal` has private storage fields and identity/capability accessors. `User`, `Me`, `Token`, `Invite`, and `Identity` expose no password hashes, TOTP secrets, or token hashes. Only creation/enrollment results carry the corresponding one-time secret.

The user list uses `store.ListUsersWithUsage`. Separate aggregates count artifacts and sum retained version bytes, avoiding duplicate artifact counts through the version join. Listing users takes one query instead of two extra queries per account. Usage failures now propagate rather than appearing as zero usage.

OIDC discovery warms up when serving, not when constructing an application to generate OpenAPI or mint an offline link. Discovery still retries on use. Offline invite/reset commands go through the accounts administrative port.

### Boundaries deliberately retained

The content transport still consumes storage-shaped content records through the sharing service. Replacing that established path with opaque handles would touch streaming, templates, and gate handling without changing the requested dashboard boundary. It is a possible separate refactor.

The service-local `mapStore` functions remain separate. Sharing owns quota wording; accounts owns one-time-link failures. A generic mapper in storage would couple persistence to application policy merely to deduplicate a few lines.

## Frontend module map

The frontend uses Effect 4.1.4 throughout. `effect` permits compatible updates from `^4.1.4`; `@effect/platform-browser` and `@effect/atom-react` are pinned to `4.1.4`, and the lockfile fixes the installed graph. There are no Effect v3 platform packages, TanStack Query, openapi-fetch, or zod dependencies.

```text
web/src/
  main.tsx                         React entry, providers, router, session watcher
  api/
    redrop-api.ts                  HttpClient service, Layer, call/probe, request encoding
    errors.ts                      RFC 9457 -> Schema.TaggedError union; safe display messages
    codecs.ts                      Timestamp, list envelope, successful command response
  runtime/
    runtime.ts                     AppLayer, Atom.runtime, one AtomRegistry, query/mutation helpers
    notifier.ts                    Effect notification service backed by sonner
    use-keep-previous.ts           Display previous rows across filter atom changes
  app/
    router.tsx                     Lazy route table and guarded prefetch
    guards.ts                      Session, guest, and admin capability guards
    paths.ts                       SPA path declarations and safe login destinations
    session-watch.ts               Session loss/identity change and focus rechecks
    providers.tsx                  Registry, theme, tooltip, and toaster providers
    route-error.tsx                Route/data/chunk/render failure boundary
    layouts/                      Public shell, signed-in navigation, user menu
  features/
    auth/                          Config, password/TOTP login, logout, SSO navigation
    onboarding/                    First-run setup, invite acceptance, password reset
    artifacts/                     List, settings, versions, files, log, and owner preview
    tokens/                        Token creation/revocation and CLI instructions
    account/                       Profile, password, TOTP enrollment/QR, SSO identities
    admin/                         Usage, users, invites, metadata-only artifact moderation
  components/                     Generic tables, dialogs, copy fields, form errors, stat cards
    ui/                            shadcn primitives
  hooks/                          Generic clipboard and debounce hooks
  lib/                            Form bridge, formatting, and class-name utilities
```

Each feature has `schemas.ts`, `endpoints.ts`, `atoms.ts`, and its pages/components. No barrels or per-endpoint wrapper hooks are required. Cross-feature imports share existing concepts, such as `Me`, auth providers, and the account-creation password rules, rather than duplicating them.

### Effect data flow

```text
component -> atom-react hook -> feature atom
  -> pure form-to-request mapper
  -> feature endpoint function
  -> RedropApi Context.Service
  -> BrowserHttpClient.layerFetch
  -> Go JSON API
  -> Effect Schema decode -> domain value / tagged error
  -> AsyncResult and declared invalidation / notification policy
```

`AppLayer` composes `RedropApi` and `Notifier` over `BrowserHttpClient.layerFetch`. This export exists in `@effect/platform-browser@4.1.4`; it re-exports the fetch implementation supplied by Effect. There is no need for an XHR client for this dashboard.

`RedropApi` has `call` and `probe`. It hides the `/api/v1` prefix, path-segment encoding, query encoding, CSRF header, JSON Schema encoding, response Schema decoding, timeout, non-JSON proxy errors, and the session-recheck policy. Features describe calls and never manipulate `HttpClientRequest` or HTTP responses themselves.

Every consumed request body and response has a feature Schema. Successful commands without payloads still decode the `{ok: boolean}` response. Timestamps decode to `Date`; nullable list envelopes become arrays in endpoint adapters. A malformed successful response is a contract defect, not a user input error. Its public error omits the raw response body, which could contain a secret.

`api/errors.ts` maps server status codes to a closed `Schema.TaggedError` union. Problem `title` becomes the machine code and `detail` becomes the user-safe message. `totp_required` is converted inside auth endpoints to the successful `TotpRequired` variant. Components never inspect problem status codes to implement the login steps.

### Atoms, invalidation, and identity

There is one registry outside React. Both route guards/loaders and `RegistryContext.Provider` use it. A loader cannot accidentally fill a separate cache from the one React reads.

Queries use `Atom.family` when parameterized. Mutations take form values or user choices, not request bodies. Their declarations specify success messages, inline-versus-toast failure handling, and typed `Reactivity` invalidation keys.

Keys are feature-wide: `artifacts`, `tokens`, `identities`, `admin`, `onboarding`, `authConfig`, and `session`. Effect 4.1.4's record-form invalidation also fires its bare key, so this design does not pretend to provide per-ID invalidation. Only mounted reads refetch; idle reads live for 60 seconds. Mutation atoms have zero idle TTL. There is no polling, optimistic cache manipulation, or persistent browser cache.

Filters live in react-router search parameters. Effect Schema parses allowed statuses, `Atom.family` selects the read, and `useKeepPrevious` retains the displayed rows during a filter change. The search input debounces URL writes. `Atom.searchParam` was rejected because it changes browser history outside react-router.

A protected 401 invalidates `session`, which re-reads `/me`. It does not itself sign anyone out: the same status also means a wrong current password. `probe`, used for `/me`, maps 401 to `Option.none` without invalidation, preventing a loop. Credential-entry endpoints suppress the recheck. The session watcher reloads to login only after confirmed session loss; it also rechecks on focus for changes made in another tab.

Successful sign-in, setup, and invite acceptance use full navigation. Confirmed sign-out or a changed signed-in identity also discards the page. This intentionally destroys all atoms, so a new account cannot inherit the previous account's displayed data. It costs one page load instead of maintaining a second set of cache-reset rules.

### Forms and secrets

`schemaResolver` connects `Schema.toStandardSchemaV1` to `@hookform/resolvers/standard-schema`. React Hook Form holds the Schema's encoded inputs, often strings. Submission receives decoded values, including numbers and trimmed text. Cross-field Schema issues attach to the appropriate field.

`useFormSubmit` uses promise-exit mode, displays typed failures at `root.server`, and resets the mutation atom in `finally`. This clears both the last argument and result, because `Atom.fn` retains both. Passwords remain only in their form or the short-lived login TOTP step. Creation dialogs copy one-time results into their own state, then disappear on close. Secrets decode as `Redacted`, and no secret is persisted to browser storage or included in a toast.

The token dialog shows a one-line setup command that contains the new token, at the owner's request: `curl … /install.sh | REDROP_TOKEN='…' sh` or `$env:REDROP_TOKEN='…'; irm … /install.ps1 | iex`. `features/tokens/setup-command.ts` builds it from the canonical `public_url` and quotes the token for POSIX sh or PowerShell. The installers move the token out of the environment before running anything else, pipe it to `redrop login --token-stdin` (never argv), and the PowerShell installer clears `Env:REDROP_TOKEN` on success or failure so it cannot shadow the saved login. The dialog warns that the command lands in shell history and names the prompting `redrop login --url ...` alternative.

Settings use explicit `keep`, `set`, and `remove` choices so opening a form does not extend expiry or remove a password. The live version supplies the SPA flag because the artifact DTO does not contain it. Explicit TTL choices are relative to submission time, not idempotent absolute timestamps. The server remains authoritative about TTL policy and limits.

### Routing and presentation

Public routes are `/login`, `/setup/:token`, `/invite/:token`, and `/reset/:token`. Signed-in routes are `/artifacts`, `/artifacts/:id`, `/tokens`, and `/account`. `/admin` contains overview, users, invites, and artifact metadata pages, guarded by `can_admin`, including the SSO-only administration setting.

Every page is a lazy route. The eager shells have Suspense fallbacks, and the root provides an initial hydration fallback and an error boundary. Since matched loaders run concurrently, artifact prefetch loaders await the session guard themselves before requesting private data. The HTTP service remains the authorization boundary even if a UI guard fails.

Shared components have no feature imports. Tables retain textual labels; status is never color-only. Stats use labeled numbers and single-measure progress indicators rather than charts with categorical color. Light/dark rendering uses the existing shadcn tokens. Visual and browser-interaction verification remains outstanding because browser testing was explicitly skipped.

## Adding a feature

1. Add `features/<name>/schemas.ts`. Define only the wire DTOs the feature consumes, plus form Schemas, defaults, and pure request mappers. Compare the wire fields with `web/openapi.json`.
2. Add `endpoints.ts`. Use `RedropApi` and encode every JSON body with its request Schema. Decode responses with their response Schemas. Keep URLs and envelopes here.
3. Add `atoms.ts`. Declare a typed `Reactivity` key, then query/mutation atoms and invalidation/toast policy. Choose inline form errors for submitted forms. Keep secrets transient.
4. Add pages/components that use atom-react hooks and `useFormSubmit`. Register lazy routes and any navigation in `app`. Shared components must remain domain-independent.
5. Add Schema/mapper tests, fake-HttpClient tests for failure behavior, and live contract coverage. Run all checks below.

For a backend operation, start with a service-owned input/result and its authorization/business rules. Implement the store operation if needed, then add pure mappings in `httpapi/dto.go` and a thin handler/route. Use session-only permissions for account/token management. Admin artifact operations must stay on `Moderation` and return `AdminArtifact`. Regenerate the OpenAPI snapshot with `npm run gen:api` and update the corresponding frontend Schemas/tests.

Oxlint enforces the dependency boundary: only endpoint modules, the API implementation, runtime composition, and tests may import the HTTP implementation. Generic shared components cannot import features. The fence was tested with violating imports, not merely configured.

## Rationale and synthesis

### Problem and grounding

The original Go package split was useful, but services returned storage records. HTTP mapping called back into services for status/link policy and blanked admin links with an `adminView` boolean. Invite TTL policy lived in a handler. User usage silently ignored errors and caused per-user queries. The frontend entry point and query client imported files that no longer existed.

Grounding traced app-host and content-host requests, authentication/CSRF, publishing, gate decisions, DTO mapping, SQLite reads, and CLI/e2e dependencies. The backend integration tests established the initial baseline. These observations, not a package-naming preference, determined which boundaries changed.

### Candidates considered

The first sketch compared three complete arrangements:

| Candidate | Decision |
| --- | --- |
| Feature folders with domain-owned Go views and form-value mutation hooks | Base. It placed endpoint, validation, cache, and UI ownership together without putting request bodies back into components. |
| Resource-option modules plus route folders and separate administrative ports | Kept the administrative ports and guarded prefetch. Rejected a new status-filter error response and a large content-handle rewrite because they would change behavior or expand the refactor. |
| Three large files per feature, opaque content handles, and new storage coordination | Kept the credential-401 distinction and the observation that SPA belongs to a version. Rejected the wider storage rewrite and handwritten copies of every generated type. |

An independent first-round comparison also preferred the feature-folder candidate. When the owner changed the frontend stack, the TanStack/zod sketch was discarded rather than patched with a second state system.

The second, Effect-only sketch compared one shared API service/runtime against separate API services/runtimes per feature. A third candidate did not complete because its runner was rate-limited. The shared-service candidate became the base: its small `call`/`probe` interface hides transport decisions without requiring an extra service wrapper for every endpoint. Its compiling spike also exercised errors, Schema encoding, family equality, and invalidation behavior against installed Effect 4.1.4.

Both completed candidates agreed on a shared registry, tagged errors, Schema-backed forms, and transient secrets. The synthesis adopted immediate mutation reset in addition to zero idle TTL, retained the existing router-loader model with guarded prefetch, and used full navigation for identity changes instead of retaining stale account data while refetching it. These choices avoid per-feature identity-cache coordination.

### Effect alternatives rejected

- `@tim-smart/openapi-gen@1.0.5` was run against this OpenAPI snapshot. It emitted v4 import paths but plain TypeScript interfaces, not Effect Schemas, and the output had two undefined `UnknownInterface` references. It therefore could not satisfy the request/response runtime-validation requirement. Schemas are handwritten per feature, with unit and live contract checks.
- Separate `ManagedRuntime` execution for loaders would introduce a second runtime next to atoms. Loaders read the existing registry instead.
- Per-feature service Layers are useful when they own separate implementations or policies. Here endpoint functions already own the feature's typed operations. Adding service classes around those functions would mostly forward calls to the same transport.
- A handwritten Effect `HttpApi` definition would duplicate the Go route contract and its error declarations. Revisit it if a working v4 generator can derive the runtime contract.
- Effect v3 packages and a mixed TanStack/Effect cache were excluded by the owner's stack decision.

The implementation did not reveal a repeated boundary failure requiring another architectural rewrite. Local deviations were the unusable generator, explicit secret reset, guarded prefetch, and reading SPA from versions. No new server DTO field or endpoint was needed.

## Development and checks

```sh
# Repository root
npm --prefix web ci
go vet ./...
go test ./...
go test -race ./...

cd web
npm run typecheck
npm run lint
npm test
npm run build
npm run gen:api
# Explicitly targets disposable development state, not production:
BASE=http://127.0.0.1:8080 npm run check:contract
```

`go generate ./internal/brand` regenerates `internal/brand/icons/` from `internal/brand/iconsrc`. A test fails when the committed files differ from the generator output. The Vite dev server reads the same directory as its public directory. The build does not copy it, because the Go server serves the icons at the root.

`npm run gen:api` refreshes `openapi.json`; it no longer emits an unused openapi-fetch declaration file. `check:contract` runs the actual endpoint functions and Schemas against a server. It logs in/out, round-trips settings, and creates/revokes a token and invite. It is not a production health probe. Its defaults match the disposable e2e fixture; override credentials through the documented environment variables when appropriate.

`./scripts/dev-server.sh` rebuilds the dashboard before the Go binary, builds CLI targets, and restarts the LAN-only development unit. It does not echo the bootstrap URL. The requested fresh e2e workflow must target only `/root/redrop-dev/data`, with the development unit stopped before reset. Never reset a production data directory to run tests.

Vite writes to `internal/webui/dist`. The build removes only its generated `assets` directory, retaining the tracked `.gitkeep`; stale fingerprinted chunks do not accumulate. Build the frontend before compiling a distributable Go binary.

## Operational assumptions and remaining risks

This is an architecture/dashboard change, not a production deployment or a production-readiness certification. No repository push, image publication, PR, or production deployment is part of it.

- The operator owns DNS/TLS for the separate app/content domains, trusted-proxy configuration, OIDC provider configuration, quotas, and resource limits. The LAN development server intentionally does not exercise production Secure cookies or TLS.
- Existing janitor snapshots cover SQLite. Recovery also needs blobs and the signing secret; protect and back up the whole data directory with permissions restricted to the service operator. Restore consistent snapshots, and do not delete persistent data during upgrades or rollback.
- Rollback must use a matching server/dashboard/CLI artifact and a compatible database backup if a future change adds migrations. This change adds no migration. Production Docker Compose packaging, pinned production images, and publishing/security-scan CI remain work outside this pass; they are prerequisites before claiming the global production delivery bar is met.
- Browser rendering, keyboard/mobile flows, third-party-cookie behavior of embedded previews, and a real OIDC round trip were not exercised. The owner explicitly skipped installing browser test tooling. Runtime atom APIs are marked unstable upstream, so upgrades require the test and contract suite. Clipboard access on a plain-HTTP LAN origin may be unavailable; copy fields allow manual selection.
- Handwritten Schemas can drift. Keep the OpenAPI snapshot and live contract check in release verification. The checks cover dashboard-consumed operations rather than unused upload endpoints. Existing backend concurrency/resource-exhaustion behavior, full shutdown/maintenance coordination, and a comprehensive production security review remain separate work.
