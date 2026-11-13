# redrop dashboard

The React dashboard is embedded in the Go server. It is not a separately hosted application.

## Develop

Use Node 24 and the repository's Go toolchain:

```sh
npm ci
npm run dev
```

Vite proxies `/api` to `http://127.0.0.1:8080`. Set `REDROP_DEV_API` to use another development backend. First-run setup requires the one-time link emitted by the server; registration is otherwise invite-only.

From the repository root, `./scripts/dev-server.sh` builds both the dashboard and Go binaries and restarts the LAN-only development service. It requires systemd and is not a production deployment command.

## Verify

```sh
npm run typecheck
npm run lint
npm test
npm run build
```

`npm test` runs deterministic Schema, API failure, form-mapping, redirect, redaction, and atom tests with Node's test runner. It does not require a running server.

For the live Schema contract check, use disposable development state initialized by `scripts/e2e.sh`:

```sh
BASE=http://127.0.0.1:8080 npm run check:contract
```

This check creates/revokes a token and an invite, round-trips artifact settings, and logs in/out. Do not point it at production. `E2E_USER` and `E2E_PASSWORD` override the development fixture credentials. Do not put real credentials in source files or command history.

## API and feature conventions

Use Effect v4, `@effect/platform-browser`'s `BrowserHttpClient.layerFetch`, and `@effect/atom-react`. Feature modules contain:

- `schemas.ts` for wire and form Schemas, defaults, and pure form-to-request mappings.
- `endpoints.ts` for typed calls through the `RedropApi` service.
- `atoms.ts` for reads, writes, invalidation, and notification policy.
- `pages/` and `components/` for UI that only consumes feature atoms.

Forms use React Hook Form and the Standard Schema resolver over Effect Schema. Shared components must not import features. Oxlint checks these boundaries. Full conventions, rationale, and the module map are in [`docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md).

`npm run gen:api` refreshes `openapi.json` from Go. Request/response Schemas are handwritten per feature because the tested v4 generator did not emit usable runtime Schemas. Keep them aligned with the snapshot and the live contract check. There is no second generated TypeScript client.

## Build

`npm run build` writes to `../internal/webui/dist`. It keeps `.gitkeep` and replaces the generated assets. Compile the Go server only after building the dashboard. A server binary built from an unbuilt checkout can run its API without the dashboard.

TanStack Query, openapi-fetch, zod, and Effect v3 platform packages are not used. Install with `npm ci` so local and release builds use the lockfile.
