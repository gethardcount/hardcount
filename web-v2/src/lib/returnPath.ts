// Where to send a user after login: the page they were trying to open, which
// ProtectedRoute stores in the router state as `from`. Only a path inside the
// dashboard is honoured ("/invoices/x"), never "//host" or "https://...", so
// a crafted link cannot send someone off-site after they sign in.
export function returnPath(state: unknown): string {
  const from = (state as { from?: unknown } | null)?.from
  if (typeof from !== 'string' || !from.startsWith('/') || from.startsWith('//') || from.startsWith('/\\')) {
    return '/'
  }
  if (from === '/login' || from.startsWith('/login?')) return '/'
  return from
}
