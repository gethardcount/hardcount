# Roadmap

What changed lately, and what waits. Every change, with its date and reasoning, is in the [CHANGELOG](../CHANGELOG.md).

## Now: v0.9.0

Not yet released. Migrations run with goose, and `velox migrate force` is gone. There is no upgrade from v0.8.x: install fresh. Everything else it changes: [Unreleased](../CHANGELOG.md#unreleased).

## Shipped recently

October 2026 (v0.6.1 to v0.8.2):

- **Try it with one script.** `deploy/compose/quickstart.sh` starts Velox from the published images and prints a login, and `./quickstart.sh demo` runs AI token usage through to an invoice.
- **Late usage is billed.** An invoice is built an hour after its period ends (`INVOICE_GRACE_PERIOD`), so usage that arrives a little late still counts.
- **New accounts retry failed payments.** Each new account starts with a "Default retries" policy.
- **Live credit balance.** `GET /v1/credits/balance/{customer_id}/live` and the public cost page show how much prepaid credit a customer has left, including usage not yet invoiced.
- **LiteLLM usage for a customer that does not exist yet is kept.** Velox holds it and shows it on the Usage page. Create the customer, then release it once.
- **Issued invoices do not change.** An invoice or credit note keeps the customer and company details it was issued with.
- **Release images for ARM and Intel/AMD**, signed with cosign.

July to September 2026:

- Prepaid commits, drawn down by later invoices.
- Provider cost tables and per-customer margin.
- Team invites.
- Safer handling of card charges whose outcome is unclear, and of unpaid invoices.
- An automated check that runs 30 money consistency rules in CI.
- Running several copies of Velox at once, with background jobs handed over in seconds when one stops.

## Next

Open work is in the [issue tracker](https://github.com/getvelox/velox/issues).

## Not planned for now

These wait until someone using Velox needs them and can say what shape they need:

- A Helm chart and a Terraform module. Docker Compose is the supported install.
- A tool to move from Stripe Billing.
- Compliance documents (SOC 2, data deletion, audit-log retention).
- Roles and permissions. Every team member has full access today.
- Bulk actions, billing alerts, moving many subscriptions to a new plan at once, and a separate docs site.

**Next:** try it on your computer with the [quick start](quick-start.md).
