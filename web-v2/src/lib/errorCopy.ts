// What the dashboard says when a request fails. One place decides, by the
// HTTP status and error code, which sentence an operator reads, so a load
// error and an error toast for the same failure say the same thing.
//
// The server's own sentence is shown when it is written for the person who
// made the request (validation, conflicts, refusals). It is replaced when it
// is written for API callers ("missing credentials … Authorization: Bearer"),
// says nothing ("an internal error occurred"), or comes from the browser
// ("Failed to fetch"). Those raw strings stay available for support in a
// collapsed Details.
//
// Dependency-free (duck-typed, no import of lib/api) so node --test can load it.

export type ErrorKind =
  | 'offline'   // no response: network down, server unreachable, or timed out
  | 'session'   // 401: signed out; the session handler sends them to sign-in
  | 'notfound'  // 404
  | 'rate'      // 429
  | 'server'    // 5xx: a server fault
  | 'request'   // any other 4xx: the server explains what to change
  | 'unknown'   // not an API error at all (a bug in the page)

interface ApiErrorLike {
  message: string
  status: number
  code?: string
  requestId?: string
  method?: string
}

function asApiError(err: unknown): ApiErrorLike | null {
  if (!(err instanceof Error)) return null
  const e = err as Error & Partial<ApiErrorLike>
  if (typeof e.status !== 'number') return null
  return { message: e.message, status: e.status, code: e.code, requestId: e.requestId, method: e.method }
}

// fetch() rejects with a TypeError when no response arrives. The wording is
// browser-specific: Chrome "Failed to fetch", Safari "Load failed",
// Firefox "NetworkError when attempting to fetch resource."
function isNetworkFailure(err: unknown): boolean {
  return err instanceof TypeError && /failed to fetch|load failed|networkerror/i.test(err.message)
}

export function classifyError(err: unknown): ErrorKind {
  const api = asApiError(err)
  if (!api) return isNetworkFailure(err) ? 'offline' : 'unknown'
  if (api.status === 0) return 'offline'
  if (api.status === 401) return 'session'
  if (api.status === 404) return 'notfound'
  if (api.status === 429) return 'rate'
  if (api.status >= 500) return 'server'
  return 'request'
}

// Sentence case for a server message: server strings start lower-case
// ("a payment for this invoice is already in progress …") because API
// callers compose them; on screen they stand alone.
function sentence(s: string): string {
  const t = s.trim()
  if (!t) return t
  const out = t.charAt(0).toUpperCase() + t.slice(1)
  return /[.!?…)]$/.test(out) ? out : out + '.'
}

export const SESSION_ENDED_MESSAGE = 'Your session ended. Sign in to continue.'

// errorSentence is the one sentence shown for a failed request.
export function errorSentence(err: unknown): string {
  const api = asApiError(err)
  switch (classifyError(err)) {
    case 'offline':
      return api?.code === 'timeout'
        ? "The server didn't respond in time. Check your connection and try again."
        : "Couldn't reach the server. Check your connection and try again."
    case 'session':
      return SESSION_ENDED_MESSAGE
    case 'notfound':
      // The generic "<resource> not found" names a table, not a thing an
      // operator knows; a domain 404 with its own code explains itself.
      if (api?.code && api.code !== 'not_found' && api.message) return sentence(api.message)
      return 'It may have been deleted, or it belongs to the other mode (test or live).'
    case 'rate':
      return 'Too many requests. Wait a moment, then try again.'
    case 'server':
      // A 5xx with its own code ("stripe_unavailable", "provider_error")
      // carries a sentence worth reading; the bare internal error does not.
      if (!api || api.code === 'internal_error' || !api.code || !api.message) {
        return 'Something went wrong on the server. Try again; if it keeps happening, copy the error ID for support.'
      }
      return sentence(api.message)
    case 'request':
      return sentence(api!.message)
    default:
      // Not an API error: a message the page threw on purpose, or a bug.
      return err instanceof Error && err.message.trim()
        ? sentence(err.message)
        : 'Something unexpected went wrong. Try again.'
  }
}

// rawErrorDetail is what goes in a collapsed Details: the original text,
// when the sentence above replaced it. Empty when the sentence already is it.
export function rawErrorDetail(err: unknown): string {
  const raw = err instanceof Error ? err.message.trim() : ''
  if (!raw) return ''
  return sentence(raw) === errorSentence(err) ? '' : raw
}

// errorRequestId is the support id to offer for copying: only for a server
// fault. On a validation error the id is noise; the sentence says what to fix.
export function errorRequestId(err: unknown): string | undefined {
  const api = asApiError(err)
  if (!api || classifyError(err) !== 'server') return undefined
  return api.requestId || undefined
}

// outcomeUnknown is true when the action may have taken effect but no answer
// says so: a change sent with no response back (timeout or dropped
// connection), or a charge Stripe never answered (payment_outcome_unknown).
// A caller title like "Failed to void" or "Couldn't charge" would then be a
// guess, so the toast says the result is unknown instead.
export function outcomeUnknown(err: unknown): boolean {
  const api = asApiError(err)
  if (!api) return false
  if (api.code === 'payment_outcome_unknown') return true
  if (api.status !== 0 || !api.method) return false
  return api.method !== 'GET' && api.method !== 'HEAD'
}

export const NO_ANSWER_TITLE = 'Result unknown'
export const NO_ANSWER_MESSAGE =
  "The server didn't answer, so this may have gone through. Refresh to check before trying again."

export interface ErrorToast {
  // false: show nothing. A 401 is handled once for the whole dashboard
  // (sign-in redirect), so each failed request must not also toast.
  show: boolean
  // 'warning' when the action may have gone through (no answer came back):
  // that is uncertain, not a failure, so it is not drawn as an error.
  tone: 'error' | 'warning'
  title: string
  description?: string
  copyId?: string
}

// errorToast keeps the caller's title ("Couldn't attach meter") and puts the
// sentence under it. With no title, the sentence is the title.
export function errorToast(err: unknown, title?: string): ErrorToast {
  const body = errorSentence(err)
  const copyId = errorRequestId(err)
  const show = classifyError(err) !== 'session'
  if (outcomeUnknown(err)) {
    // Stripe's own sentence says what happens next; a request with no
    // response has nothing to add beyond "check before retrying".
    if (asApiError(err)?.code === 'payment_outcome_unknown') {
      return { show, tone: 'warning', title: 'Payment result unknown', description: body, copyId }
    }
    return { show, tone: 'warning', title: NO_ANSWER_TITLE, description: NO_ANSWER_MESSAGE, copyId }
  }
  if (!title) return { show, tone: 'error', title: body, copyId }
  return { show, tone: 'error', title, description: body, copyId }
}
