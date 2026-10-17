// Orval "mutator" — orval-generated hooks call this function instead
// of building their own fetch/axios client. We delegate to the
// existing apiRequest fetch wrapper so:
//
// - Session cookies ride automatically (credentials: 'include').
// - Error responses parse through humanizeError + ApiError.
// - The Velox-Request-Id header is captured into setLastRequestId so
//   "Report an issue" carries the most recent trace handle.
// - Generated code never has to know about the Velox API base path
//   (/v1) — the wrapper prepends it.
//
// orval's fetch-flavoured mutators are invoked with a positional
// (url, RequestInit) signature; we don't return the rich
// `{ data, status, headers }` envelope that some orval consumers want
// because the generated `getXyzResponse` types track that shape but
// nothing on our side reads `.headers` or `.status` (success cases
// drop straight into react-query data, errors throw via ApiError).
// Returning the parsed JSON body keeps the migration boring: the
// dashboard's existing Invoice-shaped consumers see the same field
// tree they always saw, just typed.
import { apiRequest } from './api'

export const orvalClient = async <T>(
  url: string,
  init?: RequestInit,
): Promise<T> => {
  // Orval emits absolute paths starting with `/v1/...` because that's
  // what the spec's `paths` keys say. apiRequest expects the path
  // *under* /v1 (it prepends the base), so strip a leading `/v1` if
  // present. Without this we'd end up hitting `/v1/v1/invoices/...`.
  let path = url
  if (path.startsWith('/v1')) {
    path = path.slice('/v1'.length)
  }
  const method = (init?.method ?? 'GET').toUpperCase()
  // orval serialises the request body to a JSON string in `init.body`.
  // apiRequest expects an unserialised value (it JSON.stringifies
  // internally), so parse back if a body is present. Skipping the
  // round-trip would either double-encode or skip Content-Type.
  let body: unknown
  if (init?.body && typeof init.body === 'string') {
    try {
      body = JSON.parse(init.body)
    } catch {
      body = init.body
    }
  } else if (init?.body) {
    body = init.body
  }
  return apiRequest<T>(method, path, body)
}
