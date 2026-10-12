// Orval configuration — generates typed @tanstack/react-query hooks
// from the canonical OpenAPI spec at api/openapi.yaml (copied into
// web-v2/public/openapi.yaml by scripts/copy-openapi-spec.mjs as the
// `npm run copy-spec` step that gates `dev`, `build`, and `gen`).
//
// Generated artifacts live in src/lib/gen/ — a dedicated subdirectory
// so re-runs don't risk wiping hand-written siblings if `clean` is
// ever turned on. The hooks file (queries.gen.ts) and the schema
// types file (schemas.gen.ts) sit there together; importing code
// pulls from `@/lib/gen/queries` etc.
//
// `openapi-typescript` writes a parallel raw-type file at
// src/lib/api.gen.ts (paths-as-keys flat type tree, useful for
// endpoints we haven't migrated to typed hooks yet). Orval's role is
// the hooks layer, openapi-typescript's is the broad type layer; both
// regenerate from the same spec on `npm run gen`.
//
// `mutator` points orval at orvalClient.ts which delegates to the
// existing apiRequest fetch wrapper in src/lib/api.ts — so generated
// hooks reuse the dashboard's auth posture (session cookies,
// credentials: 'include'), error envelope parsing, and
// Velox-Request-Id header handling without duplicating any of that
// into orval's default Axios client. Same trade-off Stripe makes in
// stripe-node — the SDK is generated from the spec but the transport
// is hand-written and reused.
import { defineConfig } from 'orval'

export default defineConfig({
  velox: {
    input: {
      target: './public/openapi.yaml',
    },
    output: {
      mode: 'single',
      target: './src/lib/gen/queries.gen.ts',
      schemas: './src/lib/gen/schemas',
      client: 'react-query',
      override: {
        mutator: {
          path: './src/lib/orvalClient.ts',
          name: 'orvalClient',
        },
        // No useQuery/useMutation flags: since orval 8.22 setting both
        // forces BOTH hook kinds onto every operation (a mutation hook for
        // each GET, a query hook for each PUT/DELETE). The defaults give
        // GET a query and every other verb a mutation, which is the shape
        // the pages are written against.
        query: {
          signal: false,
        },
        // Orval 8.x defaults to wrapping responses in
        // `{ data, status, headers }` for the fetch client. Our
        // mutator delegates to the existing apiRequest wrapper,
        // which throws ApiError on non-2xx and returns just the
        // parsed JSON body on success — so consumers want the body
        // type directly (e.g. InvoiceWithLineItems), not an
        // envelope. `includeHttpResponseReturnType: false` keeps
        // the generated return types as the bare body, matching
        // what the mutator actually resolves with.
        fetch: {
          includeHttpResponseReturnType: false,
        },
      },
      prettier: false,
      // No `clean` — the gen/ subdirectory is the only writable
      // surface and orval's per-file rewrite covers drift on its own.
      // `clean: true` would recursively delete the parent of `target`
      // (src/lib/), which would nuke api.ts and every other
      // hand-written sibling.
    },
  },
})
