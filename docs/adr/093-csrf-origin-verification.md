# ADR-093: CSRF defense for the cookie dashboard — origin verification, not tokens

Date: 2026-07-16
Status: Accepted
Relates: ADR-011 (email+password sessions — the httpOnly cookie this protects), ADR-075/077 (unrelated; billing TZ)

## Summary

Velox blocks cross-site request forgery (CSRF: another website making an operator's browser send a request that carries the session cookie) by checking where each request comes from. One middleware, `CSRFGuard`, wraps `/v1/auth` and the `/v1` group and refuses a cross-site POST, PUT, PATCH or DELETE with 403. It trusts the browser's `Sec-Fetch-Site` header first, then `Origin` matched against `CORS_ALLOWED_ORIGINS`; a request with neither header (a script) or a Bearer-key request with no session cookie passes. Header checks replace CSRF tokens because the dashboard is a first-party app that no other site embeds, so no token handling or frontend change is needed. Status: active.

## Context

The dashboard authenticates with an httpOnly `velox_session` cookie (ADR-011);
the API authenticates with `Authorization: Bearer` keys. A cookie is *ambient
authority*: the browser attaches it to every request to the API automatically,
so any page the operator visits can cause an authenticated request. That is the
CSRF class, and a live walkthrough on 2026-07-16 confirmed it was exploitable
end-to-end in a real browser, in two shapes:

- **Forced logout.** `POST /v1/auth/logout` answered a request carrying *no*
  cookie with `Set-Cookie: velox_session=; Max-Age=0`. SameSite=Lax withholds
  the cookie from every cross-site request, so an attacker's cross-site POST
  landed in the no-cookie branch, and the browser honoured the clear — logging
  the operator out on sight (auto-submitting form → no click), repeatedly, while
  the session stayed live server-side for its full TTL (an orphaned credential).

- **Login fixation.** `POST /v1/auth/login` accepted a `text/plain` body (a
  cross-site HTML form's only JSON-capable content type, with the JSON smuggled
  through the field name), authenticated the attacker's own credentials, and
  returned `Set-Cookie`. The victim's browser stored it — silently signing them
  into the *attacker's* account and tenant, so their subsequent work (customers,
  data, cards) landed where the attacker could read it.

Two facts shaped the fix:

- **CORS is not a CSRF defense.** The existing CORS middleware decides whether an
  attacker's JS may *read the response*; it never blocks the request from
  *executing*. A form POST or any "simple request" runs the handler regardless,
  and a fixation attacker never needs to read anything — the `Set-Cookie` side
  effect already happened.

- **SameSite=Lax is necessary but not sufficient.** It narrows the surface but
  still permits top-level GET navigation and does nothing about a cookie being
  *set* cross-site (the fixation case).

## Decision

### 1. Server-side origin verification on unsafe, cookie-path requests

A single middleware (`middleware.CSRFGuard`) wraps the two dashboard-serving
route groups — `/v1/auth` and the cookie-or-bearer `/v1` group. On unsafe
methods (POST/PUT/PATCH/DELETE), unless the request carries an
`Authorization: Bearer` header, the request must be same-site:

- **`Sec-Fetch-Site`** is the primary signal — browser-set, unforgeable by page
  JS. `same-origin` and `none` pass; `cross-site` is rejected 403. `same-site`
  passes **only if its `Origin` is an allowlisted dashboard origin** — otherwise
  a sibling subdomain (subdomain takeover, a user-content host) could ride the
  same-site label plus the Lax cookie into a mutation.
- **`Origin` allowlist** is the fallback for pre-2020 browsers that omit
  `Sec-Fetch-Site`, and the confirmation for the `same-site` case above. The
  allowlist is the *same* `CORS_ALLOWED_ORIGINS` list, so the two cannot drift.
- **Neither header present → allow.** Deliberately not fail-closed: a genuine
  cross-site attack requires a browser, and every browser that can run the SPA
  sets at least one of those headers on a cross-site unsafe request (page JS
  cannot strip them). "Neither present" therefore means a non-browser caller
  (curl, SDK, server job) that holds no ambient cookie and cannot be the CSRF
  vector. Failing closed here would only break scripting, for no security gain.

This closes both findings at their root: the cross-site POST to `/login` or
`/logout` is refused before it reaches the handler.

### 2. Bearer is exempt, by construction — but only the *pure* bearer path

A bearer key is a *non-ambient* credential — an attacker cannot make a victim's
browser attach it — so the SDK/server-to-server path is CSRF-immune and
legitimately sends no `Origin`. The guard skips a request with a Bearer header
**only when it carries no session cookie**. A request with *both* a bearer header
and a session cookie is not a legitimate shape (the SDK has no cookie; the
dashboard sends no bearer) — it is the "add a junk `Authorization` header to trip
the exemption, let the ambient cookie authenticate" dodge, which a permissive
(`CORS_ALLOWED_ORIGINS=*`) config would otherwise pass through a preflight. Such
a request is origin-checked, not exempted, so the guard holds regardless of CORS
configuration. (An HTML form cannot set headers at all, so the pure-form vector
never reaches the exemption; this covers the `fetch`-with-credentials variant.)

### 3. Why origin verification and not CSRF tokens

For a first-party SPA whose frontend we control, header verification is the
modern OWASP-endorsed equivalent of a synchronizer token and costs far less: no
token minting, storage, rotation, or per-request plumbing, and **zero frontend
changes** (the browser already sends `Origin`/`Sec-Fetch-Site` on same-origin
requests). A token earns its keep only when the frontend is embedded in
third-party contexts where `Origin` is unreliable — Velox is not, so tokens stay
in the back pocket, to be revisited if embedding ever appears.

### 4. The logout no-cookie branch clears nothing

Independently of the guard (defense in depth), `logout` no longer calls
`ClearCookie` when it received no cookie. "No cookie arrived" is not "the browser
holds no cookie" — under Lax they are indistinguishable at the server, so a
branch that cannot authenticate the caller must not mutate the caller's cookie
jar. The legitimate sign-out still clears, on the far side of a revoke that
worked.

## Consequences

- The whole cookie-authenticated mutation surface (login/logout + the 20 other
  `/v1` JSON mutation handlers) is covered by one chokepoint, not per-handler
  checks. A new mutation route under `/v1` inherits the defense for free.
- Not gated: the Stripe webhook group (HMAC, server-to-server, no Origin), the
  unauthenticated public payment/invoice/cost surfaces (no operator session to
  abuse), and GET-only siblings (SSE stream, CSV exports).
- Behaviour change for tooling: a cross-site browser POST to a dashboard
  endpoint now 403s. Non-browser callers (curl/SDK) are unaffected. A developer
  scripting a browser-shaped request against the dashboard must send a same-site
  `Sec-Fetch-Site` or an allowlisted `Origin`.

## Rejected / deferred

- **`__Host-` cookie prefix** (hardening against subdomain cookie injection):
  deferred. It requires the `Secure` attribute, which local dev (HTTP) does not
  set, so adopting it forces a dev/prod cookie-name divergence for a threat
  distinct from and lesser than this CSRF class. Its own decision, later.
- **CSRF tokens** (synchronizer / double-submit): rejected as primary for the
  reasons in §3; reconsider only on third-party embedding.
- **Content-Type: application/json enforcement**: worthwhile API hygiene and it
  independently defeats the HTML-form vector, but it is not the load-bearing
  control here and pretending it is a second CSRF defense would be
  belt-and-suspenders. The origin gate is the complete fix.
