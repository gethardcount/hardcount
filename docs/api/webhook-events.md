# Webhook events

Every event Hardcount sends, when it sends it, what its `data` carries, and
an example message. How messages are delivered, signed and retried:
[Webhooks](../webhooks.md). How to build a receiver step by step:
[Receive webhooks](../guides/receive-webhooks.md).

## How to read this page

- **Each family of events carries one resource in `data`**, named in the
  family's heading: `data.invoice`, `data.subscription` and so on. It is the
  object the API returns for that resource, as it was when Hardcount made
  the event.
- **"Also in `data`"** lists the other keys an event carries beside the
  resource. Two are sent only when they apply, and their events say when:
  `reason` on `subscription.canceled` and `resumed_at` on
  `subscription.collection_resumed`. Every other key is always sent, even
  when it is empty, such as `applied` on `dunning.escalated`.
- **"Always sent"** marks an event saved together with the change that
  caused it, so a crash cannot drop it. Any other event can be lost in a
  crash just after the change. See
  [Three rules for your receiver](../webhooks.md#three-rules-for-your-receiver).
- **The examples are the real shape of each message.** A test renders every
  event's `data` from the code that builds it and compares it with the files
  in [internal/domain/testdata/webhook_payloads](../../internal/domain/testdata/webhook_payloads/);
  another test checks each example here against the same code. The examples
  share one fixed invoice, subscription, customer and so on, so a value such
  as an invoice's `status` does not always fit the event: the
  `invoice.finalized` example shows a paid invoice.

Terms: [glossary](../glossary.md).

## Invoices: `data.invoice`

`data.invoice` is the invoice as `GET /v1/invoices/{id}` returns it under
`invoice`. Four fields the API works out when you read it are left out:
`attention`, `recovery_warning`, `billing_period_display` and
`test_clock_id`. Line items are not included; `GET /v1/invoices/{id}`
returns them.

### `invoice.finalized`

An invoice is final and ready to pay. It is sent once per invoice, including
the invoice for a mid-period upgrade, seat increase or added item. A draft
sends it when it is finalized.

Its `amount_due_cents` is the amount before credit, and
`credits_applied_cents` is 0: Hardcount uses credit when it collects the
invoice, just after this event
([more](../webhooks.md#invoicefinalized-shows-the-amount-before-credit)).
Under Stripe Tax, `tax_transaction_id` can be recorded after this event;
`GET /v1/invoices/{id}` shows it.

**Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "invoice.finalized",
  "id": "hc_whevt_golden"
}
```

</details>

### `invoice.paid`

An invoice is fully paid, by card, credit or an offline payment, or a credit
note left nothing due. Its `amount_paid_cents` is what was paid at that
moment: 0 when credit or a credit note covered the rest.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "invoice.paid",
  "id": "hc_whevt_golden"
}
```

</details>

### `invoice.payment_recorded`

Someone recorded a payment made outside Hardcount, such as a bank transfer
or a cheque. `recovered_from` is the status the invoice was paid from:
`finalized` or `uncollectible`.

**Also in `data`:** `recovered_from`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    },
    "recovered_from": "uncollectible"
  },
  "event_type": "invoice.payment_recorded",
  "id": "hc_whevt_golden"
}
```

</details>

### `invoice.marked_uncollectible`

An invoice was written off as bad debt.

**Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "invoice.marked_uncollectible",
  "id": "hc_whevt_golden"
}
```

</details>

### `invoice.voided`

An invoice was voided. Under Stripe Tax, Hardcount reverses the invoice's
tax after it sends this event; `GET /v1/invoices/{id}` shows
`tax_reversed_at` once that is done.

**Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "invoice.voided",
  "id": "hc_whevt_golden"
}
```

</details>

## Payments: `data.invoice`

`data.invoice` is the invoice the payment was for, in the same shape as for
the invoice events. Hardcount has no separate payment object. The invoice
holds the charge's Stripe PaymentIntent (`stripe_payment_intent_id`), its
error (`last_payment_error`) and whether it was paid outside Hardcount
(`paid_out_of_band`).

**The four anomaly events** (`payment.duplicate_charge`,
`payment.amount_mismatch`, `payment.currency_mismatch` and
`payment.received_on_voided_invoice`) carry the charge that raised the
event: `incoming_payment_intent_id` is that charge, and `captured_cents` is
what it took. Match the charge by `incoming_payment_intent_id`, not by the
invoice's `stripe_payment_intent_id`; each event says what the invoice's
one is.

### `payment.succeeded`

A charge was collected. The invoice names its PaymentIntent.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.succeeded",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.failed`

A charge the invoice was waiting on failed. It is sent once per charge
attempt (PaymentIntent). A decline on the hosted payment page does not send
it, because the customer saw it there and can try again.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.failed",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.duplicate_charge`

A second charge succeeded on an invoice that was already paid. Refund it.

The invoice's `stripe_payment_intent_id` is the other charge, the one
Hardcount recorded on the paid invoice. It is left out when Hardcount
recorded none, such as when credit paid the invoice or it was paid outside
Hardcount (`paid_out_of_band` is true).

**Also in `data`:** `incoming_payment_intent_id`, `captured_cents`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "captured_cents": 10825,
    "incoming_payment_intent_id": "pi_second",
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.duplicate_charge",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.amount_mismatch`

A charge settled for a different amount than the invoice owed. The invoice
was just paid by this charge, so its `stripe_payment_intent_id` is this
charge, and `amount_paid_cents` is what Hardcount booked.

**Also in `data`:** `incoming_payment_intent_id`, `captured_cents`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "captured_cents": 10825,
    "incoming_payment_intent_id": "pi_second",
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.amount_mismatch",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.currency_mismatch`

A charge settled in a different currency than the invoice's.
`charge_currency` is the charge's currency, in upper case; compare it with
the invoice's `currency`. The payment is recorded as usual and nothing is
converted. Review it in Stripe. The invoice is as it was when the charge
arrived: its `stripe_payment_intent_id` may be this charge, an earlier one,
or none.

**Also in `data`:** `incoming_payment_intent_id`, `captured_cents`,
`charge_currency`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "captured_cents": 10825,
    "charge_currency": "EUR",
    "incoming_payment_intent_id": "pi_second",
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.currency_mismatch",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.received_on_voided_invoice`

Money arrived for a voided invoice. Refund it. The invoice is as it was
when the charge arrived: its `stripe_payment_intent_id` may be this charge,
an earlier one, or none.

**Also in `data`:** `incoming_payment_intent_id`, `captured_cents`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "captured_cents": 10825,
    "incoming_payment_intent_id": "pi_second",
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.received_on_voided_invoice",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.dispute_opened`

A customer disputed a card payment with their bank (a chargeback), and
Hardcount recorded it against the invoice. It is sent once per dispute.

`dispute` is the dispute as `GET /v1/invoices/{id}/disputes` lists it, with
every field it has there, including `stripe_dispute_id`, `status`, `reason`,
`amount_cents`, `currency`, `evidence_due_by` (the deadline to respond in
Stripe), `opened_at`, and `closed_at` once it is closed. What a dispute does
and how to turn on Stripe's dispute events:
[Card disputes](../ops/runbook/card-disputes.md).

**Always sent.** **Also in `data`:** `dispute`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "dispute": {
      "amount_cents": 10825,
      "created_at": "2026-10-01T12:00:00Z",
      "currency": "USD",
      "evidence_due_by": "2026-10-08T12:00:00Z",
      "id": "hc_dsp_golden",
      "invoice_id": "hc_inv_golden",
      "livemode": false,
      "opened_at": "2026-10-01T12:00:00Z",
      "reason": "fraudulent",
      "status": "needs_response",
      "stripe_charge_id": "ch_golden",
      "stripe_dispute_id": "du_golden",
      "stripe_payment_intent_id": "pi_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.dispute_opened",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.dispute_closed`

A recorded dispute closed. It is sent again if Stripe turns a loss into a
win. Its `dispute.status` is `won`, `lost`, `warning_closed` (an inquiry
that never became a chargeback) or `prevented`. When the first Stripe event
Hardcount receives is already the close, both dispute events are sent with
the closed status, in either order.

**Always sent.** **Also in `data`:** `dispute`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "dispute": {
      "amount_cents": 10825,
      "created_at": "2026-10-01T12:00:00Z",
      "currency": "USD",
      "evidence_due_by": "2026-10-08T12:00:00Z",
      "id": "hc_dsp_golden",
      "invoice_id": "hc_inv_golden",
      "livemode": false,
      "opened_at": "2026-10-01T12:00:00Z",
      "reason": "fraudulent",
      "status": "needs_response",
      "stripe_charge_id": "ch_golden",
      "stripe_dispute_id": "du_golden",
      "stripe_payment_intent_id": "pi_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    }
  },
  "event_type": "payment.dispute_closed",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment.refunded_in_stripe`

A payment on the invoice was refunded in Stripe, outside Hardcount (the
Stripe dashboard, or Stripe's API called directly), and Hardcount recorded
it. It is sent once per refund, when the outcome is decided: at once for
most refunds, and for a refund first seen pending, when Stripe says it
succeeded, failed or was canceled.

`refund` is the refund as `GET /v1/invoices/{id}/stripe-refunds` lists it,
with `stripe_refund_id`, `stripe_payment_intent_id`, `amount_cents`,
`currency`, Stripe's `status`, and `outcome`, what Hardcount did with it:

- `credit_note`: Hardcount recorded it as the issued credit note
  `credit_note_id`, whose card refund is this refund. Hardcount made no
  refund call for it, and the note reverses its share of the tax.
- `other_payment`: a refund of a payment that did not pay the invoice (a
  second charge, or money on a voided invoice). No credit note.
- `needs_review`: a refund of the invoice's own payment that Hardcount did
  not record; `review_reason` says why. The invoice shows a notice until
  the operator records it with a credit note or dismisses it.
- `not_refunded`: it failed or was canceled before it succeeded. No money
  moved.

The invoice is as it was: a refund never changes its status or amounts. A
later change to the refund, such as a failure, sends nothing more; a
recorded note's `refund_status` follows it. Refunds Hardcount makes itself
send no event. Hardcount hears of a refund made in Stripe only with
`refund.created` on the Stripe webhook endpoint:
[A refund made in Stripe](../ops/runbook/refund-made-in-stripe.md).

**Always sent.** **Also in `data`:** `refund`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "invoice": {
      "amount_due_cents": 0,
      "amount_paid_cents": 10825,
      "billing_period_end": "2026-10-01T12:00:00Z",
      "billing_period_start": "2026-09-01T12:00:00Z",
      "billing_reason": "subscription_cycle",
      "billing_timezone": "UTC",
      "created_at": "2026-10-01T12:00:00Z",
      "credits_applied_cents": 0,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "discount_cents": 0,
      "due_at": "2026-10-01T12:00:00Z",
      "id": "hc_inv_golden",
      "invoice_number": "HC-000042",
      "is_simulated": false,
      "issued_at": "2026-10-01T12:00:00Z",
      "metadata": {
        "order": "A-1"
      },
      "net_payment_term_days": 0,
      "paid_at": "2026-10-01T12:01:00Z",
      "paid_out_of_band": false,
      "payment_card_brand": "visa",
      "payment_card_last4": "4242",
      "payment_status": "succeeded",
      "status": "paid",
      "stripe_payment_intent_id": "pi_golden",
      "subscription_id": "hc_sub_golden",
      "subtotal_cents": 10000,
      "tax_amount_cents": 825,
      "tax_country": "US",
      "tax_name": "Sales tax",
      "tax_provider": "manual",
      "tax_rate": 8.25,
      "tax_status": "ok",
      "tenant_id": "hc_ten_golden",
      "total_amount_cents": 10825,
      "updated_at": "2026-10-01T12:01:00Z"
    },
    "refund": {
      "amount_cents": 4000,
      "created_at": "2026-10-01T12:00:00Z",
      "credit_note_id": "hc_cn_golden",
      "currency": "USD",
      "id": "hc_srf_golden",
      "invoice_id": "hc_inv_golden",
      "livemode": false,
      "outcome": "credit_note",
      "reason": "requested_by_customer",
      "refund_created_at": "2026-10-01T12:00:00Z",
      "status": "succeeded",
      "stripe_charge_id": "ch_golden",
      "stripe_payment_intent_id": "pi_golden",
      "stripe_refund_id": "re_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "payment.refunded_in_stripe",
  "id": "hc_whevt_golden"
}
```

</details>

## Payment methods: `data.payment_method`

`data.payment_method` is the saved card as
`GET /v1/customers/{id}/payment-methods` lists it: its brand, last 4
digits, expiry and whether it is the default. A Stripe event that arrives
after the card was removed in Hardcount leaves it removed and sends neither
event.

### `payment_method.attached`

A customer saved a payment method. Invoices waiting for one are charged on
the next billing run. `stripe_payment_method_id` is the card's Stripe ID.

**Also in `data`:** `stripe_payment_method_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "payment_method": {
      "card_brand": "visa",
      "card_exp_month": 12,
      "card_exp_year": 2030,
      "card_last4": "4242",
      "created_at": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "id": "hc_pm_golden",
      "is_default": true,
      "type": "card"
    },
    "stripe_payment_method_id": "pm_golden"
  },
  "event_type": "payment_method.attached",
  "id": "hc_whevt_golden"
}
```

</details>

### `payment_method.updated`

A customer's payment method was set up or replaced through Stripe Checkout.
`stripe_customer_id` is the Stripe customer it was saved to.

**Also in `data`:** `stripe_customer_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "payment_method": {
      "card_brand": "visa",
      "card_exp_month": 12,
      "card_exp_year": 2030,
      "card_last4": "4242",
      "created_at": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "id": "hc_pm_golden",
      "is_default": true,
      "type": "card"
    },
    "stripe_customer_id": "cus_golden"
  },
  "event_type": "payment_method.updated",
  "id": "hc_whevt_golden"
}
```

</details>

## Subscriptions: `data.subscription`

`data.subscription` is the subscription with its items, as
`GET /v1/subscriptions/{id}` returns it, without
`current_billing_period_display` (a date range the API writes out when you
read it). A cancel's time is the subscription's `canceled_at`, and a
schedule's is its `cancel_at` and `cancel_effective_at`.

### `subscription.created`

A subscription was created.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.created",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.activated`

A draft subscription was activated.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.activated",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.canceled`

A subscription was canceled. `canceled_by` is `operator`, `customer`,
`schedule`, `dunning` or `system`. A cancel by dunning says `dunning`, also
when a test-clock advance ran it. A cancel at trial end carries
`reason: trial_end_cancel`; other cancels have no `reason`.

**Always sent.** **Also in `data`:** `canceled_by`, `reason`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "canceled_by": "schedule",
    "reason": "trial_end_cancel",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.canceled",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.trial_ended`

A trial ended. `triggered_by` is `operator` or `schedule`, and `ended_at`
is when it ended.

**Always sent.** **Also in `data`:** `ended_at`, `triggered_by`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "ended_at": "2026-10-01T12:00:00Z",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "triggered_by": "schedule"
  },
  "event_type": "subscription.trial_ended",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.trial_extended`

A trial's end date moved later. The new end is the subscription's
`trial_end_at`. `triggered_by` is `operator`.

**Also in `data`:** `triggered_by`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "triggered_by": "operator"
  },
  "event_type": "subscription.trial_extended",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.cancel_scheduled`

A cancel at period end was scheduled.

**Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.cancel_scheduled",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.cancel_cleared`

A scheduled cancel was removed. Clearing when no cancel is scheduled sends
nothing.

**Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.cancel_cleared",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.collection_paused`

Collection was paused: invoices stay drafts. The subscription's
`pause_collection` says how and until when. `triggered_by` is `operator` or
`dunning` (payment retries ran out and your policy pauses).

Dunning's pause is saved with the change, so a crash cannot drop its event.
Dunning never resumes collection itself: the pause lasts until that invoice
is paid (`subscription.collection_resumed` with `triggered_by: payment`) or
you resume it. On a canceled subscription, paying the invoice does not lift
the pause; resume it yourself.

**Also in `data`:** `triggered_by`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "triggered_by": "operator"
  },
  "event_type": "subscription.collection_paused",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.collection_resumed`

Collection resumed; a resume call on a subscription that is not paused sends
nothing. `triggered_by` is `operator`, `schedule` (its resume date came) or
`payment` (a payment ended a pause that dunning set).

- A payment lifts the pause when it is recorded. This event follows on the
  next scheduled billing run, once any period that ended under the pause
  has closed.
- A `schedule` or `payment` resume carries `resumed_at`, and is saved with
  the change, so a crash cannot drop it. An `operator` resume has no
  `resumed_at`.
- Each pause that ends sends one, with its own trigger, even when a new
  pause or a resume call reaches it first. That resume call sends no
  `operator` resume.

**Also in `data`:** `triggered_by`, `resumed_at`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "resumed_at": "2026-10-01T12:00:00Z",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "triggered_by": "schedule"
  },
  "event_type": "subscription.collection_resumed",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.item.added`

A plan was added to the subscription. `item_id` names the new item.

**Also in `data`:** `item_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "item_id": "hc_si_golden",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.item.added",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.item.updated`

An item's plan or quantity changed. `item_id` names the item.

**Also in `data`:** `item_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "item_id": "hc_si_golden",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.item.updated",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.item.removed`

An item was removed. `item_id` names it; it is no longer in `items`.

**Also in `data`:** `item_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "item_id": "hc_si_golden",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.item.removed",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.pending_change.scheduled`

A plan change for the next billing period was scheduled. The item's
`pending_plan_id` and `pending_plan_effective_at` show it.

**Also in `data`:** `item_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "item_id": "hc_si_golden",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.pending_change.scheduled",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.pending_change.canceled`

A scheduled plan change was canceled.

**Also in `data`:** `item_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "item_id": "hc_si_golden",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.pending_change.canceled",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.pending_change.applied`

A scheduled plan change was applied at the period end. The item's `plan_id`
is the new plan, `old_plan_id` the one it left, and `applied_at` when. The
subscription is as it was before the close that starts the next period.

**Also in `data`:** `item_id`, `old_plan_id`, `applied_at`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "applied_at": "2026-10-01T12:00:00Z",
    "item_id": "hc_si_golden",
    "old_plan_id": "hc_pln_basic",
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.pending_change.applied",
  "id": "hc_whevt_golden"
}
```

</details>

### `subscription.threshold_crossed`

Usage crossed a billing threshold, so an invoice was made early.

- `invoice_id`: the invoice made early.
- `running_subtotal`: the spend compared with the threshold, in cents.
- `crossed_amount`: whether the amount threshold was crossed.
- `crossed_item_id`: the item whose usage threshold was crossed, empty if
  none.

**Also in `data`:** `invoice_id`, `running_subtotal`, `crossed_amount`,
`crossed_item_id`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "crossed_amount": true,
    "crossed_item_id": "",
    "invoice_id": "hc_inv_golden",
    "running_subtotal": 50100,
    "subscription": {
      "activated_at": "2026-09-01T12:00:00Z",
      "billing_thresholds": {
        "amount_gte": 50000,
        "item_thresholds": []
      },
      "billing_time": "calendar",
      "cancel_at_period_end": false,
      "code": "acme-pro",
      "created_at": "2026-09-01T12:00:00Z",
      "current_billing_period_end": "2026-11-01T12:00:00Z",
      "current_billing_period_start": "2026-10-01T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "display_name": "Acme Pro",
      "id": "hc_sub_golden",
      "items": [
        {
          "created_at": "2026-09-01T12:00:00Z",
          "id": "hc_si_golden",
          "pending_plan_effective_at": "2026-11-01T12:00:00Z",
          "pending_plan_id": "hc_pln_team",
          "plan_id": "hc_pln_pro",
          "quantity": 3,
          "subscription_id": "hc_sub_golden",
          "tenant_id": "hc_ten_golden",
          "updated_at": "2026-10-01T12:00:00Z"
        }
      ],
      "next_billing_at": "2026-11-01T12:00:00Z",
      "started_at": "2026-09-01T12:00:00Z",
      "status": "active",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "subscription.threshold_crossed",
  "id": "hc_whevt_golden"
}
```

</details>

## Customers: `data.customer`

`data.customer` is the customer as `GET /v1/customers/{id}` returns it.

### `customer.email_bounced`

An email to a customer bounced, and Hardcount stopped emailing that address.
`reason` is the email provider's.

**Also in `data`:** `reason`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "customer": {
      "created_at": "2026-08-02T12:00:00Z",
      "display_name": "Acme Inc",
      "email": "billing@acme.example",
      "email_bounce_reason": "mailbox does not exist",
      "email_last_bounced_at": "2026-10-01T12:00:00Z",
      "email_status": "bounced",
      "external_id": "acme-1",
      "id": "hc_cus_golden",
      "status": "active",
      "stripe_customer_id": "cus_golden",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "reason": "mailbox does not exist"
  },
  "event_type": "customer.email_bounced",
  "id": "hc_whevt_golden"
}
```

</details>

### `customer.email_complained`

A customer marked an email from Hardcount as spam, and Hardcount stopped
emailing that address. `reason` is the email provider's.

**Also in `data`:** `reason`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "customer": {
      "created_at": "2026-08-02T12:00:00Z",
      "display_name": "Acme Inc",
      "email": "billing@acme.example",
      "email_bounce_reason": "mailbox does not exist",
      "email_last_bounced_at": "2026-10-01T12:00:00Z",
      "email_status": "bounced",
      "external_id": "acme-1",
      "id": "hc_cus_golden",
      "status": "active",
      "stripe_customer_id": "cus_golden",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "reason": "mailbox does not exist"
  },
  "event_type": "customer.email_complained",
  "id": "hc_whevt_golden"
}
```

</details>

## Dunning: `data.dunning_run`

Dunning is Hardcount retrying a failed payment on a schedule.
`data.dunning_run` is the run as `GET /v1/dunning/runs/{id}` returns it
under `run`: its invoice, state, attempts and how it was resolved.

### `dunning.started`

Payment retries started for an invoice.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "dunning_run": {
      "attempt_count": 4,
      "created_at": "2026-09-17T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "id": "hc_dr_golden",
      "invoice_id": "hc_inv_golden",
      "last_attempt_at": "2026-10-01T11:00:00Z",
      "paused": false,
      "policy_id": "hc_dpol_golden",
      "reason": "payment_failed",
      "resolution": "retries_exhausted",
      "resolved_at": "2026-10-01T12:00:00Z",
      "state": "escalated",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "dunning.started",
  "id": "hc_whevt_golden"
}
```

</details>

### `dunning.escalated`

Payment retries ran out, and the final action from your policy was taken.
`final_subscription_action` and `final_invoice_action` are your policy's;
`applied` is what was done to this invoice, such as
`subscription_canceled+invoice_marked_uncollectible`, and empty when nothing
applied.

**Also in `data`:** `final_subscription_action`, `final_invoice_action`,
`applied`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "applied": "subscription_canceled+invoice_marked_uncollectible",
    "dunning_run": {
      "attempt_count": 4,
      "created_at": "2026-09-17T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "id": "hc_dr_golden",
      "invoice_id": "hc_inv_golden",
      "last_attempt_at": "2026-10-01T11:00:00Z",
      "paused": false,
      "policy_id": "hc_dpol_golden",
      "reason": "payment_failed",
      "resolution": "retries_exhausted",
      "resolved_at": "2026-10-01T12:00:00Z",
      "state": "escalated",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    },
    "final_invoice_action": "mark_uncollectible",
    "final_subscription_action": "cancel"
  },
  "event_type": "dunning.escalated",
  "id": "hc_whevt_golden"
}
```

</details>

### `dunning.resolved`

Payment retries ended early, for example because the invoice was paid or
someone resolved it by hand. The run's `resolution` says why.

**Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "dunning_run": {
      "attempt_count": 4,
      "created_at": "2026-09-17T12:00:00Z",
      "customer_id": "hc_cus_golden",
      "id": "hc_dr_golden",
      "invoice_id": "hc_inv_golden",
      "last_attempt_at": "2026-10-01T11:00:00Z",
      "paused": false,
      "policy_id": "hc_dpol_golden",
      "reason": "payment_failed",
      "resolution": "retries_exhausted",
      "resolved_at": "2026-10-01T12:00:00Z",
      "state": "escalated",
      "tenant_id": "hc_ten_golden",
      "updated_at": "2026-10-01T12:00:00Z"
    }
  },
  "event_type": "dunning.resolved",
  "id": "hc_whevt_golden"
}
```

</details>

## Credit: `data.credit_balance`

`data.credit_balance` is the customer's credit balance in one currency, as
`GET /v1/credits/balance/{customer_id}` lists it, after the change that sent
the event. The balance events follow the settled balance, not usage, so do
not use them to decide whether to keep serving a customer:
[why](../webhooks.md#the-credit-events-follow-the-settled-balance).

### `credit.balance_low`

The settled credit balance in one currency fell below your account's
threshold, `threshold_cents`.

**Always sent.** **Also in `data`:** `threshold_cents`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "credit_balance": {
      "balance_cents": 900,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "total_deducted": 500,
      "total_expired": 100,
      "total_granted": 10000,
      "total_retired": 500,
      "total_used": 8000
    },
    "threshold_cents": 1000
  },
  "event_type": "credit.balance_low",
  "id": "hc_whevt_golden"
}
```

</details>

### `credit.balance_depleted`

The settled credit balance in one currency reached zero or less.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "credit_balance": {
      "balance_cents": 900,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "total_deducted": 500,
      "total_expired": 100,
      "total_granted": 10000,
      "total_retired": 500,
      "total_used": 8000
    }
  },
  "event_type": "credit.balance_depleted",
  "id": "hc_whevt_golden"
}
```

</details>

### `credit.balance_recovered`

The settled credit balance in one currency went above zero again.

**Always sent.** **Also in `data`:** nothing.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "credit_balance": {
      "balance_cents": 900,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "total_deducted": 500,
      "total_expired": 100,
      "total_granted": 10000,
      "total_retired": 500,
      "total_used": 8000
    }
  },
  "event_type": "credit.balance_recovered",
  "id": "hc_whevt_golden"
}
```

</details>

### `credit.commit_retired`

A credit note took back prepaid credit.

- `grant_id`: the prepaid credit grant.
- `funding_invoice_id`: the invoice that bought it.
- `credit_note_id`: the credit note that took the credit back.
- `retired_cents`: the credit taken back.
- `refunded_gross_cents`: the money the credit note returns.
- `remaining_after_cents`: what is left on the grant.

**Always sent.** **Also in `data`:** `grant_id`, `funding_invoice_id`,
`credit_note_id`, `retired_cents`, `refunded_gross_cents`,
`remaining_after_cents`.

<details>
<summary>Example message</summary>

```json
{
  "created_at": "2026-10-01T12:00:00Z",
  "data": {
    "credit_balance": {
      "balance_cents": 900,
      "currency": "USD",
      "customer_id": "hc_cus_golden",
      "total_deducted": 500,
      "total_expired": 100,
      "total_granted": 10000,
      "total_retired": 500,
      "total_used": 8000
    },
    "credit_note_id": "hc_cn_golden",
    "funding_invoice_id": "hc_inv_commit",
    "grant_id": "hc_ccl_golden",
    "refunded_gross_cents": 500,
    "remaining_after_cents": 0,
    "retired_cents": 500
  },
  "event_type": "credit.commit_retired",
  "id": "hc_whevt_golden"
}
```

</details>
