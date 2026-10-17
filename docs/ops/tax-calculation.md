# Tax calculation

Velox adds tax to each invoice in one of three ways: no tax, one flat rate, or Stripe Tax. This page is for the person who sets up an account and the engineer who runs Velox. It says which choice fits, where to set it, and what to do when tax cannot be worked out.

Velox does not decide which taxes you owe. You register for tax with the authorities (and, for Stripe Tax, in your Stripe account), and Velox applies what you set up. That works for registrations such as:

- US sales tax,
- VAT or GST in your own country,
- OIDAR, a registration for selling digital services across borders.

## Pick a tax provider

Set it in the dashboard under **Settings → Tax → Tax calculation → Tax provider**. Each account picks one.

| Choice | What it does | Pick it when |
|---|---|---|
| **None** | Invoices have no tax. | You do not collect tax, for example early business-to-business sales where you are not registered. |
| **Manual (flat rate)** | One rate on every line of every invoice, with a name you choose, such as "VAT" or "Sales Tax". | You sell in one place with one legal rate, or you have not set up Stripe Tax. |
| **Stripe Tax** | Stripe works out the rate for each customer's location, and records each invoice in Stripe's tax reports. | You sell in several countries or US states, or you need Stripe's tax reports to include Velox's invoices. Needs Stripe connected under **Settings → Payments**. |

Velox never switches to another provider by itself. If Stripe Tax fails, the invoice waits; it is not issued at a guessed rate.

### Manual (flat rate)

1. Choose **Manual (flat rate)**.
2. Enter the **Tax name**, shown on each invoice line.
3. Enter the **Tax rate**: a percent from 0 to 100, with up to 4 decimal places, such as `8.8750`. `0` turns tax off.
4. Save.

   **You see:** the next invoice has a tax line with your name and rate.

### Stripe Tax

1. Connect Stripe under **Settings → Payments**, in each mode you use. Test mode and live mode use separate Stripe keys, so connect both.
2. In your Stripe account, add your tax registrations.
3. Choose **Stripe Tax** and save. You can also set a **Default product tax code**, which Stripe uses for a plan that does not name its own.
4. Make sure each customer has a full address, at least the country. Stripe Tax cannot work out tax without it.

   **You see:** the next invoice has a tax line that matches Stripe's calculation.

## Options that work with every provider

- **Tax-inclusive pricing** (Settings → Tax). When it is on, your prices already include tax, and Velox shows the tax part inside them. When it is off, tax is added on top.
- **A customer's tax status**, set in the customer's **Billing Profile**:
  - **Standard**: tax applies as usual.
  - **Exempt**: no tax. Velox asks for the reason.
  - **Reverse charge**: no tax on the invoice, because the business customer pays the VAT or GST themselves in their own country.
- **Tax IDs**, such as a VAT or GST number, are shown on the invoice. Velox checks only that an ID has the right shape, not that the tax authority knows it.

## When tax cannot be worked out

This happens only with Stripe Tax. The invoice waits: it is not issued, emailed or charged. The invoice page shows a notice with the reason and a **Retry tax** button.

| The notice says | What to do | Retried by itself? |
|---|---|---|
| The customer's address or tax ID is missing or invalid | Fix the customer's billing profile, then click **Retry tax** | No |
| The tax provider can't be reached right now | Check [status.stripe.com](https://status.stripe.com). Velox retries; click **Retry now** to try at once. | Yes, up to 8 times |
| The tax provider rejected the API key | Update the Stripe keys under **Settings → Payments**, then retry | No |
| Stripe isn't connected in this mode | Connect Stripe for this mode (test or live), then retry | Yes, up to 8 times |
| The tax provider can't calculate tax for this customer's region | Add a tax registration for that region in Stripe, then retry | No |
| Tax calculation failed for an unknown reason | Retry. If it keeps failing, check the server log. | Yes, up to 8 times |

After 8 automatic tries, Velox stops and the notice says so. Click **Retry tax** to try again.

## What to watch

Velox counts every invoice that waits for tax on the metric `velox_tax_outcome_total`, with `outcome="deferred"` and a `reason`. Successful calculations are not counted, so any increase means something needs a look: alert when its rate is above zero.

| `reason` | Meaning |
|---|---|
| `no_country` | The customer has no country on file |
| `no_client_for_mode` | Stripe is not connected for the invoice's mode, test or live |
| `api_error` | Stripe Tax returned an error |

Each one also writes a warning to the log:

```
WARN stripe tax failed, deferring invoice for retry reason=api_error error=… livemode=true
WARN stripe tax failed, deferring invoice for retry reason=no_country
WARN stripe tax failed, deferring invoice for retry reason=no_client_for_mode livemode=true
```

They are warnings, not errors, because nothing is lost: the invoice waits and is retried. Alert on the metric rather than on the log lines.

| Pattern | Likely cause | What to do |
|---|---|---|
| One account, again and again | That account's Stripe Tax setup is incomplete, or its customers have no country | Contact the account owner |
| Many accounts at the same time | A Stripe outage. Check [status.stripe.com](https://status.stripe.com). | Make sure the waiting invoices clear once Stripe is back |
| One account, starting suddenly | Its Stripe API key was changed or revoked | Ask the account owner to update the keys |

## Related

- [How the tax providers work inside Velox](../architecture.md#tax-providers): the provider methods, what is saved on each invoice, and why a failure waits instead of guessing.
- [Operations runbook](runbook/monitoring.md): alerts on billing runs, including tax.
