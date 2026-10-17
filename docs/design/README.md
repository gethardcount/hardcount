# Design docs

A design doc is the plan for a feature, written before it was built. Each
one here has shipped. The code changed after some of them, so each page
opens with a status line that names the current rules.

Want to know how a feature works today? Read the user docs in
[docs/README.md](../README.md) first. Come here for the reasoning behind
a feature.

| Design | Status | What it covers |
|---|---|---|
| [Billing thresholds](billing-thresholds.md) | Shipped. Current rules: ADR-065, ADR-066 | Billing a subscription early when its spend or usage passes a limit you set. |
| [CC on billing emails](cc-emails.md) | Shipped. Current rules: ADR-082 | Sending invoice, receipt and payment-retry emails to more than one address. |
| [Create-preview endpoint](create-preview.md) | Shipped. No ADR; the code is the record | Showing a customer's next invoice before it is made. |
| [Customer usage endpoint](customer-usage.md) | Shipped. No ADR; the code is the record | One call that returns what a customer used and what it costs. |
| [Multi-dimensional meters](multi-dim-meters.md) | Shipped. Current rules: ADR-044, ADR-045, ADR-070 | One meter that prices each model and token type at its own rate. |
| [Pricing recipes](recipes.md) | Shipped. Current rules: ADR-085 | Ready-made pricing set up in one call. |
| [Provider cost tables](cost-tables.md) | Shipped. Current rules: ADR-079 | Recording what each AI provider charges you, to show your margin. |
| [Prepaid commits](prepaid-commits.md) | Shipped. Current rules: ADR-078 | Credit a customer pays for up front, then uses up. |
| [Paid-commit credit-note relief](commit-cn-relief.md) | Shipped. Current rules: ADR-080 | Refunding the unused part of a prepaid commit. |

The decision records (ADRs) are in [docs/adr/](../adr/README.md).
