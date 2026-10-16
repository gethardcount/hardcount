// A session can end while a dashboard tab stays open: it expires, the
// operator is removed from the workspace, or their password is reset
// elsewhere. Every request then answers 401, and before this each page showed
// the server's text for API callers ("missing credentials — … Authorization:
// Bearer vlx_secret_…"). Now any 401 is reported here, one check asks the
// server who owns the cookie, and only if that also answers 401 is the
// operator sent to sign-in, then back to the page they were on.
//
// The check matters: a 401 from one endpoint does not prove the session is
// gone, and signing someone out on a false alarm would lose their work.
//
// Dependency-free so the unit tests can drive it with fakes.

// Requests whose 401 is part of signing in or out, not a sign of a lost
// session. The check itself (whoami) is here too, so it cannot report itself.
const IGNORED_PATHS = ['/auth/login', '/auth/logout', '/whoami', '/auth/password-reset', '/auth/invite/', '/auth/accept-invite']

export function isSessionProbePath(path: string): boolean {
  return IGNORED_PATHS.some(p => path.startsWith(p))
}

export interface SessionEndWatcher {
  // report is called with the API path of every request that answered 401.
  report: (path: string) => void
}

export function createSessionEndWatcher(deps: {
  isSignedIn: () => boolean
  // Resolves if the session is still valid; rejects otherwise.
  confirm: () => Promise<unknown>
  isUnauthorized: (err: unknown) => boolean
  onEnded: () => void
}): SessionEndWatcher {
  let checking = false
  return {
    report(path: string) {
      if (isSessionProbePath(path)) return
      if (!deps.isSignedIn()) return
      // Many requests fail together when a session ends; check once.
      if (checking) return
      checking = true
      deps
        .confirm()
        .then(
          () => undefined,
          err => {
            if (deps.isUnauthorized(err) && deps.isSignedIn()) deps.onEnded()
          },
        )
        .finally(() => {
          checking = false
        })
    },
  }
}

// The api client reports 401s to whichever watcher the auth provider
// installed. None is installed outside the signed-in app (hosted invoice,
// payment pages), so a 401 there does nothing.
let installed: SessionEndWatcher | null = null

export function installSessionEndWatcher(w: SessionEndWatcher | null): void {
  installed = w
}

export function reportUnauthorized(path: string): void {
  installed?.report(path)
}
