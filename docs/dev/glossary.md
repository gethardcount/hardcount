# Contributor glossary

Words from the code, the database and the operations docs. You do not need
them to use Velox.

### Database access

- **RLS (Row-Level Security)**: A PostgreSQL feature that keeps each
  tenant's data invisible to the others. Tenants share the same tables, and
  each tenant-owned row has a `tenant_id`. A policy shows only the rows that
  match the transaction's tenant, and on mode-aware tables its live or test
  mode, unless the transaction set the bypass flag.
  [More](../adr/003-postgresql-rls-multi-tenancy.md)
- **TxTenant and TxBypass**: The two ways to open a database transaction
  with `db.BeginTx`. TxTenant sets the tenant (and test or live mode), so
  RLS shows only that tenant's rows. TxBypass turns RLS off for work across
  tenants, such as the billing scheduler.
  [More](../adr/003-postgresql-rls-multi-tenancy.md#decision)
- **Table and column names behind the words above**:
  - dimensions: the `properties` column of a usage event.
  - cost table: `provider_cost_rates`.
  - saved cards: `payment_methods`.
  - dunning run: a row in `invoice_dunning_runs`.
  - an invoice's billing timezone: `invoices.billing_timezone`.
  - a test clock's time: `frozen_time`.
  - clock-pinned invoices and credit notes: `is_simulated` is set.

### Running Velox

- **outbox**: A database table of messages waiting to be sent
  (`webhook_outbox`, `email_outbox`). Most webhook events, and the four
  money emails (payment receipt, payment failed, dunning warning, dunning
  escalation), are queued inside the transaction that caused them. The rest
  are queued right after it commits. A background dispatcher then delivers
  each row, with retries. [More](../adr/040-outbox-always-on.md)
- **leader lease**: A row in the `leader_leases` table that lets exactly
  one copy of Velox run one tick of a job that must run once across all
  copies (billing, dunning, both outbox dispatchers, webhook delivery). It
  is renewed during the tick and released after it. It expires 10 seconds
  after the last renewal.
  [More](../adr/114-leader-leases-tick-scoped-fencing.md)
- **fencing token**: A number for each job in `leader_leases`
  (`holder_token`) that goes up every time a copy takes the lease. The five
  queries that claim due work check it with `leader_fence`, so a former
  leader that stalled claims nothing.
  [More](../adr/114-leader-leases-tick-scoped-fencing.md#fencing-boundary-the-five-claim-funnels)
- **velox-doctor**: A read-only command that scans a Velox database for
  states no correct code could produce, such as invoice totals that do not
  add up. It exits 1 when it finds a problem, or 2 if a check cannot run. It
  runs in CI and needs a database role that can see every row.
  [More](records/product-stability-plan.md)

### Working on the code

- **site-set (and effect-firer, gated reader)**: Every place in the code
  that touches one state, such as an invoice status. That means each
  writer and each effect-firer (code that sends an email, a webhook, a
  Stripe call or a ledger write when the state changes). It also means each
  gated reader (code that branches on the state), each caller and callee,
  and each crash point.
  [More](money-path-robustness-playbook.md#2-the-meta-practice-complete-site-set-enumeration)
