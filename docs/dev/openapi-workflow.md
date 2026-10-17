# OpenAPI workflow

`api/openapi.yaml` is the single source of truth for the HTTP API. `make gen`
regenerates four things from that one file: the Go server interface, the
TypeScript types, the `@tanstack/react-query` hooks, and the spec copy served
to external consumers at `/openapi.yaml`. CI fails the build if any generated
file drifts from what the spec describes.

This is the same shape Stripe, Twilio, GitHub and Kubernetes use to keep
their SDKs aligned with their HTTP surface.

## Add or change an endpoint

1. **Edit `api/openapi.yaml`.** Add the path, the `operationId`, the request
   body schema (under `components.schemas` if reusable, inline otherwise),
   and the response shape. Include error responses such as `404`, so the
   generated TS types narrow on status and callers get the right body type
   per status code.
2. **Run `make gen` from the repo root.** It runs both Go and TS codegen,
   refreshes `web-v2/public/openapi.yaml`, and rewrites the generated files
   in place.
3. **Implement the handler.** On the Go side, add a method on the relevant
   handler that matches the generated `ServerInterface` signature, for
   example `func (h *Handler) GetInvoice(w, r, id string)`. A
   `var _ <interface> = (*Handler)(nil)` assertion (a compile-time check
   that the handler implements the generated interface) turns spec-vs-code
   drift into a build error rather than a runtime 404.
4. **Move the dashboard to the generated hook.** Replace ad-hoc
   `useQuery({ queryFn: () => api.foo() })` calls with the generated hook,
   for example `useGetInvoice(id)`. The hook handles the URL, the query keys
   and the typed response shape. The mutator (`orvalClient`, the function
   every generated request goes through) sends the request through
   `apiRequest`, so cookies, error-envelope parsing and `Velox-Request-Id`
   capture keep working unchanged.
5. **Run the local gates:** `go build ./...`, `go vet ./...`,
   `go test ./... -short`, `cd web-v2 && npx tsc -b`. Use `tsc -b`:
   `--noEmit` is a no-op under this repo's composite project references.
6. **Commit the spec change and the regenerated files together.** See
   [CI gate](#ci-gate).

## Files at a glance

| Path | What it is | Hand-edit? |
| ---- | ---------- | ---------- |
| `api/openapi.yaml` | OpenAPI 3.0.3 source of truth | Yes — this is the contract |
| `api/cfg.yaml` | `oapi-codegen` config (Go) | Rarely |
| `tools/tools.go` | Build-tagged import that pins `oapi-codegen` to `go.mod` | When bumping codegen |
| `internal/api/generated/api.gen.go` | Generated Go types + chi-compat `ServerInterface` | No — regenerate |
| `web-v2/orval.config.ts` | `orval` config (TS hooks) | Rarely |
| `web-v2/scripts/copy-openapi-spec.mjs` | Copies `api/openapi.yaml` into `web-v2/public/openapi.yaml`, so the served spec and codegen read the same file | No |
| `web-v2/src/lib/api.gen.ts` | Generated raw TS types from `openapi-typescript` | No — regenerate |
| `web-v2/src/lib/gen/queries.gen.ts` | Generated react-query hooks from `orval` | No — regenerate |
| `web-v2/src/lib/gen/schemas/` | Generated TS schema types (one file per `components.schemas` entry) | No — regenerate |
| `web-v2/src/lib/orvalClient.ts` | Hand-written `orval` mutator that delegates to the existing `apiRequest` fetch wrapper. See [Why a custom orval mutator](#why-a-custom-orval-mutator) | Yes |

## CI gate

`.github/workflows/ci.yml` runs `make gen`, then `git diff --exit-code`. The
first command regenerates everything from the spec. The second fails if that
changed anything in the working tree. So both of these fail CI:

- a spec edit without the regenerated files;
- a hand edit to a generated file, such as `api.gen.go`.

The rule: generated files must equal what `make gen` produces from the
committed spec.

## Why it is built this way

### Why a build-time copy of the spec into `web-v2/public/`

The spec is served to external consumers at a stable same-origin URL
(`/openapi.yaml`). The embedded Scalar viewer that rendered it at
`/docs/api` was cut 2026-04-29. `web-v2/scripts/copy-openapi-spec.mjs`
copies `api/openapi.yaml` into `web-v2/public/openapi.yaml`, and runs as a
`predev` / `prebuild` / `gen` hook in `web-v2/package.json`.

A symlink would break on hosts that flatten symlinks during checkout. An
explicit copy is portable, and it shows in the package.json scripts as a
clear dependency.

### Why a custom orval mutator

`orval` builds its own Axios client by default. We delegate to `apiRequest`
(in `web-v2/src/lib/api.ts`) instead, because it already carries:

- session-cookie auth;
- parsing of the Stripe-style error envelope (the `{ error: {…} }` JSON
  wrapper on failure responses) into `ApiError`;
- capture of the `Velox-Request-Id` header into `setLastRequestId`, for
  "Report an issue" trace handles.

Reusing the wrapper keeps generated hooks on the same auth posture as every
hand-written caller in the dashboard. Stripe-node makes the same trade-off:
the SDK is generated from the spec, and the transport is hand-written and
reused.

### Why two TS generators

They regenerate from the same spec on `make gen`, and they do different jobs:

- `openapi-typescript` writes `web-v2/src/lib/api.gen.ts`: one flat type
  tree indexed by URL path and method. A page that has not moved to the
  typed hooks can still take a precise payload type from
  `paths['/v1/foo']['get']`.
- `orval` writes the typed react-query hooks.

## Trade-offs and known sharp edges

- **Optional fields are plain types, not pointers.**
  `prefer-skip-optional-pointer: true` in `api/cfg.yaml` renders optional
  fields with `omitempty` JSON tags rather than `*string`. A `*""` non-nil
  pointer marshals as `""`, which would break wire-shape parity with the
  existing `domain.Invoice` JSON output. The cost: callers cannot tell
  "field absent" from "field present and empty" at the type level. For
  Velox's domain types, every empty string means the same as "absent", so
  that is the right call.
- **Hooks return the bare body.**
  `output.override.fetch.includeHttpResponseReturnType: false` keeps the
  orval hook return types as the body (`Promise<InvoiceWithLineItems>`),
  not the `{ data, status, headers }` envelope. The mutator throws
  `ApiError` on non-2xx and resolves with the body on 2xx. The bare-body
  type matches what it returns, and removes a `.data` step at every call
  site.
- **Generated files live in `web-v2/src/lib/gen/`**, not beside the
  hand-written files in `web-v2/src/lib/`. orval's `clean: true` would delete
  the parent of `target` recursively. Pointed at `src/lib/queries.gen.ts`, it
  would delete every hand-written file in `src/lib/` on the next regen. The
  `gen/` subdirectory limits the damage even if `clean` is ever turned back
  on.
