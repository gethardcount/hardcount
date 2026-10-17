# Roadmap

What changed lately, and what is being worked on next. Every change, with its date and reasoning, is in the [CHANGELOG](../CHANGELOG.md).

## Shipped recently

October 2026 (v0.6.1 and v0.7.0):

- **Try it with Docker only.** `deploy/compose/quickstart.sh` starts Velox from the published images and prints a login, and `./quickstart.sh demo` runs AI token usage through to an invoice.
- **Late usage is billed.** An invoice is built an hour after its period ends (`INVOICE_GRACE_PERIOD`), so usage that arrives a little late still counts.
- **Issued invoices do not change.** An invoice or credit note keeps the customer and company details it was issued with.
- **Billing that keeps failing is visible.** The dashboard marks subscriptions whose billing is failing, says why, and has a Retry button.
- **Live credit balance.** `GET /v1/credits/balance/{customer_id}/live` and the public cost page show how much prepaid credit a customer has left, including usage not yet invoiced.
- **LiteLLM works with a real proxy.** Usage from a LiteLLM proxy is recorded per model and token type, with the provider's own cost when the proxy sends it.
- **Safer payments.** A void, write-off or bank payment waits for a card charge in progress, and a Stripe rate limit no longer stops an invoice.
- **Login works in Safari when Velox runs on localhost.**
- **Security fixes.** Endpoints refuse ids from another account, webhooks check the address they connect to, and `/metrics` needs a token in production.
- **Release images for ARM and Intel/AMD**, signed with cosign.
- **API reference for every route you call** ([`api/openapi.yaml`](../api/openapi.yaml)).

July to September 2026:

- Prepaid commits, drawn down by later invoices.
- Provider cost tables and per-customer margin.
- Team invites.
- Safer handling of card charges whose outcome is unclear, and of unpaid invoices.
- An automated check that runs 30 money consistency rules in CI.
- Running several copies of Velox at once, with background jobs handed over in seconds when one stops.

## Next

- **Safer upgrades.** A repair command for a database upgrade that stopped halfway ([#967](https://github.com/getvelox/velox/issues/967)), and checks that a database upgrade cannot lock tables for long ([#987](https://github.com/getvelox/velox/issues/987)).
- **LiteLLM usage for a customer that does not exist yet** is skipped today. Make it visible, and decide whether to keep it ([#1044](https://github.com/getvelox/velox/issues/1044)).

## Not planned for now

These wait until someone using Velox needs them and can say what shape they need:

- A Helm chart and a Terraform module. Docker Compose is the supported install.
- A tool to move from Stripe Billing.
- Compliance documents (SOC 2, data deletion, audit-log retention).
- Roles and permissions. Every team member has full access today.
- Bulk actions, billing alerts, moving many subscriptions to a new plan at once, and a separate docs site.
