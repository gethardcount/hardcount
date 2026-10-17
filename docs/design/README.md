# Design docs

A design doc is the plan for a feature, written before it was built. Each
one here has shipped. Each page opens with three lines: what was decided,
the release and pull request (PR) that shipped it, and the doc that
describes how it works now. The code changed after some of them, so read
that current doc first.

Come here for the reasoning behind a feature. The user docs start at
[docs/README.md](../README.md).

| Design | What was decided | Shipped | Current docs |
|---|---|---|---|
| [Billing thresholds](billing-thresholds.md) | Invoice a subscription early when its spend or usage reaches a cap you set. | v0.1.0, [#29](https://github.com/getvelox/velox/pull/29) | [Feature list](../features.md#subscriptions-and-invoices); ADR-065, ADR-066 |
| [CC on billing emails](cc-emails.md) | Copy up to 10 more addresses on invoice, receipt, payment and credit-note emails. | v0.2.0, [#393](https://github.com/getvelox/velox/pull/393) | No guide yet: [ADR-082](../adr/082-email-recipient-semantics.md), [API reference](../../api/openapi.yaml) |
| [Create-preview endpoint](create-preview.md) | Show a customer's next invoice before it is made. | v0.1.0, [#28](https://github.com/getvelox/velox/pull/28) | [Feature list](../features.md#subscriptions-and-invoices); no ADR, the code is the record |
| [Customer usage endpoint](customer-usage.md) | One call returns what a customer used and what it costs. | v0.1.0, [#26](https://github.com/getvelox/velox/pull/26) | [The Velox API](../api-surface.md#usage); no ADR, the code is the record |
| [Multi-dimensional meters](multi-dim-meters.md) | One meter prices each model and token type at its own rate. | v0.1.0, [#20](https://github.com/getvelox/velox/pull/20) | [AI pricing](../ai-pricing.md#how-velox-picks-the-price-for-an-event); ADR-044, ADR-045, ADR-070 |
| [Pricing recipes](recipes.md) | Ready-made pricing set up in one call. | v0.1.0, [#23](https://github.com/getvelox/velox/pull/23) | [AI pricing](../ai-pricing.md#ready-made-recipes); ADR-085 |
| [Provider cost tables](cost-tables.md) | Record what each AI provider charges you, to show your margin. | v0.1.0, [#373](https://github.com/getvelox/velox/pull/373) | [AI pricing](../ai-pricing.md#see-your-margin); ADR-079 |
| [Prepaid commits](prepaid-commits.md) | Credit a customer pays for up front, then uses up. | v0.1.0, [#371](https://github.com/getvelox/velox/pull/371) | [AI pricing](../ai-pricing.md#sell-prepaid-credit-a-prepaid-commit); ADR-078 |
| [Paid-commit credit-note relief](commit-cn-relief.md) | Refund the unused part of a paid prepaid commit, at the purchase rate. | v0.2.0, [#391](https://github.com/getvelox/velox/pull/391) | No guide yet: [ADR-080](../adr/080-paid-commit-cn-relief.md), [API reference](../../api/openapi.yaml) |

The decision records (ADRs) are in [docs/adr/](../adr/README.md). Failure
modes found while benchmarking are in the
[failure-mode catalog](../failure-modes/README.md).
