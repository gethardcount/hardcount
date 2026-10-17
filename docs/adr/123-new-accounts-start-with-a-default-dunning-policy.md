# ADR-123: New accounts start with a default dunning policy that retries three times and changes nothing

**Date:** 2026-10-17
**Status:** Accepted

## Summary

Every new account gets one dunning policy per mode, "Default retries", as
the default. It retries a failed payment on day 3, 7 and 14, sends the
usual warning and final emails when email is set up, and then leaves the
subscription and the invoice as they are. The policy is marked `seeded`.
While it is, the first enabled policy anyone creates (by hand or from a
recipe) takes over the default. Editing it or making it the default clears the
mark, and from then on it is an ordinary policy. Existing accounts get
nothing: a policy appearing on a running account would start retrying old
failed invoices.

## Context

Issue #1152. A new account had no dunning policy, so a failed payment was
not retried. `StartDunning` skipped it on purpose with a warning (ADR-036
amendment of 2026-07-05), and nothing in the dashboard said so. An operator
who never opened **Dunning policies** lost the payment silently. Bootstrap
seeded nothing on purpose: a seeded default would take the default slot,
and a recipe's policy created after it would be non-default and govern
nothing.

What other billing engines do (read 2026-10-17):

- **Stripe** retries by default. "Stripe recommends using Smart Retries…
  The recommended default setting is 8 tries within 2 weeks." A custom
  schedule allows "up to three retries, each with a specific number of days
  after the previous attempt". When retries run out, one option is "Leave
  the subscription overdue": it stays `past_due` and later invoices are
  still charged. Failed-payment emails are a setting the account turns on.
  ([Smart Retries](https://docs.stripe.com/billing/revenue-recovery/smart-retries))
- **Kill Bill** retries by default: `org.killbill.payment.retry.days`,
  "number of payment retries along with the interval in days", default
  `8,8,8`. Its documentation gives no default overdue configuration, so
  nothing is blocked or cancelled unless the account sets that up.
- **Lago** creates no dunning campaign by itself. Campaigns are created and
  assigned by hand.

Two of three retry without being asked, and none of them takes a
subscription or write-off action the account did not choose.

## Decision

- **The policy.** `dunning.DefaultSeedPolicy()`: name `Default retries`,
  enabled, 3 attempts, grace 3 days, `retry_schedule` `["96h","168h"]`
  (retries on day 3, 7 and 14), `final_subscription_action` `none`,
  `final_invoice_action` `none`. Both final actions are set explicitly,
  because an empty value normalizes to `pause`.
- **One writer.** `dunning.Service.SeedDefaultPoliciesTx` validates the
  policy with the same `normalizeAndValidatePolicy` every writer uses, then
  `PostgresStore.SeedDefaultPoliciesTx` inserts it once with
  `app.livemode='off'` and once with `'on'`, as `is_default = true,
  seeded = true`, on the caller's transaction. It writes `is_default`
  directly instead of using the first-policy rule, because bootstrap runs on
  a bypass-RLS transaction where that rule's `NOT EXISTS` would see other
  accounts' defaults.
- **Both account-creation paths call it**, in their own transaction, so a
  failed seed creates no account:
  - `tenant.RunBootstrap` (`POST /v1/bootstrap`, `cmd/velox-bootstrap`,
    the compose quick start) through `BootstrapDeps.SeedDunningTx`;
  - `tenant.Service.Create` (`POST /v1/tenants`) through
    `SetDunningSeeder`.
  Both refuse to run when the function is not wired. The tenant package
  cannot import dunning (`internal/arch/boundaries_test.go`), so the
  composition root passes the function in.
- **Audit.** One `create` / `dunning_policy` row per mode, in that mode's
  log, with `action` `bootstrap_provisioned` or `tenant_provisioned`.
- **Handing over the default.** Migration 0180 adds
  `dunning_policies.seeded` (default false). In `upsertPolicyTx`, an insert
  of an enabled policy first runs `UPDATE dunning_policies SET is_default =
  false WHERE is_default AND seeded`, then keeps the existing first-policy
  rule. A policy created turned off skips the demote and is born
  non-default: the API's `enabled` defaults to false, and handing the slot
  to a disabled policy would stop retries for every unassigned customer
  without anyone choosing that. The create dialog says before saving that
  the new policy will replace the seed as default, and the create's audit
  row carries `became_default: true`, since the demotion writes no row of
  its own. An
  update sets `seeded = false`, and so does `SetDefaultPolicy` on the
  promoted row. The demoted seed stays, so a customer assigned to it keeps
  it.
- **Existing accounts.** No backfill. The Dunning and Dunning policies
  pages show "Failed payments are not retried" when the mode has no
  default, and say so by name when the default is turned off.

## Why this design

The first enabled policy anyone creates still becomes the default, as
before; the seed only fills the gap until then. That keeps recipes working
without a special case for them, and an operator who has touched the seed
keeps their choice. Leaving the subscription and invoice alone is the one
end of retries that cannot surprise anyone. Pausing or cancelling a
customer, or writing off their debt, is a decision about a business the
seed knows nothing about.

## Alternatives considered

- **A. A recipe's policy always takes the default.** Simpler rule, but an
  account that applies a recipe months later would have its chosen default
  replaced without asking.
- **B. Leave the recipe's policy non-default when a seed exists.** The
  recipe's dunning section would then do nothing, silently, which is the
  bug the bootstrap comment warned about.
- **C. Seed `pause` (the API's default for a new policy).** Stops invoices
  stacking up for an overdue customer, but pauses collection on a business
  that never chose it. Rejected; the policy card says how to switch.
- **D. Backfill existing accounts.** Within one tick the dunning backfill
  and the card-less collector would start retries, charges and emails on
  every failed or unpaid invoice with no run, of any age. A money action on
  every running account that nobody agreed to.
- **E. Seed live mode only.** Test mode would behave differently from
  live, and a test-clock walk would show no retries without setup.

## Consequences

### Positive

- A failed payment on a new account is retried from day one, in both
  modes.
- Recipes and an operator's first enabled policy behave as before. A
  policy created turned off on a new account no longer takes the default,
  so a draft cannot switch retries off.

### Risks / open items

- **Invoices stack.** Under `none`/`none` an overdue subscription keeps
  billing, and each new unpaid invoice gets its own run and emails. This is
  the cost of not acting for the operator; the policy card says so and
  points to "Pause collection".
- **Retries and emails without opting in.** A new self-hosted account
  charges retries and emails customers from its first failed payment. The
  release notes say so.
- **The first policy on an existing account** still starts retries for
  every old failed or unpaid invoice without a run, as it always has. The
  dashboard does not yet count those invoices before the policy is created;
  that warning needs its own count endpoint and is left for a separate
  change.
- **Every route that takes ownership must clear `seeded`.** Today those are
  PATCH (which also covers disabling) and set-default. A new route that
  edits a policy must clear it too, or a later create would take over a
  policy the operator customised.

## References

- ADR-036 (campaigns model; amendment of 2026-07-05 superseded in part by
  this ADR), ADR-112 (two final actions), ADR-116 (the card-less collector)
- Migration 0180 (`dunning_policies.seeded`)
- Issue #1152
