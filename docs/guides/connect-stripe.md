# Connect Stripe and take card payments

This guide connects your Stripe account to Hardcount, saves a test
customer's card, and follows the first charge. Hardcount works out each
bill and makes the invoice; Stripe charges the card, and the money goes
straight to your Stripe account.

Do it in test mode first. Each mode, test and live, has its own Stripe
connection, and you connect live mode the same way when you are ready.

## Before you start

- Hardcount running, and a test secret key in `HARDCOUNT_KEY`
  ([first invoice](../first-invoice.md#before-you-start)).
- A Stripe account, in test mode.
- An address Stripe can reach. Stripe sends its webhooks (messages about
  payments) to Hardcount over the internet. On your own computer, the
  Stripe CLI can forward them: see step 2.

How payments work, in short: [Payments](../concepts/payments.md).

## 1. Save your Stripe keys

1. In Stripe, open **Developers → API keys**. Copy the secret key
   (`sk_test_…`, or a restricted key, `rk_test_…`) and the publishable key
   (`pk_test_…`).
2. In Hardcount, open **Settings → Payments**, with the dashboard in test
   mode. Under **Connect your Stripe API keys**, paste both keys and click
   **Connect test mode**.

   **You see:** "Verified as" and your Stripe account's name.

Or with the API. Keep the keys out of your shell history:

```bash
umask 077 && cat > /tmp/stripe-keys.json <<'EOF'
{"livemode": false, "secret_key": "sk_test_…", "publishable_key": "pk_test_…"}
EOF
curl -s -X POST "$HARDCOUNT/v1/settings/stripe" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d @/tmp/stripe-keys.json | jq '{id, stripe_account_name, verified_at, last_verified_error}'
rm -f /tmp/stripe-keys.json
```

**You see:** the connection's `id` (`hc_spc_…`), your account's name, a
`verified_at` time and `"last_verified_error": null`. A
`last_verified_error` with a message means Stripe refused the key: the keys
are saved, but nothing can be charged until you save working ones.

## 2. Send Stripe's events to Hardcount

Hardcount learns that a card was saved, or a payment succeeded, from
Stripe's webhooks. Each connection has its own webhook address:
`<your Hardcount address>/v1/webhooks/stripe/<id>`, with the `id` from
step 1. **Settings → Payments** shows it under **Deliver Stripe events to
Hardcount**.

1. In Stripe, open **Developers → Webhooks** and add an endpoint with that
   address. Pick these 12 events, the ones Hardcount lists under the
   address:

   - `payment_intent.succeeded`, `payment_intent.payment_failed`
   - `checkout.session.completed`
   - `setup_intent.succeeded`, `setup_intent.setup_failed`
   - `charge.refund.updated`, `refund.created`, `refund.updated`,
     `refund.failed`
   - `charge.dispute.created`, `charge.dispute.updated`,
     `charge.dispute.closed`

2. Stripe shows the endpoint's signing secret (`whsec_…`). Paste it into
   Hardcount and click **Finish setup**. Or send it with
   `PATCH /v1/settings/stripe/test/webhook` and `{"webhook_secret": "whsec_…"}`.

   **You see:** the card reads **Stripe test mode connected**, with a
   **Ready** badge.

On your own computer, use the Stripe CLI instead of step 1:

```bash
stripe listen --forward-to http://localhost:8080/v1/webhooks/stripe/hc_spc_…
```

**You see:** the CLI prints a webhook secret (`whsec_…`). Use it in step 2.

Until the secret is saved, Hardcount answers Stripe's webhooks with `404`.
A customer would then see "card saved" on Stripe's page while Hardcount
never learns of the card.

## 3. Save a customer's card

The customer types their card on a Stripe page, never on yours. You give
them a link to it:

- **From the dashboard:** on the customer's page, **Payment methods**, click
  **Add payment method**, then email the link or copy it.
- **From the API:**

  ```bash
  curl -s -X POST "$HARDCOUNT/v1/customers/$CUSTOMER_ID/payment-methods/setup-session" \
    -H "Authorization: Bearer $HARDCOUNT_KEY" \
    -H "Content-Type: application/json" \
    -d '{}' | jq -r .url
  ```

  **You see:** a `https://checkout.stripe.com/…` address. Send it to the
  customer, or open it yourself in test mode. Add `"return_url"` to the
  body to choose where Stripe sends them afterwards.
- **By email from Hardcount:**
  `POST /v1/customers/{customer_id}/payment-methods/send-setup-email`
  sends the link to the customer's email address. It needs email set up
  ([Set up email](../ops/email-setup.md)).

In test mode, save Stripe's test card `4242 4242 4242 4242`, with any
future expiry, any CVC and any postal code.

**You see:** on the customer's page, **Payment methods** lists **Visa
····4242**. `GET /v1/customers/{customer_id}/payment-methods` lists it too.
A card saved this way becomes the customer's default, the one Hardcount
charges.

A customer can also save a card by paying an invoice with **Pay** on its
hosted invoice page.

## 4. The first charge

Put the customer on a plan and let an invoice be issued: in test mode, on a
[test clock](../concepts/test-clocks.md), move the clock past the period
end. Right after the invoice is issued, Hardcount uses any credit the
customer holds, then charges the card for the rest.

**You see:**

- the invoice reads **Paid** in the dashboard, and the payment shows in
  your Stripe dashboard, under **Payments**;
- your app gets `invoice.paid` and `payment.succeeded`
  ([webhooks](../webhooks.md)).

To see a failure, save `4000 0000 0000 0341` on another customer. Stripe
saves it, then declines every charge. The invoice's payment goes to
`failed`, `payment.failed` is sent, and dunning starts retrying it.

## 5. Disputes

A dispute is a customer asking their bank to reverse a card payment. With
the three `charge.dispute.*` events in step 2, Hardcount records each
dispute on its invoice, which stays paid, and sends
`payment.dispute_opened`, then `payment.dispute_closed` when Stripe closes
it. Read them with `GET /v1/invoices/{id}/disputes`. Respond to a dispute
in Stripe, before its deadline.

More: [Card disputes](../ops/runbook/card-disputes.md).

## Addresses Stripe sends customers back to

After a customer saves a card, Stripe sends them to a page of your
dashboard. Set these on your Hardcount server, or customers land on
`http://localhost:5173`, which exists only in local development. The card
is saved either way.

| Setting | The page after |
|---|---|
| `CUSTOMER_PORTAL_URL` | a setup link from step 3. Hardcount adds `/payment-method-added`. |
| `PAYMENT_UPDATE_RETURN_URL` | the "add a payment method" email Hardcount sends when an invoice finds no card. A full address. |
| `HOSTED_INVOICE_BASE_URL` | **Pay** on the hosted invoice page. |

More: [Settings](../settings.md).

**Next:** go live: [Production on one VM](../production-one-vm.md#6-connect-stripe).
To test a whole charge, from saving a card to a refund:
[Stripe end-to-end test](../dev/stripe-end-to-end-test.md).
