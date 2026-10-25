# ADR-036: Dunning campaigns model (multi-policy-per-tenant)

**Date:** 2026-05-16
**Status:** Accepted (amended 2026-05-16 — terminal-action set + pause-semantics fix; amended 2026-07-05 — initial-default-at-first-create + no-policy = deliberate skip; amended 2026-10-17 — new accounts start with a seeded default, [ADR-123](123-new-accounts-start-with-a-default-dunning-policy.md); amended 2026-10-22 — a collection pause does not stop open runs; amended 2026-10-22 — a dunning pause lifts when its invoice is paid; amended 2026-10-23 — the schedule is one list of days, `retry_days`; **the terminal-action half is SUPERSEDED by [ADR-112](112-dunning-exhaustion-settles-two-questions.md), 2026-08-05**; see Amendment sections below)

> ⚠️ **The 2026-05-16 amendment below describes a `final_action` column that no
> longer exists.** [ADR-112](112-dunning-exhaustion-settles-two-questions.md)
> split it into `final_subscription_action` (`none`/`pause`/`cancel`) and
> `final_invoice_action` (`none`/`mark_uncollectible`), because three of the
> four values below describe the SUBSCRIPTION and one describes the INVOICE —
> so choosing one meant giving up the others, and "cancel and write off" was
> inexpressible. The per-action *semantics* in that table are still accurate;
> only their mutual exclusivity is gone. Everything else in this ADR — the
> campaigns model, per-customer assignment, the retry schedule — stands.

## Summary

A tenant can keep many named dunning policies (retry rules for unpaid invoices), with at most one per tenant and `livemode` marked `is_default`. A customer uses the policy set in `customers.dunning_policy_id`, or the tenant default when it is empty, and a run keeps the policy it started with. This follows the named-template model of Lago, Orb and Recurly and replaces the per-customer partial overrides. `UpsertPolicy` rejects a policy whose `max_retry_attempts` is greater than `len(retry_schedule) + 1`, so no retry reuses an interval silently. An amendment of 2026-05-16 added the terminal actions that fire when retries run out (pause collection, mark uncollectible, cancel subscription, manual review). ADR-112 has since split them into separate subscription and invoice choices. Amended 2026-07-05: the first policy per tenant and `livemode` becomes the default. When a customer has no effective policy, dunning is skipped on purpose with a warning instead of failing billing, while a real database error still fails. Amended 2026-10-17 (ADR-123): every new account starts with a seeded "Default retries" policy per mode as the default, and the first enabled policy anyone creates takes the default from it unless the operator has edited it or chosen it.

## Context

The pre-2026-05-16 dunning data model was a tenant-wide singleton:

- `dunning_policies` enforced one row per `(tenant_id, livemode)` via a
  UNIQUE constraint. The "policy" was a tenant-global retry config:
  `(max_retry_attempts, grace_period_days, retry_schedule[],
  final_action)`.
- `customer_dunning_overrides` allowed a per-customer **partial-field
  override** — `max_retry_attempts`, `grace_period_days`, and
  `final_action` could be overridden but `retry_schedule` could not.
  At runtime the engine merged the override fields into the tenant
  policy at `StartDunning` time.

Two structural problems surfaced during the 2026-05 dogfood pass:

1. **Silent retry-schedule fallback** — when an override set
   `max_retry_attempts=5` against a tenant policy with a 2-entry
   `retry_schedule`, the engine's `processRun` used `idx >=
   len(retryIntervals) { idx = len(retryIntervals) - 1 }` to **reuse
   the last interval** for the extra attempts. Operators saw three
   back-to-back 120h retries they hadn't configured. This is a
   `feedback_no_silent_fallbacks` violation — the engine paper-overed
   a misconfiguration instead of surfacing it at save time.

2. **Partial-field override has no industry precedent.** Multi-platform
   verification on 2026-05-16:

   | Platform | Data model | Per-customer mechanism |
   |---|---|---|
   | **Stripe** | Account-level Smart Retries policy (count + window) | Account-level only; "Automations" for customer-segment policies. No per-individual-customer field override. |
   | **Lago** | Named **Campaigns** (`name, threshold, delay, attempts`). | Assign a different campaign to the customer — whole template swap. |
   | **Orb** | **Dunning Schedules** (templates with step offsets) + **Dunning Rules** (eval order, customer/plan matching) | Customer differentiation via rule matching / exclusion. |
   | **Recurly** | Named **Dunning Campaigns**. | Assign a campaign per Plan or Account. |

   All four converge on **named templates** + **assign-to-customer** (or
   rule-match). Velox's partial-field override is a one-off shape with
   no precedent.

   **Correction (2026-10-23):** Lago's campaigns run automatically only with
   a premium licence and the `auto_dunning` flag. Checked in lago-api
   `2e52dfc` (2026-10-22): `app/services/dunning_campaigns/bulk_process_service.rb:8`,
   `app/models/organization.rb:152`.

Sources:
- [Stripe — Smart Retries](https://docs.stripe.com/billing/revenue-recovery/smart-retries)
- [Lago — Automatic dunning](https://doc.getlago.com/guide/dunning/automatic-dunning)
- [Orb — Advanced dunning](https://docs.withorb.com/invoicing/advanced-dunning)
- [Recurly — Dunning campaigns](https://docs.recurly.com/recurly-subscriptions/docs/dunning-management)

## Decision

Adopt the converging industry shape: **multi-policy-per-tenant + per-
customer assignment**. Per `feedback_stripe_parity_framing`, where
Stripe is structurally limited (account-level only), adopt the
superset architecture used by Lago / Orb / Recurly (named templates +
assignment).

### Data model

1. **`dunning_policies` becomes multi-per-tenant.** Drop the
   `tenant_id, livemode` UNIQUE constraint. Add `is_default boolean
   NOT NULL DEFAULT false` with a partial UNIQUE index enforcing
   exactly one default per `(tenant_id, livemode)`.

2. **`customers.dunning_policy_id`** — nullable FK to
   `dunning_policies.id`. NULL = use the tenant's default policy.
   Updatable via `PATCH /v1/customers/{id}` (`dunning_policy_id`
   field; empty string = clear).

3. **Drop `customer_dunning_overrides`.** Per-customer differentiation
   now flows through the FK assignment.

### Service-layer resolution

`dunning.Service.GetEffectivePolicyForCustomer(ctx, tenantID,
customerID)` resolves:
1. If `customer.dunning_policy_id` is set → load that policy.
2. Else → tenant's `is_default=true` policy.

Called at `StartDunning` time to bind the policy to a new run.

Once a run exists, `processRun` resolves the policy via `run.policy_id`
(persisted at start) so **mid-flight runs stay on their original
policy** even if the customer's assignment changes. The new
assignment takes effect on the NEXT run.

### Save-time validation (closes the silent-fallback gap)

*(2026-10-23: replaced. The schedule is one list, `retry_days`, whose length
is the retry count, so there is no count to check it against. See
[the amendment](#amendment-2026-10-23--the-schedule-is-one-list-of-days).)*

`UpsertPolicy` now rejects when `max_retry_attempts > len(retry_schedule) + 1`.
The runtime `idx >= len(retryIntervals)` reuse-last-interval branch is
removed; if the engine ever encounters an out-of-bounds index it
fails loudly (a schema-invariant violation, not a runtime fallback).

### Handler surface

- `GET /v1/dunning/policies` — list policies with `assigned_customers` counts.
- `POST /v1/dunning/policies` — create.
- `GET /v1/dunning/policies/{id}` — read one.
- `PATCH /v1/dunning/policies/{id}` — update (cannot flip `is_default`).
- `DELETE /v1/dunning/policies/{id}` — delete; refused for the default policy or any policy with assigned customers.
- `POST /v1/dunning/policies/{id}/set-default` — atomic flip.
- `PATCH /v1/customers/{id}` — `dunning_policy_id` field (empty = clear).

Dropped: `GET/PUT /v1/dunning/policy` (singleton),
`GET/PUT/DELETE /v1/dunning/customers/{id}/override`.

### UI

- New `/dunning-policies` admin page — list, create, edit, set-default,
  delete. Schedule shortage warning rendered inline when the form's
  `max_retry_attempts > retry_schedule.length + 1`.
- Customer detail page — "Dunning policy" card shows the effective
  policy (assigned or default) + "Change" button → `AssignDunningPolicyDialog`
  picking from the policy list.
- `/dunning` page no longer hosts the policy editor (Tabs → single
  Runs view; "Manage policies" link to the admin page).

## Why this design

- **Industry convergence.** All four reference platforms verified
  during 2026-05-16 research use the same named-templates pattern.
  Velox follows that shape rather than inventing a Velox-specific
  one.

- **No silent fallback.** The runtime "reuse last interval" branch is
  gone. Misconfiguration is caught at save time with a clear error
  message naming the missing entries.

- **Migration friendliness.** Existing tenants' singleton policies
  flip to `is_default=true` automatically (migration 0086 backfill).
  No customer data was lost because the pre-launch state has zero
  production customers (`project_state_2026_04`); even if it had,
  the old override semantics are subsumable: any
  `(max_retry_attempts, grace_period_days, final_action)` override
  can be expressed as a new named policy with those values plus the
  tenant's `retry_schedule`, then assigned to the customer.

- **Mid-flight stability.** Runs persist their `policy_id` at start
  time and re-resolve via that id (not via the customer's current
  assignment) for the run's lifetime. An operator reassigning a
  customer doesn't yank the rules out from under in-progress retries.

## Alternatives considered

1. **Extend `CustomerDunningOverride` with `retry_schedule []string`.**
   Pure Velox extension; closes the partial-override gap but ships a
   shape no industry platform uses. Rejected — `feedback_stripe_parity_framing`
   says match the converging industry pattern.

2. **Stripe-style account-level-only.** Drop per-customer
   differentiation entirely. Simplest but doesn't fit operators who
   want differentiated dunning for enterprise vs SMB. Rejected — the
   "we want different retry budgets for high-value customers" need is
   a real B2B SaaS workflow.

3. **Orb-style rules engine.** Customer matching by `plan_ids`,
   `payment_method_types`, `exclusion_lists` with rule priority
   ordering. More flexible than direct assignment but adds a rules
   evaluation layer Velox doesn't need today. Deferred — when a
   design partner asks for "assign by plan tier" we can layer
   rule-based assignment on top of the campaigns model without
   schema churn.

## Consequences

### Positive
- Operator workflow matches Stripe / Lago / Recurly mental model.
- Save-time validation prevents the back-to-back-retry surprise.
- Per-customer flexibility without partial-override confusion.
- Path to Orb-style rule-based assignment open if needed.

### Risks / open items
- **Schema migration is destructive** for `customer_dunning_overrides`
  rows. Velox is pre-launch so no real data lost; down-migration
  re-creates the table empty.
- **In-flight run persistence** assumes `dunning_policies` rows are
  never hard-deleted while a run references them. Application-layer
  `DeletePolicy` enforces this (refuses delete if `assigned_customers >
  0`); the schema-level FK `customers.dunning_policy_id ON DELETE
  SET NULL` is belt-and-suspenders for the customer side. There's no
  FK on `invoice_dunning_runs.policy_id` — if a policy is force-
  deleted via raw SQL while runs reference it, `processRun` errors
  cleanly at lookup time. Acceptable for an operator-controlled
  surface.

## Amendment 2026-05-16 — terminal-action set + pause-semantics fix

Follow-up after operator question "is dunning's pause subscription
option correct industry standard?" — triggered a second multi-platform
research pass on dunning **terminal actions** (the action that fires
after retries exhaust).

### Verified cross-platform terminal actions (2026-05-16)

| Action | Stripe | Lago | Orb | Recurly |
|---|---|---|---|---|
| Cancel subscription | ✓ (default) | ✓ (terminate) | ✗ docs list only retry_payment + email | ✓ (expire) |
| Pause subscription | ✓ (via `pause_collection.behavior`) | ✗ | ✗ | ✗ |
| Mark uncollectible | ✓ ("Mark Invoice as Uncollectible") | — | — | ✓ (analog: "mark invoice failed") |
| Leave open / manual review | ✓ ("Keep active") | ✓ (default) | — | ✓ ("leave overdue") |

Sources:
- [Stripe — Manage failed payments](https://docs.stripe.com/billing/revenue-recovery/smart-retries) — *"Cancel the Subscription (default setting), Mark Invoice as Uncollectible …, or Pause the Subscription until payment is resolved."*
- [Lago — automatic-termination feature](https://getlago.canny.io/feature-requests/p/trigger-automatic-subscription-termination-based-on-dunning-campaigns)
- [Orb — advanced dunning](https://docs.withorb.com/invoicing/advanced-dunning) (no state-change actions documented)
- [Recurly — dunning management](https://docs.recurly.com/recurly-subscriptions/docs/dunning-management)

**Correction (2026-10-23):** Lago's "Cancel subscription ✓ (terminate)" cell is wrong; it should read ✗ (requested, not built). The source above is a feature request. A Lago dunning campaign has thresholds, `bcc_emails`, `days_between_attempts` and `max_attempts`, and no final action. Automatic dunning also needs a premium licence. Checked in lago-api `2e52dfc` (2026-10-22): `app/models/dunning_campaign.rb:59-69`, `app/services/dunning_campaigns/process_attempt_service.rb:17-33`, `app/services/dunning_campaigns/bulk_process_service.rb:8`.

### Decision

Velox supports all four actions; semantics aligned with Stripe.

*(Superseded by ADR-112 — these are no longer four alternatives on one column
but two independent choices. The behaviours described are unchanged.)*

| Velox enum (2026-05-16 amendment; column split by ADR-112) | Behavior |
|---|---|
| `manual_review` | Run lands state=escalated. No sub/invoice mutation. Operator handles via dashboard. Maps to Stripe "Keep active." |
| `pause` | **Calls `subscription.Service.PauseCollection(behavior=keep_as_draft)`** — not the pre-amendment hard `PauseAtomic`. Cycle keeps drafting invoices; invoices drafted during the pause are not charged and start no dunning, and they stay drafts after collection resumes (only later cycles finalize). Collection resumes when the operator resumes it or, for a pause dunning set, when the invoice whose dunning set it is paid (amendment 2026-10-22, "a dunning pause lifts when its invoice is paid"). Runs already open on earlier invoices keep retrying (amendment 2026-10-22, "a collection pause does not stop open runs"). Matches Stripe's `pause_collection.behavior=keep_as_draft`. The hard-pause path silently skipped invoice generation for the affected periods (per ADR-035 analysis) — non-Stripe and destructive. |
| `mark_uncollectible` | Calls `invoice.Service.MarkUncollectible` — flips the unpaid invoice to status='uncollectible' (new status, migration 0089). Stripe-standard semantics: receivable closed but invoice stays in financial reporting. Distinct from voided. |
| `cancel_subscription` | Calls `subscription.Service.Cancel`. Stripe-default terminal action; supported by 3 of 4 reference platforms. Previously missing — operator workflow required a manual cancel step after escalation. |

Old `write_off_later` enum value mapped to `mark_uncollectible` during
the migration (semantics-identical, industry-standard spelling).

### Implementation slices

1. **Migration 0088** — drop CHECK on `dunning_policies.final_action`,
   re-add accepting the four new values, backfill `write_off_later → mark_uncollectible`.
2. **Migration 0089** — add `uncollectible` to the `invoices_status_check` CHECK constraint.
3. Domain — rename `DunningActionWriteOff → DunningActionMarkUncollectible`,
   add `DunningActionCancelSubscription`. Add `InvoiceUncollectible` to invoice status enum.
4. Service interfaces — `SubscriptionPauser.Pause → PauseCollection`,
   new `SubscriptionCanceler.Cancel`, new `InvoiceUncollectibleMarker.MarkUncollectible`.
5. `dunning.Service.exhaustRun` switch covers all four cases.
6. `invoice.Service.MarkUncollectible` — flips status with the same
   transition guards as `Void` (paid/voided/already-uncollectible refused).
7. Router adapters wire `SetSubscriptionPauser`, `SetSubscriptionCanceler`,
   `SetInvoiceUncollectibleMarker`.
8. SPA `DunningPolicies.tsx` dropdown shows the four options with
   Stripe-aligned labels (pause label explicitly says "keep drafting").
9. Invoice activity timeline's `describeDunningEvent` recognizes the
   three state-change reasons.

### Why each call

- **`pause` → PauseCollection**: matches Stripe semantics. The hard-pause
  path silently skipped cycle billing for the affected periods, leaving
  no record of customer activity during the paused window — destructive.
  PauseCollection(keep_as_draft) keeps the cycle running so the operator
  has an audit trail.

- **Add `cancel_subscription`**: 3 of 4 platforms support it; Stripe
  defaults to it. Velox required a manual operator step post-escalation
  before this amendment — non-standard.

- **Rename `write_off_later → mark_uncollectible`**: same intent, but
  `mark_uncollectible` is the Stripe-standard term. `write_off_later`
  is a Velox-only spelling with no industry analog.

- **Add `InvoiceStatusUncollectible`**: enables true Stripe-parity for
  the mark-uncollectible action. Pre-amendment the status didn't exist
  and the option couldn't be wired correctly (would have had to use Void
  as an approximation, which is destructive in different ways).

## References

- ADR-029: fully disjoint test-clock flows
- ADR-030: simulated time everywhere on clock-pinned entities
- ADR-035: per-fact simulated-time anchoring
- Migration 0086: `dunning_policies_campaigns_model` (initial decision)
- Migration 0088: `dunning_policies_final_action_enum` (amendment — 4-action set)
- Migration 0089: `invoices_status_uncollectible` (amendment — new invoice status)
- Memory: `feedback_stripe_parity_framing`, `feedback_verify_stripe_parity_claims`,
  `feedback_no_silent_fallbacks`, `feedback_reference_platforms`,
  `feedback_enum_check_constraint_audit`

## Amendment (2026-07-05) — initial default + no-policy is a deliberate skip

The multi-policy model above created a **no-default trap**: `upsertPolicyTx`
inserted every new policy with `is_default=false` and only `SetDefaultPolicy`
could designate one, so the normal state after a recipe instantiation (or a
tenant's first manual policy) was *"a policy exists, but `is_default=false`"* →
`GetDefaultPolicy` → `ErrNotFound` → `GetEffectivePolicyForCustomer` errored →
the no-payment enrollment sweep reported `had_errors` and poisoned the billing
catchup for an unpaid invoice. Surfaced live 2026-07-05 (a recipe tenant's
`"AI default retry"` policy was `is_default=false`).

Two orthogonal fixes (validated by a divergent design panel):

1. **Initial-default-at-first-create.** The FIRST policy per `(tenant, livemode)`
   is born `is_default=true`, computed in-statement as
   `NOT EXISTS(SELECT 1 FROM dunning_policies WHERE is_default)` (RLS-scoped to
   the current tenant+livemode). Both `UpsertPolicy` (manual) and `UpsertPolicyTx`
   (recipe) inherit it. Subsequent policies stay `is_default=false`; the operator
   re-points via `SetDefaultPolicy` (still the sole atomic demote-all/promote-one
   path). `is_default` continues to mean exactly what it says — the row is
   genuinely flipped, so `GetDefaultPolicy`/UI/analytics all agree (rejected
   *resolve-to-sole*, a read-time patch that leaves `is_default` lying in the DB
   and introduces a 1→2-policy silent-stop cliff). The partial unique index
   `idx_dunning_policies_one_default_per_tenant` is the loud backstop for a
   concurrent-first-insert race — no savepoint-retry (creates are serial; add it
   under a named concurrency need). This generalizes migration 0086's own
   one-time backfill (`UPDATE … SET is_default=true`) to every future tenant.

2. **No effective policy = a deliberate skip, not an error.** `StartDunning` maps
   *only* `ErrNotFound` from `GetEffectivePolicyForCustomer` to
   `errs.InvalidState("dunning not configured")` (with a WARN) — the same
   deliberate-skip class as a *disabled* policy, which the enrollment adapter
   already swallows. So a genuinely zero-policy tenant (a plain bootstrap — which
   deliberately seeds **no** dunning policy, to avoid shadowing recipe policies)
   never poisons the money-path catchup: an unconfigured optional feature must not
   break core billing (the SMTP-unset precedent). A real infra error still
   propagates (fail loud — no silent fallback). Companion: `startDunningWithRetry`
   short-circuits `ErrInvalidState` so disabled/no-policy tenants don't burn the
   retry budget or emit the misleading "operator must start manually" ERROR.

Resolution precedence (per-customer `dunning_policy_id` → tenant `is_default`)
is unchanged. Memory: `feedback_adversarial_panel_before_build`,
`feedback_settings_aspirational_runtime_enforcement`, `feedback_no_silent_fallbacks`,
`feedback_no_belt_and_suspenders`, `feedback_longterm_fixes`.

## Amendment 2026-10-17 — new accounts start with a default

Point 2 of the 2026-07-05 amendment said a plain bootstrap deliberately
seeds no dunning policy, because a seeded default would take the default
slot and leave a recipe's policy non-default. That left every new account
not retrying failed payments until someone created a policy (#1152).

Now every new account, from `tenant.RunBootstrap` or `POST /v1/tenants`,
starts with one "Default retries" policy per mode as the default, marked
`seeded`. The first-policy rule of point 1 still decides the default: an
insert of an enabled policy first demotes a default that is still
`seeded`, so the first enabled policy anyone creates (by hand or from a
recipe) becomes the default as before. A policy created disabled leaves the
seed as default.
Editing the seeded policy or making it the default clears `seeded`, and it
then keeps the default. The no-policy skip of point 2 is unchanged and now
applies only to accounts created before this change. Details, values and
the rejected alternatives are in
[ADR-123](123-new-accounts-start-with-a-default-dunning-policy.md).

## Amendment (2026-10-22) — a collection pause does not stop open runs

The `pause` row above said "charging + dunning paused until operator
resumes". That was true only for invoices created during the pause. A
collection pause keeps each NEW invoice a draft, so it is never charged and
never starts a run. It does not touch a run already open on an invoice that
was finalized before the pause: `processRun` has no pause check, so the run
keeps its schedule and each retry charges the customer's default card. Only
the charge sweeps (the queued auto-charge, and the charge when a card is
added) skip a paused subscription.

This behavior stays. Stripe does the same: "Invoices created before you pause
subscriptions continue to be retried unless you void them"
([Pause payment collection](https://docs.stripe.com/billing/subscriptions/pause-payment),
read 2026-10-22). The copy was wrong, not the code. The no-card notice said
"adding a card won't charge this invoice" and the queued notice said "won't be
charged automatically" while the next retry would charge (product bug-class
sweep 2026-10-21, A2).

Where it shows:

- Both paused-collection invoice notices read the invoice's run. With a run
  that is still retrying, they say the retries continue, and their Details say
  how to stop them: void the invoice or mark it uncollectible. With no open
  run, they keep saying nothing charges the invoice until collection resumes.
- The `PauseCollection` docstring and the pause-collection API description
  say the same.
- The dunning escalation email after a `pause` final action says new invoices
  for the subscription will not be charged automatically, not that collection
  on the account stopped. The dunning policy outcome and the write-off
  dialog's "Also pause collection" hint say new invoices stay drafts and
  retries already running on other unpaid invoices continue.
- The pause dialog on the subscription page says the same: retries already
  running on unpaid invoices continue, and voiding the invoice or marking it
  uncollectible stops them.
- "Retries continue" is said for a run that is active, not paused, and not
  waiting on a failed final action. A run whose final action failed stays
  active with every attempt used, and its next tick re-attempts only the
  action. One case still reads as retrying when no retry is due: an open run
  whose attempt count is at or above a `max_retry_attempts` lowered after the
  run started. Its next tick runs only the final action
  (`processRun` exhausts on entry). The refused-card notice reads the same
  flag and has the same gap.

When it happens: an operator pauses collection on a subscription that has an
unpaid invoice in dunning. When dunning's own `pause` final action fires, the
run on the invoice that exhausted is closed, but runs open on the
subscription's other unpaid invoices keep retrying.

There is no control that pauses one run. The `paused` column on
`invoice_dunning_runs` is read by `processRun`, but nothing sets it.

## Amendment 2026-10-22 — a dunning pause lifts when its invoice is paid

The `pause` final action used to hold "until the operator resumes". A
customer who then paid the overdue invoice (by card, offline, or through a
credit note that left nothing due) stayed paused. Their new invoices stayed
drafts and nothing charged them, with no sign of why. Found as A3 in the
2026-10-21 product sweep. The rule below is Sagar's decision of 2026-10-21:
paying the invoice whose dunning paused collection resumes it.

**Decision.**

1. A dunning pause records the invoice whose dunning set it:
   `subscriptions.pause_collection_invoice_id` (migrations 193 and 194).
   NULL is an operator's pause, set through the API or the dashboard. The
   API shows it as `pause_collection.set_by`: `dunning` or `operator`.
   Pauses set before the migration have no invoice, so they stay the
   operator's: they were set under the old rule. A CHECK refuses an invoice
   on a row with no pause, so a writer that clears the pause and forgets the
   column fails.
2. Paying that invoice resumes collection, even if another invoice on the
   subscription is still owed. Paying any other invoice changes nothing.
   That other invoice has its own dunning run; if its dunning exhausts while
   collection is still paused, the pause stays the first invoice's (point
   5). The trade is deliberate. A wider rule ("resume once nothing is
   owed") can leave a subscription paused after its overdue invoice is
   paid, which is the A3 harm itself. It also needs a re-check on every
   void and write-off. Under this rule, paying the overdue invoice always
   resumes collection.
3. The release rides the paid transaction. Every paid flip goes through the
   invoice store's `markPaidLockedTx` (ADR-068's choke point), so card,
   offline, credit-note and credits-cover payments release it the same
   way. Each transaction that can reach that flip first locks the
   subscription row when its dunning pause waits for this invoice
   (`LockDunningPauseTx`), before the invoice row. Every writer of both rows
   locks the subscription first, so a cancel or plan swap that holds the
   subscription and then relieves the invoice cannot deadlock with the
   payment.
4. The release does not clear the pause. It sets `resumes_at` to the time
   Velox records the payment, and the scheduled resumer does the rest, as
   for an operator's timed pause:
   - a period that ended under the pause still closes as a draft;
   - the resumer's compare-and-clear announces the resume once;
   - the resumer writes the "Collection resumed" row (detail "The overdue
     invoice was paid") and sends `subscription.collection_resumed`, both
     with `triggered_by=payment`.

   Every reader already treats a `resumes_at` in the past as resumed.
5. Dunning never overrides a pause in force. The pause write locks the
   subscription, then the unpaid invoice, and pauses only while that invoice
   is still owed, so a payment that commits first wins and the run closes as
   recovered. It leaves an operator's pause, with its `resumes_at`, as it
   is, and a dunning pause keeps the invoice it was set for. A second
   exhaustion on a subscription dunning already paused writes no second
   "Collection paused" row. This closes ADR-112's deferred "pause has no
   skip-if-done" item.
6. An operator pause never lifts on a payment. An operator who pauses a
   subscription dunning paused takes the pause over (`set_by=operator`).

**Kept as today.** Drafts from paused periods stay drafts; only periods
that end after the payment finalize and charge (Stripe: after a resume you
turn automatic collection back on for each draft). A void or a write-off of
the invoice does not lift the pause; only its payment does. A canceled
subscription keeps its pause.

**Not built.** The resumer's clear does not check that `resumes_at` is
unchanged since it listed the row. It waits for a reported need.

**Built later.** A dunning pause sends `subscription.collection_paused`
with `triggered_by: dunning`, queued in the pause's transaction (#1384,
2026-10-25); the operator's pause carries `triggered_by: operator`. The
operator's resume sends nothing when nothing was paused (#1377).

## Amendment 2026-10-23 — the schedule is one list of days

**Decision.** A policy's schedule is one ordered list, `retry_days`: the
whole days to wait before each retry. The first wait counts from the failed
payment, or, with no card on file (ADR-060), from the collection that found
none. Each later one counts from the retry before it, so `[3, 4, 7]` retries
on day 3, 7 and 14. The number of retries is the list's length. It replaces
three fields that had to agree: `grace_period_days` (the wait before retry
1), `retry_schedule` (Go durations for retries 2 to N) and
`max_retry_attempts` (N). The save-time check above existed only because
they could disagree, and the retry path still carried an out-of-bounds
error for the case. Simplification audit item B8, decided under the owner's
delegation.

**Why one list.** Kill Bill's `org.killbill.payment.retry.days` is the same
shape: a list of days, its length the retry count, the first entry counted
from the failed payment (`PaymentConfig.java:31-34`,
`InvoicePaymentControlPluginApi.java:591-609`, killbill `3548b02`).
Stripe's custom retry schedule is up to three retries "each with a specific
number of days after the previous attempt"
([Automate payment retries](https://docs.stripe.com/billing/revenue-recovery/smart-retries#custom-retry-schedule),
read 2026-10-23). Lago has one `days_between_attempts` and a separate
`max_attempts` (lago-api `2e52dfc`, `app/models/dunning_campaign.rb:23-24, 62-65`,
`app/services/dunning_campaigns/process_customer_service.rb:99-105`).

**The grace period is the first entry.** It was only ever the wait before
the first retry: `StartDunning` was its one reader. ADR-112 and ADR-113
give it no other meaning, and the invoice grace of ADR-120 (late usage
before a period closes) is a different thing that shared the word.

**Days, not hours.** Every peer above counts in days. The seed, the recipes
and the dashboard's defaults all wrote whole days; no Velox writer asks for
a shorter wait. A day is 24 hours, as `grace_period_days` was. Each
wait is 1 to 30 days and a policy has 1 to 15 retries. Before, only the
first wait had an upper bound (30 days); a later wait over 30 days is now
refused.

**Kept as it was.** When a run starts (ADR-064), the card-less enrollment
(ADR-060), exhaustion and its two questions (ADR-112), the final actions,
the run's `paused` flag and test clocks. A run still reads its bound
policy every tick, so shortening `retry_days` below a run's attempts
exhausts it on its next tick, as lowering `max_retry_attempts` did.
`GET /v1/dunning/runs` keeps a run's `max_retry_attempts`, now the length
of the policy's list.

**Stored policies.** Migration 00206 converts each one exactly: the first
wait, then the waits its retries used, in days. Entries past the last
retry were never used and are dropped. A schedule left out (one retry
needed none) was stored as JSON null and read as an empty list, so it
converts as one. A policy whose retries used a wait
that is not whole days from 1 to 30 stops the migration, named in the
error: rounding would retry on a day nobody chose, and under the clean
break from 0.8.x only local databases can hold one. The API refuses the
three old fields by name with a 422.

