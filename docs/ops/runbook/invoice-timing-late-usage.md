# Invoice timing and late usage

Velox waits a short grace period after a billing period ends before it
builds the invoice, so usage that arrives a little late still counts. This
page is for an invoice that seems late, or usage missing from an invoice.
Back to the [runbook index](index.md).

## Symptom

- A billing period has ended and its invoice is not built yet.
- Or a customer or a usage pipeline reports usage missing from an invoice.

## Why

Velox builds a period's invoice only once `INVOICE_GRACE_PERIOD` (default
1 hour, 0 to 72 hours) has passed after the period ends. Usage that is
stamped inside the period but arrives late is then still billed. The hourly
billing run picks it up after that, so with the default the invoice is
built 1 to 2 hours after the period end. The same wait applies to the final
invoice of a cancel scheduled for a date, and to `POST /v1/billing/run`.

- **The invoice is still dated at the period end.** Its webhook, tax
  record, credits, charge and first payment retry come when it is built.
- **Some actions build the invoice at once, with no grace:** an immediate
  cancel, `POST /v1/subscriptions/{id}/retry-billing` on a subscription
  whose billing is failing, any other change to a subscription whose period
  has ended, and a test-clock advance.
- **A cancel or plan change scheduled for a date** takes effect at that
  close, up to the grace after its date. Until then the subscription is
  `active` on its old plan. `subscription.canceled` is sent at the close,
  with `canceled_at` at the scheduled date. A scheduled resume of paused
  collection that falls after a period end waits for that close too.
- **Once the invoice is built it does not change.** Usage stamped inside
  its period that arrives later is stored, but billed on no invoice, now or
  later. After a spend-threshold invoice, usage stamped before it that
  arrives later is not on the period-end invoice either. Velox does not
  alert on it, and there is no list of such events yet.
- **During the grace the subscription still shows the ended period.** Spend
  thresholds and the live credit balance do not count the new period's
  usage until the invoice is built.
- **A credit grant that expires during the grace** is retired only after
  the grace, so it still pays the invoice of the period it was live in.
  Until then it stays in the ledger balance; no later invoice draws from it.

## Check

```bash
# When is this subscription's period due, and is it failing?
curl -sS "$API/v1/subscriptions/$SUB" -H "Authorization: Bearer $KEY" \
  | jq '{status, current_billing_period_end, next_billing_at, cancel_at, billing_failure}'
# The setting in force is logged at boot, in nanoseconds (3600000000000 = 1 hour):
#   {"msg":"invoice grace period","invoice_grace_period":3600000000000,...}
```

Due means `next_billing_at` (or a `cancel_at` inside the period) is at
least `INVOICE_GRACE_PERIOD` in the past. If it is, and `billing_failure`
is set, go to [Billing failing](billing-failing.md).

## Fix

**Usage that missed a built invoice.** Compare the customer's usage for the
invoice's period with the invoice's usage lines. Pass the period
explicitly. Without `from` and `to`, this endpoint returns the current
cycle, which is the next one once the invoice's period has closed:

```bash
curl -sS "$API/v1/customers/$CUSTOMER/usage?from=$BILLING_PERIOD_START&to=$BILLING_PERIOD_END" \
  -H "Authorization: Bearer $KEY"
```

- `$BILLING_PERIOD_START` and `$BILLING_PERIOD_END` are the invoice's
  `billing_period_start` and `billing_period_end` (RFC3339).
- This endpoint prices only meters on the customer's active or trialing
  subscriptions. For a canceled subscription it returns
  `422 customer_has_no_subscription` without a window, and `meters: []`
  with one. In that case list the raw events:
  `GET /v1/usage-events?customer_id=$CUSTOMER&meter_id=$METER&from=$BILLING_PERIOD_START&to=$BILLING_PERIOD_END`
  (`from` inclusive, `to` exclusive, like the period).
- Bill the difference with a one-off invoice: `POST /v1/invoices`, then
  `POST /v1/invoices/{id}/line-items`, then
  `POST /v1/invoices/{id}/finalize`.
- For a max or last meter, the late events may change nothing; check the
  events before you bill.

**A pipeline that often delivers later than the grace.** Raise
`INVOICE_GRACE_PERIOD` above its worst delay (at most `72h`) and restart.
Every period-end invoice then comes that much later.

**A walkthrough or script that needs a period closed right after it ends.**
Run with `INVOICE_GRACE_PERIOD=0`. (`retry-billing` refuses a subscription
whose billing is not failing, so it cannot close a healthy one early.)

## Verify

After the grace and the next billing tick, `velox_billing_due_subscriptions`
falls back and the invoice exists, dated at the period end.

Related: [ADR-120](../../adr/120-invoice-grace-period-for-late-usage.md).
