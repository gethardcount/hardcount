// A sign-in can succeed on the server and still leave the browser signed
// out: in production the session cookie is marked Secure, and a browser
// drops a Secure cookie that arrives over plain HTTP from any host but
// localhost. The login call answers 200, the next request carries no
// cookie, and the operator lands back on the sign-in page with no error.
// These helpers turn that silent bounce into a message.

export class SessionNotKeptError extends Error {
  constructor() {
    super('session not kept')
    this.name = 'SessionNotKeptError'
  }
}

// confirmSessionKept asks the server who owns the cookie right after a
// successful sign-in. Only a 401 proves the cookie was dropped; any other
// failure is a transient error and must not block a sign-in that worked.
export async function confirmSessionKept(
  whoami: () => Promise<unknown>,
  isUnauthorized: (err: unknown) => boolean,
): Promise<void> {
  try {
    await whoami()
  } catch (err) {
    if (isUnauthorized(err)) throw new SessionNotKeptError()
  }
}

// plainHTTPSignInMessage is shown when sign-in fails on a page that is
// neither HTTPS nor localhost. Two things fail there: the browser sends no
// Sec-Fetch-Site header from an insecure page, so the cross-site guard
// answers 403; and if the request does get through, the Secure cookie is
// dropped.
export const plainHTTPSignInMessage =
  'Sign-in does not work at this address because it is plain HTTP. Open the dashboard over HTTPS, or at http://localhost on the server.'

// secureContext is window.isSecureContext: true on HTTPS and on localhost.
export function sessionNotKeptMessage(secureContext: boolean): string {
  if (!secureContext) return plainHTTPSignInMessage
  return 'Your password is correct, but this browser did not keep you signed in. Check that cookies are allowed for this site.'
}

// signInRefusedMessage maps a refused sign-in (403 from the cross-site
// guard) to the plain-HTTP explanation when that is the cause. On a secure
// page a 403 has another cause, so the server's own message stands.
export function signInRefusedMessage(secureContext: boolean, serverMessage: string): string {
  return secureContext ? serverMessage : plainHTTPSignInMessage
}
