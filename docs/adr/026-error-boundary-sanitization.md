# ADR-026: HTTP error responses are sanitized at a single boundary

**Status:** Accepted (amended 2026-07-20)
**Date:** 2026-05-05

## Summary

`respond.FromError` is the one place that turns a service error into an HTTP response, so raw SDK, database and runtime errors never reach a response body. Typed Velox errors pass through with their status, and errors that implement `SafeMessageError` show only their `OperatorSafeMessage()`, such as "Card was declined: Insufficient funds.". Any other error becomes a generic 500 and is logged in full with the request ID. One legacy fallback remains: an untyped error worded like validation ("invalid …") gets a 422 with its text. Amended 2026-07-20: when the auth lookup cannot reach a verdict, for example because the database is down, the auth middleware returns `503 authentication_unavailable` instead of a misleading 401. Status: active, amended.

## Context

Operator-visible toast on Retry Payment exposed a raw Stripe SDK
string verbatim:

> Keys for idempotent requests can only be used with the same parameters
> they were first used with. Try using a key other than
> 'velox_inv_vlx_inv_d7s5753majdik74k71l0_pm_1TTRjFGcT3wmy5fZkgFV3Ozx'

That bug had two layers:
1. The actual idempotency-key collision (fixed in a separate commit by
   making the key per-attempt via `inv.UpdatedAt.UnixNano()`).
2. **The error-leak path that put the raw SDK message on screen** —
   `respond.Validation(w, r, fmt.Sprintf("payment failed: %s", err.Error()))`
   in `internal/invoice/handler.go:675`.

An audit of the codebase found **12 sites** doing the same thing —
passing a raw error string straight to the response body, where it
becomes:
- The text on a SPA toast,
- The body of a JSON error response,
- A potential leak of Stripe SDK strings, Postgres error messages,
  internal Velox identifiers (tenant_id, livemode, idempotency keys),
  Go runtime errors, etc.

Industry comparison (Stripe / Vercel / Linear / Twilio): all of them
sanitize at the boundary, never per-call-site. Stripe's `error.message`
on their public API is always a categorized human string; raw upstream
or DB internals never reach the wire.

## Decision

A single-boundary sanitization contract:

1. **`respond.FromError(w, r, err, resource)` is the only correct way
   to translate a service/store error into an HTTP response.** Direct
   `respond.Validation(..., err.Error())` — and equivalents — are
   bug-prone and removed from the high-impact paths.

2. **Typed Velox errors pass through.** `errs.ErrNotFound`,
   `errs.ErrAlreadyExists`, `errs.ErrValidation`, `errs.ErrConflict`,
   `errs.ErrInvalidState`, `errs.ErrPreconditionFailed`, plus any
   `*DomainError` with an explicit code — these are operator-safe by
   construction; `FromError` surfaces their messages directly with the
   right HTTP status.

3. **Errors opting into curated rendering (SafeMessageError marker)
   are surfaced via `OperatorSafeMessage()`.** New marker interface in
   `internal/api/respond/`:

   ```go
   type SafeMessageError interface {
       error
       OperatorSafeMessage() string
   }
   ```

   `*payment.PaymentError` implements it — returns "Card was declined:
   Insufficient funds." for declines, "Payment provider rejected the
   request" for everything else. Detected via `errors.As` so wrappers
   (`fmt.Errorf("payment failed: %w", paymentErr)`) preserve the
   marker.

4. **Anything else falls through to `InternalError(w, r)`.** Generic
   500 body, full error logged with the request-ID. The request-ID
   header is the bridge for support to find the trace in logs.

5. **Customer-facing endpoints (Tier 2) get tighter sanitization.**
   `internal/payment/public_handler.go` (hosted-invoice payment
   update via tokenized email link) renders only neutral copy:
   "We couldn't start the payment update right now. Please contact
   your billing administrator if the problem persists." No "Stripe
   error" framing — end customers shouldn't see operator context.

## What got migrated

| File | Before | After |
|---|---|---|
| `internal/invoice/handler.go:675` (Collect/Retry) | `Validation(fmt.Sprintf("payment failed: %s", err.Error()))` | `FromError(err, "payment")` — surfaces `*PaymentError.OperatorSafeMessage()` for declines, generic for SDK errors |
| `internal/invoice/handler.go:628` (SendInvoice) | `Validation(fmt.Sprintf("failed to send email: %s", err.Error()))` | `FromError(err, "invoice_email")` |
| `internal/payment/portal.go:95` (Update PM Checkout) | `Error(... fmt.Sprintf("failed: %v", err))` | Generic message + slog full error |
| `internal/payment/checkout.go:184` (Operator setup Checkout) | Same | Same |
| `internal/payment/public_handler.go:167` (Customer-facing Checkout) | Same | Tier 2: neutral customer copy |
| `internal/auth/middleware.go:36` | `Unauthorized(err.Error())` | Generic "invalid or expired API key"; full error logged. Closes a key-enumeration / DB-error leak. |
| `internal/session/middleware.go:62` | Same | Same |

Sites left as-is (intentional — their `err.Error()` returns hand-
written operator-safe messages):
- `internal/api/exports.go` (×4): `parseDateRange` returns curated
  RFC3339 error strings.
- `internal/coupon/handler.go` (×3): `buildListFilter` / `parseIfMatch`
  return curated validation strings.
- `internal/usage/handler.go:280`: `h.resolve` returns validation
  errors with field names.

These are **not** leaks because the upstream functions are
`fmt.Errorf("invalid X: ...")` — not raw SDK / DB strings.

## Wrapping a typed error must use `%w`, not `%s`/`%v`

`internal/payment/stripe.go:339` previously did:

```go
return domain.Invoice{}, fmt.Errorf("%s: %s", verb, pe.Message)
```

This loses the `*PaymentError` type, so `errors.As` in `FromError`
can't detect the marker. Fixed to:

```go
return domain.Invoice{}, fmt.Errorf("%s: %w", verb, pe)
```

Same lesson applies to any future wrapper: use `%w` to preserve the
type chain.

## Tests

`internal/api/respond/errors_test.go` adds:
- `TestFromError_UnknownError`: asserts raw error message DOES NOT
  leak to the response body for default-branch errors.
- `TestFromError_SafeMessageError_SurfacedNotRaw`: asserts a fake
  marker error has its safe message surfaced and raw stripped.
- `TestFromError_SafeMessageWrapped_DetectedViaErrorsAs`: asserts
  `fmt.Errorf("...: %w", inner)` preserves the marker through
  `errors.As`.

These are the canaries — if a future change leaks raw error strings,
the assertions fail loudly.

## Consequences

- **Operator UI never shows raw SDK/DB strings.** Toast on Retry
  Payment now reads "Card was declined: Insufficient funds." instead
  of the idempotency-key-conflict prose.
- **Customers never see operator-context errors.** Public endpoints
  return Tier 2 neutral copy.
- **Auth leaks closed.** Bearer-token validation errors no longer
  reveal whether a key exists / is expired / is revoked / failed at
  the DB layer. Generic message; full reason logged.
- **No structural change to the SPA.** `showApiError(err.message)`
  keeps working — the messages are just operator-safe now.
- **Future endpoints inherit the contract automatically.** New
  handlers calling `respond.FromError` get the full sanitization for
  free; direct `err.Error()` propagation becomes the bad pattern,
  enforced by code review.

## Deferred (with triggers)

The full Stripe-class taxonomy (closed-set typed codes, doc URLs per
code, frontend code-matching) is real long-term debt but not
evidence-driven yet:

- **Closed-set code taxonomy in `errs/codes.go`**: trigger when 3+
  new error types appear or OpenAPI spec wants enumeration.
- **Frontend matches on code, not message**: trigger when error
  copy needs to vary (localization, A/B tests, role-specific framing).
- **Doc URLs per code**: trigger when first design partner asks for
  the error reference docs.
- **`error_events` analytics table**: trigger when ops needs
  patterns ("how many idempotency conflicts last hour?").

Each is a contained 1-2 day improvement when the trigger fires.
Pre-launch, none have evidence to justify the cost.

## Alternatives considered

- **Sanitize at every call site.** Bug-prone — has been the source of
  every leak in the audit. Single boundary is the right shape.
- **Detect specific upstream error types (`*stripe.Error`, pgx
  errors) in `respond.FromError` directly.** Would require the
  `respond` package to import `stripe-go` and `pgx`, creating a
  circular dependency. The marker-interface pattern keeps `respond`
  generic; types in other packages opt in via the interface.
- **String-match the error message** for known leaky patterns
  ("Keys for idempotent requests…"). Brittle — Stripe SDK messages
  drift across versions, and we'd be inventing patterns rather than
  using the type system. Rejected.

## Amendment (2026-07-20, #560): infra failures at the auth boundary are 503, not 401

The original stance ("never reveal … whether the lookup failed at the DB
layer") conflated two different secrets. Whether a *key* exists / is expired
/ is revoked is key-shaped information — revealing it enables enumeration,
so those verdicts stay behind the generic 401. Whether the *database is
reachable* is server-health information: it is fleet-wide (every caller
gets the same answer while the DB is down), already observable through
latency and every other endpoint, and reveals nothing about any individual
credential. Observed live 2026-07-20: with postgres down, every Bearer
request returned `401 invalid or expired API key`, which tells integrators
to rotate credentials during an outage — a lie about the key.

`auth.ValidateKey` now returns the sentinel `auth.ErrInvalidKey` for every
definitive rejection (format, unknown prefix, hash mismatch, expiry);
anything else means the verdict never happened. All three auth middlewares
(auth.Middleware, session.Middleware, session.MiddlewareOrAPIKey — including
its cookie branch, which previously degraded an outage to "missing
credentials" via fall-through) map non-verdict errors to
`503 api_error/authentication_unavailable` with no upstream detail in the
body. The sanitization boundary is unchanged: DB internals still never
reach a response.
