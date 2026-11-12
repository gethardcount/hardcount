# Tax calculation

Hardcount adds tax to each invoice in one of three ways: no tax, one flat rate, or Stripe Tax.

Hardcount does not decide which taxes you owe. You register for tax with the authorities (and, for Stripe Tax, in your Stripe account), and Hardcount applies what you set up. That works for registrations such as:

- US sales tax,
- VAT or GST in your own country,
- OIDAR, a registration for selling digital services across borders.

## Pick a tax provider

Set it in the dashboard under **Settings → Tax → Tax calculation → Tax provider**. Each account picks one.

| Choice | What it does | Pick it when |
|---|---|---|
| **None** | Invoices have no tax. | You do not collect tax, for example early business-to-business sales where you are not registered. |
| **Manual (flat rate)** | One rate on every line of every invoice, with a name you choose, such as "VAT" or "Sales Tax". | You sell in one place with one legal rate, or you have not set up Stripe Tax. |
| **Stripe Tax** | Stripe works out the rate for each customer's location, and records each invoice in Stripe's tax reports. | You sell in several countries or US states, or you need Stripe's tax reports to include Hardcount's invoices. Needs Stripe connected under **Settings → Payments**. |

One line is never taxed under any provider: a prepaid-commit purchase, where the customer pays up front for credit. It is not sent to Stripe Tax either, so a commit-only invoice has no Stripe Tax record. The usage the credit pays for is taxed when it is invoiced ([why](../ai-pricing.md#tax-on-prepaid-credit)).

If Stripe Tax fails, the invoice waits; Hardcount never falls back to a guessed rate ([what to do](#when-tax-cannot-be-worked-out)).

**Switching away from Stripe Tax.** Invoices Stripe Tax already taxed stay Stripe's, whatever provider is selected now: Hardcount still records their tax in Stripe's tax reports (a tax commit), and reverses it there when you void or credit one (a tax reversal). So keep Stripe connected until those invoices are settled, voided or credited. With Stripe disconnected, the commit or reversal fails and is tried again, with a wait between tries, until Stripe is connected.

### Manual (flat rate)

1. Choose **Manual (flat rate)**.
2. Enter the **Tax name**, shown on each invoice line.
3. Enter the **Tax rate**: a percent from 0 to 100, with up to 4 decimal places, such as `8.8750`. `0` turns tax off.
4. Save.

   **You see:** the next invoice has a tax line with your name and rate.

### Stripe Tax

1. Connect Stripe under **Settings → Payments**, in each mode you use. Test mode and live mode use separate Stripe keys, so connect both.
2. In your Stripe account, add your tax registrations.
3. Choose **Stripe Tax** and save. You can also set a **Default product tax code**. Stripe uses it for usage, and for the base fee of a plan that does not name its own `tax_code`. Left empty, Hardcount saves `txcd_10103001`, Stripe's code for software as a service bought by a business.
4. Make sure each customer has a full address, at least the country. Stripe Tax cannot work out tax without it.

   **You see:** the next invoice has a tax line that matches Stripe's calculation.

## Options that work with every provider

- **Tax-inclusive pricing** (Settings → Tax). When it is on, your prices already include tax, and Hardcount shows the tax part inside them. When it is off, tax is added on top.
- **A customer's tax status**, set in the customer's **Billing Profile**:
  - **Standard**: tax applies as usual.
  - **Exempt**: no tax. Hardcount asks for the reason.
  - **Reverse charge**: no tax on the invoice, because the business customer pays the VAT or GST themselves in their own country. Hardcount asks for the customer's tax ID.
- **Tax IDs**, such as a VAT or GST number, are shown on the invoice. Hardcount checks only that an ID has the right shape, not that the tax authority knows it.

## When tax cannot be worked out

This happens only with Stripe Tax. The invoice waits: it is not issued, emailed or charged. The invoice page shows a notice with the reason and a retry button. The button reads **Retry now** when the provider could not be reached, or when Stripe is connected now but was not at the first try. Otherwise it reads **Retry tax**.

| The notice says | What to do | Retried by itself? |
|---|---|---|
| The customer's address or tax ID is missing or invalid | Fix the customer's billing profile, then click **Retry tax** | No |
| The tax provider can't be reached right now | Check [status.stripe.com](https://status.stripe.com). Hardcount retries; click **Retry now** to try at once. | Yes, up to 8 times |
| The tax provider rejected the API key | Update the Stripe keys under **Settings → Payments**, then retry | No |
| Stripe isn't connected in this mode | Connect Stripe for this mode (test or live). Hardcount then queues the calculation itself, and the notice offers only **Retry now**, to run it at once. | Yes, up to 8 times |
| The tax provider can't calculate tax for this customer's region | Add a tax registration for that region in Stripe, then retry | No |
| Tax calculation failed for an unknown reason | Retry. If it keeps failing, check the server log. | Yes, up to 8 times |

When the tax is worked out on an invoice billing made, Hardcount finalizes it in the same step. If that finalize fails, the tax is not saved: the invoice keeps its notice and its last tax error, and the server log says why (`tax retry resolved the tax but did not finalize`). The try is counted, toward the 8 for an error Hardcount retries by itself.

While the invoice waits like this, its subscription cannot be canceled, change plan or lower a quantity (`funding_invoice_tax_pending`), and **Finalize** is refused. That lasts until a retry finalizes the invoice, or you void it; the 8-try limit stops the automatic retries, not the wait. If the finalize keeps failing, fix the cause the server log names and click **Retry tax**, or void the draft and bill the period on a [one-off invoice](../concepts/invoices.md#when-hardcount-makes-an-invoice).

After 8 automatic tries, Hardcount stops and the notice says so. Click the notice's retry button to try again: **Retry now** when the provider could not be reached or Stripe is now connected, **Retry tax** otherwise.

Stripe keeps a tax calculation for 90 days, and Hardcount finalizes with one until 1 hour before it expires. When billing builds a subscription's invoice, a draft too, it records the calculation in Stripe's tax reports at once, so most drafts can wait. A draft whose calculation is not recorded can wait only until the calculation expires. That happens when Stripe failed at the moment billing built the draft, or when a retry worked the tax out. If such a draft waits that long, for example because its subscription has collection paused, finalize answers `tax_calculation_expired`. Retry the tax (`POST /v1/invoices/{id}/retry-tax`), then finalize.

When a tax commit fails on a finalized invoice, Hardcount retries it until the calculation expires. The cause can last for days, such as a Stripe key that cannot write tax transactions, or a Stripe Tax incident. The tax is recorded after the cause is fixed, any time within the 90 days. Allow up to about a day after the fix: Stripe repeats its saved answer to a request, an error included, for 24 hours. After each failed try Hardcount waits before the next: an eighth of the time since the invoice was last written, at least 1 hour and at most 12 hours. The tax reversal of a voided invoice waits the same way. The invoice's `tax_next_retry_at` is the time of the next try. A commit made late is posted in Stripe's reporting period of the day it is made, not of the invoice's date, so tax for an invoice of one month can appear in the next month's report.

With tax-inclusive pricing, the retry keeps the total at the sum of the line prices, and only the tax inside them follows the new calculation. A draft that v0.9.2 or earlier already refreshed is the exception: see the next section.

### Invoices an earlier version under-billed

**Who is affected:** accounts on Stripe Tax with tax-inclusive pricing that ran Hardcount v0.2.0 to v0.9.2.

**Why.** With tax-inclusive pricing, billing stores each taxed line of a subscription's invoice at its price less the tax. On those versions, a retry that refreshed a draft's expired calculation taxed that lowered amount as if it were the price. The tax came out a second time, so the total fell by about the tax. If the draft was then finalized, the customer was charged that much less. Two more shapes come from the same cause:

- A refresh that failed left the draft waiting for tax, with the lowered amounts.
- On a draft whose tax had been retried before, a refresh at a new rate lowered the total with a discount (`discount_cents`) of about the old tax.

Later versions tax the line prices. They do not restore a lowered draft: a retry taxes what the lines charge, and they already charge the lowered amounts. On a release after v0.9.3, a retry also drops a draft's `discount_cents`: no invoice has a discount of its own.

**Check.** Run this before you upgrade, in `psql` as the database owner ([why the first line](runbook/index.md#before-you-start)). After the upgrade, a retry changes the lines it looks for. So if you upgraded already, it still finds each invoice that no retry has touched since.

```sql
SET app.bypass_rls = 'on';
SELECT i.tenant_id, i.livemode, i.invoice_number, i.status, i.tax_status,
       i.discount_cents, i.total_amount_cents
FROM invoices i
JOIN tenant_settings s ON s.tenant_id = i.tenant_id
WHERE s.tax_inclusive
  AND i.tax_provider = 'stripe_tax'
  AND i.billing_reason IS NOT NULL AND i.billing_reason <> 'manual'
  AND i.status <> 'voided'
  AND (
    (i.status = 'draft' AND i.tax_status = 'pending')
    OR EXISTS (
      SELECT 1 FROM invoice_line_items li
      WHERE li.invoice_id = i.id
        AND li.tax_amount_cents <> 0
        AND li.amount_cents = li.total_amount_cents))
ORDER BY i.tenant_id, i.created_at;
```

**You see:** one row for each invoice to check: its account (`tenant_id`), mode, number, status and total. No rows means there is nothing to check.

The query lists every invoice of the shapes above, and some that are right:

- A draft whose tax failed when billing built it, and succeeded on a later retry, stores its lines the same way. Its total is right unless a refresh at a new rate followed.
- A draft that waits for tax since billing built it is listed with the failed refreshes.
- `s.tax_inclusive` is the account's setting today. If an account turned tax-inclusive pricing off since then, remove that condition to list its invoices too.

**Fix.** For each row, work out what the invoice's lines should charge: the plan's base fee and the period's usage at the plan's prices. With tax-inclusive pricing, that sum is the right total. If the stored total is lower, the customer was under-billed by the difference.

- A draft: void it (`POST /v1/invoices/{id}/void`) and bill the period on a [one-off invoice](../concepts/invoices.md#when-hardcount-makes-an-invoice). Or finalize it and bill the difference on a one-off invoice.
- A finalized or paid invoice: bill the difference on a one-off invoice.

A one-off invoice is taxed when you finalize it, so with tax-inclusive pricing the difference you bill includes its tax.

## What to watch

Hardcount counts every tax calculation that fails on the metric `hardcount_tax_outcome_total`, with `outcome="deferred"` and a `reason`. It counts attempts, not invoices: an invoice whose tax keeps failing adds one for each automatic retry. Successful calculations are not counted, so any increase means something needs a look: alert when its rate is above zero.

| `reason` | Meaning |
|---|---|
| `no_country` | The customer has no country on file |
| `no_client_for_mode` | Stripe is not connected for the invoice's mode, test or live |
| `credentials_unreadable` | Hardcount could not read the stored Stripe keys for that mode, for example a database error or a key it cannot decrypt |
| `api_error` | Stripe Tax returned an error |

Each one also logs a WARN line, `stripe tax failed, deferring invoice for retry`, with the reason. Alert on the metric, not the log.

| Pattern | Likely cause | What to do |
|---|---|---|
| One account, again and again | That account's Stripe Tax setup is incomplete, or its customers have no country | Contact the account owner |
| Many accounts at the same time | A Stripe outage. Check [status.stripe.com](https://status.stripe.com). | Make sure the waiting invoices clear once Stripe is back |
| One account, starting suddenly | Its Stripe API key was changed or revoked | Ask the account owner to update the keys |

Each invoice stores its provider and its Stripe Tax IDs (`tax_provider`, `tax_calculation_id`, `tax_transaction_id`), so you can match it to Stripe's tax reports without reading logs.

The provider code is in `internal/tax`; [Architecture](../architecture.md#tax-providers) says where it sits.

**Next:** [alerts on billing runs](runbook/monitoring.md).
