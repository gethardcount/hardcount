// How the dashboard's data loads react to a 429 (Too many requests).
//
// A 429 used to be retried like any failure: React Query keeps a query
// pending while it retries, so the page sat on its skeleton and then showed
// "wait a moment". Now a 429 is not retried in place. The page shows its
// error state at once, saying it will try again, and the query is fetched
// again when the API's Retry-After says the next request will pass.
//
// Dependency-free (duck-typed, no import of lib/api) so the unit tests can load it.

// Without a Retry-After header, wait this long. The API always sends one on
// a rate-limit 429; a proxy in front of it may not.
export const RATE_LIMIT_DEFAULT_WAIT_MS = 5_000
// Never wait longer than a minute: every limit the API sets is per minute or
// per second, and a longer wait means a header we should not trust.
export const RATE_LIMIT_MAX_WAIT_MS = 60_000
const RATE_LIMIT_MIN_WAIT_MS = 1_000

// parseRetryAfter reads a Retry-After header value: a whole number of seconds
// or an HTTP date (RFC 9110). It returns milliseconds, or undefined when the
// value is missing or unreadable.
// eslint-disable-next-line no-restricted-syntax -- Retry-After is a wall-clock instant from the server, never a test-clock time
export function parseRetryAfter(value: string | null | undefined, now: number = Date.now()): number | undefined {
  const v = value?.trim()
  if (!v) return undefined
  if (/^\d+$/.test(v)) return Number(v) * 1000
  // An HTTP date names its day or month ("Sat, 31 Oct 2026 09:00:07 GMT").
  // Without the letter check, Date.parse reads "1.5" as a date in 2001.
  if (!/[a-z]/i.test(v)) return undefined
  const at = Date.parse(v)
  if (Number.isNaN(at)) return undefined
  return Math.max(0, at - now)
}

interface RateLimitedLike {
  status: number
  retryAfterMs?: number
}

function asRateLimited(err: unknown): RateLimitedLike | null {
  if (!(err instanceof Error)) return null
  const e = err as Error & Partial<RateLimitedLike>
  return e.status === 429 ? (e as Error & RateLimitedLike) : null
}

export function isRateLimited(err: unknown): boolean {
  return asRateLimited(err) !== null
}

// rateLimitWaitMs is how long to wait before loading again after err, or
// undefined when err is not a 429. Retry-After is honored, within 1s to 60s.
export function rateLimitWaitMs(err: unknown): number | undefined {
  const rl = asRateLimited(err)
  if (!rl) return undefined
  const wait = rl.retryAfterMs ?? RATE_LIMIT_DEFAULT_WAIT_MS
  return Math.min(RATE_LIMIT_MAX_WAIT_MS, Math.max(RATE_LIMIT_MIN_WAIT_MS, wait))
}

// retryQuery is the dashboard's default React Query retry: one retry for a
// failed load, as before, except a 429, which shows the error state now and
// loads again after Retry-After (scheduleRateLimitRefetch).
export function retryQuery(failureCount: number, err: unknown): boolean {
  if (isRateLimited(err)) return false
  return failureCount < 1
}

// A query as the scheduler needs it: is anything on screen still showing it,
// and is it still failed. React Query's Query has both.
interface RefetchableQuery {
  getObserversCount(): number
  state: { status: string }
}

// scheduleRateLimitRefetch runs refetch after the wait err asks for, if err
// is a 429. When the time comes it loads again only if a page still shows
// the query and it is still failed, so a page left behind sends nothing.
// Returns the timer, or undefined when nothing was scheduled.
export function scheduleRateLimitRefetch(
  err: unknown,
  query: RefetchableQuery,
  refetch: () => void,
  setTimer: (fn: () => void, ms: number) => ReturnType<typeof setTimeout> = setTimeout,
): ReturnType<typeof setTimeout> | undefined {
  const wait = rateLimitWaitMs(err)
  if (wait === undefined) return undefined
  return setTimer(() => {
    if (query.getObserversCount() > 0 && query.state.status === 'error') refetch()
  }, wait)
}
