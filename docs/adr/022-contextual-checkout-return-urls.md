# ADR-022: Stripe Checkout return URLs are contextual, not global

**Status:** Accepted
**Date:** 2026-05-04

## Summary

Stripe Checkout started from the operator's `/checkout/setup` returns to the page the operator came from, not a fixed URL. The dashboard sends that page as `return_url`, and the server adds `?payment=success` or `?payment=cancel` so the page can show the result. `STRIPE_CHECKOUT_SUCCESS_URL` and `STRIPE_CHECKOUT_CANCEL_URL` were removed, so every Checkout flow now uses a contextual return URL. The webhook, not the redirect, records the saved card. Status: active.

**2026-10-23:** `POST /v1/checkout/setup` was removed ([#1368](https://github.com/gethardcount/hardcount/pull/1368)), and with it the `?payment=success` / `?payment=cancel` return. The operator's setup session (`POST /v1/customers/{id}/payment-methods/setup-session`) still takes a `return_url`; without one, it returns to `/payment-method-added` on `CUSTOMER_PORTAL_URL` (`http://localhost:5173` when that is unset). The payment-update link takes no `return_url`: it returns to `PAYMENT_UPDATE_RETURN_URL`, or to `http://localhost:5173/payment-method-added` when that is unset. Both add `?status=success` or `?status=cancel`.

**2026-11-04:** None of these return addresses has a default any more: not `CUSTOMER_PORTAL_URL`, not `PAYMENT_UPDATE_RETURN_URL`, and not `HOSTED_INVOICE_BASE_URL` for the hosted invoice page's Pay. While one is unset, the flow it serves is refused before any Stripe call, so no customer is sent to `http://localhost:5173`. See the amendment at the end.

## Context

Operator-reported bug: setting up a payment method from the customer
detail page → Stripe Checkout opens in a new tab → operator clicks
Cancel → SPA renders a **blank page** at `localhost:5173/checkout/cancel`.

Trace:
- `STRIPE_CHECKOUT_CANCEL_URL=http://localhost:5173/checkout/cancel`
  was set in `.env.example` and read by `payment.NewCheckoutHandler`
  at boot.
- The handler stamped that exact URL on every Stripe Checkout session
  it created.
- The SPA had a route for `/checkout/success` (a "Payment successful"
  landing page) but **no matching route for `/checkout/cancel`**, so
  cancel hit the SPA's catch-all → blank.

Fixing the blank page would have been a one-line "add the missing
route." But the deeper issue was the design: **global return URLs are
the wrong primitive**. Every other Checkout-creating flow in the
codebase already uses contextual return URLs:

| Flow | Return URL | Source |
|---|---|---|
| Hosted-invoice Pay | `{base}/invoice/{public_token}` | `internal/hostedinvoice/handler.go:457` |
| Customer-portal update PM | `req.return_url` (defaulted to `/customers/{id}?payment=updated`) | `internal/payment/portal.go:73` |
| Public token-authenticated update | tenant config or invoice page | `internal/payment/public_handler.go:142` |
| **Operator-side `/checkout/setup`** | **`STRIPE_CHECKOUT_*_URL` env (global)** | `internal/payment/checkout.go` |

The operator-side setup flow was the outlier. Stripe's own
documentation, Vercel, Linear, and Recurly all follow the contextual
pattern: cancel = "back where you came from," not a stateless
"you canceled" page that loses customer context.

## Decision

`/checkout/setup` adopts the contextual-return pattern that the rest
of the codebase already uses:

1. **`setupRequest` accepts an optional `return_url` body field.**
   The operator's SPA passes the page they were on
   (`window.location.origin + window.location.pathname`).
2. **Handler appends `?payment=success` / `?payment=cancel`** to the
   return URL. SPA reads the query param on mount, toasts the result,
   refetches `payment_setup`, and strips the query so refresh doesn't
   re-toast.
3. **Default fallback (when `return_url` is unset)** is
   `/customers/{customer_id}?payment=…`. Always valid because every
   setup flow has a `customer_id`.
4. **`STRIPE_CHECKOUT_SUCCESS_URL` + `STRIPE_CHECKOUT_CANCEL_URL`
   removed** from `.env.example`, `deploy/compose/docker-compose.yml`,
   `deploy/compose/.env.example`, and the `payment.NewCheckoutHandler`
   constructor signature. Boot no longer reads them.
5. **`web-v2/src/pages/CheckoutSuccess.tsx` and the
   `/checkout/success` route deleted.** No flow targets them now.

## Consequences

- **Cancel now lands the operator back on the customer detail page**
  with `?payment=cancel`, which surfaces a `toast.info("Setup
  canceled — no changes were made")` and removes the query.
- **Success now lands the operator back on the customer detail page**
  with `?payment=success`, which surfaces `toast.success("Payment
  method saved")` and refetches `customer-payment-status` so the card
  details render immediately. The webhook is still the authoritative
  path that flips `customer_payment_setups.setup_status` → `ready`;
  the toast is just a UX hint that the redirect landed.
- **Every Checkout-creating flow now follows the same pattern.** No
  more outlier. Adding a new flow means "pick the page the user came
  from" rather than "add another env var."
- **No migration needed.** Pre-launch, no operators are relying on the
  env vars in production.

## Alternatives considered

- **Add the missing `/checkout/cancel` SPA route.** Fixes the blank
  page in one line but leaves the wrong design in place. Cancel still
  loses customer context — the page can't link back to the customer
  detail without re-fetching from `session_id`.
- **Template the customer_id into the env URL** (e.g.
  `…/checkout/cancel?customer_id={CUSTOMER_ID}`). Stripe's URL
  templating only supports `{CHECKOUT_SESSION_ID}`, not arbitrary
  variables. We'd have to look up customer from session — extra
  round-trip for no gain over just using the customer page directly.
- **Keep the env override as a power-user knob** with the contextual
  default underneath. Adds two config surfaces for one capability.
  Pre-launch with one operator, the env knob is pure config debt.

## Amendment 2026-11-04: no default return address

Every return address fell back to `http://localhost:5173` when its setting
was unset, in every environment. In production a customer paid or saved a
card, then landed on "site can't be reached". The money still settled by
webhook, but the customer could not tell.

Now none has a default, the same as `DASHBOARD_BASE_URL` and
`PAYMENT_UPDATE_URL` already behaved. While one is unset:

| Setting | Refused |
|---|---|
| `HOSTED_INVOICE_BASE_URL` | The hosted invoice page shows no Pay button (`pay_unavailable_reason` `online_payment_unavailable`), and a checkout POST answers 409 with that code. |
| `CUSTOMER_PORTAL_URL` | A setup session with no `return_url`, and Send setup link, answer 409 `return_address_not_set`. A caller's own `return_url` still works. |
| `PAYMENT_UPDATE_RETURN_URL` | The "add a payment method" email is not sent. A link sent earlier answers 409 `online_payment_unavailable` before its token is used, so it works once the setting is set. |

Each refusal comes before any Stripe call. Start-up logs `<NAME> NOT SET`
for each, and the invoice notices stop offering what can't work.

Refusing to start was rejected. Compose defaults to `APP_ENV=production`
with these settings empty, the one-VM guide sets them only after the stack
first starts, and saved-card charges and dunning retries need none of
them. ADR-095 rejected a boot refusal over these same settings for the
same reason: it would stop billing on an upgrade.

Changing `HOSTED_INVOICE_BASE_URL` while a hosted checkout is half-made
(a pending claim left by a crash) changes the session's parameters, so
Stripe refuses that one replay. The customer's next Pay makes a new
session.
