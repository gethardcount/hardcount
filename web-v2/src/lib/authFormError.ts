// The one line shown under a sign-in, forgot-password, reset-password or
// accept-invite form when the request fails. These pages used to show
// "Cannot connect to Velox API" for some failures and the browser's own
// "Failed to fetch" for others, and a password rule as a bare fragment
// ("must be at most 72 bytes …").
//
// Network, timeout, rate-limit and server faults read the same as everywhere
// else in the dashboard (lib/errorCopy). A password rule from the server is a
// fragment written to follow the field name, so it gets "Password" in front.
//
// Dependency-free (duck-typed) so the unit tests can load it.
import { classifyError, errorSentence } from './errorCopy'

export function authFormError(err: unknown): string {
  const kind = classifyError(err)
  if (kind === 'request') {
    const e = err as Error & { field?: string }
    if (e.field === 'password' && e.message.trim()) {
      return errorSentence(Object.assign(new Error(`Password ${e.message.trim()}`), { status: 422 }))
    }
  }
  return errorSentence(err)
}
