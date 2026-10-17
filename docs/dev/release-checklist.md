# Release checklist: live services

Run this by hand before each release tag. It covers what automated tests
cannot prove because they fake the provider: Stripe, email, LiteLLM and the
invoice PDF. Use Stripe test mode only. See [How we test](../testing.md).
The checks write Stripe-linked test data into the database the server uses,
so point it at a fresh or dedicated database if you want to keep yours clean.

## Setup

- `cp .env.example .env`, then `make up` (Postgres, Redis, Mailpit), `make bootstrap` (prints the sign-in and the test keys), `make dev` (API on :8080) and `cd web-v2 && npm run dev` (dashboard on :5173).
- The `.env.example` defaults send email to Mailpit and point email links at the dashboard on :5173. Read mail at http://localhost:8025.
- Settings → Payments: paste the Stripe test keys. Run `stripe listen --forward-to localhost:8080/v1/webhooks/stripe/<vlx_spc_id>` and paste its `whsec_…` back.
- Test cards: `4242 4242 4242 4242` succeeds; `4000 0000 0000 0341` saves but declines on charge; `4000 0027 6000 3184` asks for 3-D Secure.
- In Checkout, choose "Pay without Link" if offered, and give a ZIP for a US address, or Save does nothing.
- [Stripe end-to-end test](stripe-end-to-end-test.md) has the step-by-step commands for the Stripe checks.

## Stripe

- [ ] Wrong keys are refused: a made-up `sk_test_` key shows "Stripe didn't accept these keys".
- [ ] Charge: the first invoice of a subscription whose base fee bills in advance is charged to `4242`, Velox shows it paid, and the Stripe dashboard shows the same amount.
- [ ] Webhook: `stripe listen` shows `payment_intent.succeeded` answered with 200.
- [ ] Hosted invoice page: Pay with `4242` → "Paid" banner within seconds, and the card appears on the customer.
- [ ] 3-D Secure: Pay with `4000 0027 6000 3184`, fail the challenge → the invoice stays unpaid; pass it → paid.
- [ ] Refund: a credit note with "Refund to card" → `refund_status=succeeded`, and the refund shows in the Stripe dashboard.
- [ ] Stripe Tax: with a US state registration in the Stripe test account, tax provider "Stripe Tax", customer with a full US address → the invoice has a tax line that matches Stripe's calculation.

## Money walk across a renewal

- On the Dunning policies page, "Default retries" is the default and enabled. A bootstrap creates it in both modes (ADR-123). When its retries run out it changes nothing: final actions none and none.
- [ ] Mint a test clock, create a customer with an email, pinned to the clock, and subscribe it to a plan with a base fee (plans bill in arrears by default).
- [ ] With `stripe listen` running, open the customer's setup link (Add payment method) and save card `0341` in Checkout → the card is listed on the customer. Do this before you advance the clock.
- [ ] Advance the clock past the renewal → the renewal invoice is declined and a dunning run (automatic retries for a failed payment) starts.
- [ ] Open the invoice link in the payment-failed email and pay with `4242` → the invoice is paid, and the dunning run shows recovered.

## Email, PDF, LiteLLM

- [ ] Settings → Email → Send test email: the page says "Sent to" your address and the message arrives in Mailpit. With a wrong `SMTP_PORT`, the page shows the mail server's error instead.
- [ ] Invoice → Email: the message arrives in Mailpit; open it, read it, and check the amount and the links.
- [ ] The emailed PDF opens, and its total, lines, tax and buyer address match the invoice page.
- [ ] Run a real LiteLLM proxy with the callback from [the LiteLLM guide](../integrations/litellm.md) → one completion creates one `input` and one `output` usage event on the customer (plus `cache_read` if the prompt was cached).
