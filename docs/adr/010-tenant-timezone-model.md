# ADR-010: Tenant Timezone Model

**Date:** 2026-05-01
**Status:** Accepted — but the "billing math stays in UTC" sub-decision below is **superseded by ADR-058** (billing date-math anchors in the tenant timezone) and **ADR-077** (the billing timezone is an org-level setting, denormalized onto each invoice at issue). The display-timezone parts of this ADR still hold.

## Status
Accepted

## Date
2026-05-01

## Summary

Instants are stored as `timestamptz` and sent on the API as UTC RFC 3339 strings ([ADR-075](075-canonical-utc-api-timestamps.md) pins the process to UTC). The dashboard shows dates and times in the org timezone (`tenant_settings.timezone`, an IANA name, default `UTC`) and reads dates the operator picks in that zone. There is no per-user timezone. The sub-decision that billing math stays in UTC is superseded: billing date math now anchors in the same org timezone ([ADR-058](058-billing-date-math-tenant-timezone.md), [ADR-077](077-org-level-billing-timezone.md)). Status: active for display, superseded for billing.

## Context

Until this commit, every date input on the Velox dashboard interpreted picks
in the operator's *browser* timezone, every display rendered in browser local,
and no rendered timestamp carried a timezone label. This created two real
problems:

1. **Operator-pick semantics shift across operators.** An operator in
   `Asia/Kolkata` picking "May 5" as an API key expiry got `May 5 23:59:59
   IST` (= `May 5 18:29:59 UTC`). An operator in `America/Los_Angeles`
   picking "May 5" got `May 5 23:59:59 PDT` (= `May 6 06:59:59 UTC`).
   Same picked date, different absolute expiry — the *business* didn't have
   a single answer to "when does this key expire?".

2. **Picker behavior wasn't transparent.** The "Key will expire on May 5,
   2026" hint hid the time and timezone. Operators had no surface
   indication that their browser TZ was the silent input.

A 2026-05-01 deep research pass across Stripe, GitHub, Auth0, Salesforce,
HubSpot, Linear, Recurly, Chargebee, and Zuora identified two industry
camps:

- **UTC-everywhere camp** (Stripe, GitHub, Auth0): wire/storage/billing math
  all UTC; account/tenant timezone is a *display + report-windowing*
  preference only. Stripe explicitly does NOT shift `billing_cycle_anchor`
  or `current_period_end` based on `account.timezone`.
- **Site-TZ camp** (Recurly, Chargebee, Linear): tenant TZ governs display
  AND billing-day interpretation, locked at schedule creation time.
- **Two-level camp** (Salesforce, HubSpot): both org TZ and per-user TZ;
  most date inputs respect both. Right shape for CRM-scale tools, overkill
  for a billing engine.

Velox is a developer-facing API + operator dashboard, audience-aligned with
Stripe and GitHub. The right philosophical anchor is the UTC-everywhere
camp, with tenant TZ as a *display + civil-date-input* preference.

The infrastructure for tenant TZ already existed:
`tenant_settings.timezone` shipped in migration 0001,
`domain.TenantSettings.Timezone` is plumbed through the service and API,
and the Settings page exposes a TZ Combobox. What was missing was *using
it* — the dashboard had a tenant TZ field with no semantic effect.

## Decision

### Storage — UTC everywhere

All instants stored as Postgres `timestamptz` (already the case). No naive
`timestamp` columns for instants. Date-only fields like a future
`coupon.valid_until_date` would use `DATE` (no timezone), Salesforce's
clean date-vs-datetime split.

### Wire format — UTC ISO 8601

All `*_at` fields on the wire are RFC 3339 with `Z` suffix. No naive local
strings. Matches Stripe and GitHub conventions; sidesteps Zuora's
public-cautionary-tale of returning naive local strings whose meaning
silently shifted when the tenant changed TZ.

### Tenant timezone — single setting, IANA name, default UTC

`tenant_settings.timezone` already exists, default `'UTC'`, validated as a
real IANA zone name on write. Used for:

- **Date-grade input interpretation.** When an operator picks "May 5" in
  a calendar widget — for API key expiry, coupon valid-until, future
  cancel-at, etc. — the dashboard interprets that picked civil date as
  end-of-day in tenant timezone, then converts to UTC for the wire.
- **Display.** Every rendered timestamp on the dashboard renders in tenant
  timezone with an explicit zone label (`"May 5, 2026 at 11:59 PM PDT"`).
  No bare dates. The label closes the round-trip-uncertainty loop —
  operators see exactly what timezone the value is in.
- **Future**: report windows, scheduled-job interpretation, dashboard
  "today" rendering.

### Subscription billing math — UTC, follow Stripe

> **SUPERSEDED by ADR-058 / ADR-077 (2026-06/07).** Billing month/year
> math now anchors in the **billing timezone** (org-level per ADR-077,
> denormalized onto the invoice at issue), not UTC — `addIntervalIn`
> resolves "monthly on the 5th" in that zone (`internal/domain/
> subscription.go`). The UTC framing below is retained as the original
> decision record only.

`current_billing_period_start/end`, `billing_cycle_anchor` stay UTC.
"Monthly on the 5th" means *5th in UTC*, not 5th in tenant TZ. This
matches Stripe's model and avoids the DST + retroactive-shift pitfalls
that Chargebee documents. Velox's billing engine is already TZ-agnostic
absolute and remains so.

### Per-user timezone — none in v1

Velox has 1 operator and 0 design partners. A per-user TZ override is
pure speculation and would require a `users.timezone` column, a settings
page, and rendering plumbing. Defer until a multi-operator design partner
asks for it. The schema is non-breakingly extensible — adding
`users.timezone NULL` later defaults to tenant TZ for existing users.

### Frontend implementation

The migration is now end-to-end across the dashboard. Four-commit
arc (b523c71 → 2e93b3f):

**Module-scoped tenant TZ + display defaults** (`b523c71`)
- `date-fns-tz` added as a dependency (canonical companion to `date-fns`).
- `lib/api.ts` exposes `setTenantTimezone(tz)` / `getTenantTimezone()`.
  `formatDate(iso, timezone?)` and `formatDateTime(iso, timezone?)` default
  to the module-scoped TZ when set, fall back to browser-local otherwise.
- `formatDateTime` adds zone abbreviation (`"May 5, 2026, 2:14 PM PDT"`)
  when rendering in tenant TZ; `formatDate` stays bare (date-only is
  unambiguous at day resolution).
- `lib/TenantTimezoneBootstrap.tsx`: render-nothing component mounted in
  `main.tsx` inside `AuthProvider`. Fetches `/v1/settings` once user is
  authenticated and seeds the module-scoped TZ. React-query caches the
  fetch so per-page `useQuery(['settings'])` callers dedupe.
- Net: ~70 existing display call sites across 20+ files automatically
  inherit tenant-TZ rendering. Zero per-callsite churn.

**Civil-date pickers** (`198d670`)
- Shared helpers in `lib/dates.ts`: `endOfDayInTZ`, `startOfDayInTZ`,
  `addDaysInTZ`, `formatYMDInTZ`, `formatExpiryHintInTZ`. All default to
  `getTenantTimezone()` when no explicit TZ arg is passed.
- Migrated: ApiKeys (refactored to use shared helpers), Coupons,
  CouponDetail, Credits. All operator-picked civil dates (expiry-grade)
  now compute UTC via tenant TZ.

**Date-range filters** (`0a1675f`)
- Migrated: Invoices (client-side via `formatYMDInTZ`), UsageEvents
  (server-side via `startOfDayInTZ` / `endOfDayInTZ`), AuditLog
  (server-side via the same helpers).
- Backend: `audit/audit.go` `normalizeDateFilter` accepts either an
  RFC3339 instant (the dashboard's new format) or a bare yyyy-mm-dd
  (legacy fallback for direct curl users). Backward compatible.

**Inline `toLocaleString` cleanup** (`2e93b3f`)
- Five chart-axis / grouping callsites in CostDashboard, Dashboard, and
  AuditLog were inline `new Date().toLocaleDateString(...)` calls — they
  bypassed `formatDate` so didn't inherit the bootstrap-time tenant TZ.
- All five now read `getTenantTimezone()` and use `formatInTimeZone` with
  matching format strings, falling back to browser-local pre-bootstrap.

### Out of scope (deferred per feedback_pre_launch_scoping)

- **Three-field civil round-trip** (`expires_at_civil DATE` +
  `expires_at_tz TEXT` alongside `expires_at TIMESTAMPTZ`). Stripe-grade
  nice-to-have; defer until multi-operator-with-different-TZs DP appears.
- **Per-user TZ override.** Defer until seats > 1.
- **Tenant-TZ-anchored billing cycles.** Stripe's model (UTC-anchored
  cycles) stays. Opt-in later if a DP requires.
- **Tenant-local scheduled jobs.** Dunning, retry stay UTC cron.


### Migration of remaining surfaces (future, not part of this commit)

The dashboard has many date pickers and timestamp displays. Migrating each
to use tenant TZ is a per-feature decision:
- **Coupon `valid_until`** — civil-date semantics, should use tenant TZ.
  Audit when next we touch the coupon UI.
- **Subscription `start_date`, `cancel_at`** — civil-date semantics for
  operator picks; same pattern.
- **Invoice `due_at`** — already a UTC instant from server; display should
  use tenant TZ.
- **System timestamps** (`created_at`, `last_used_at`, audit log entries)
  — render in tenant TZ with UTC tooltip for debugging. Mirrors GitHub's
  `<time datetime="...Z">` pattern.

The pattern is the same everywhere: pickers compute UTC via tenant TZ;
displays format UTC via tenant TZ. Scaling this beyond ApiKeys is purely
edit-and-test work, no architectural changes.

## Consequences

### Positive

- Operator-picked civil dates are stable across operators (within a single
  tenant TZ).
- Picker UX is transparent: operators see what timezone they're picking in,
  with the abbreviation labelled.
- Backend remains UTC-pure; no billing math changes.
- The schema and infrastructure for per-user TZ (when needed) are
  non-breakingly extensible.
- Aligns Velox with Stripe's model — the closest conceptual peer for an
  API-first billing engine.

### Negative

- Re-render of a stored timestamp under a different tenant TZ shows a
  *different* civil date to viewers (e.g. tenant flips from PDT to IST,
  May 5 11:59 PDT becomes May 6 12:29 PM IST). Mitigated by labelling the
  TZ on every render. The Chargebee-style snapshot pattern would solve
  this fully but adds schema/wire complexity not justified for 1 operator.
- Operators in physical TZs different from tenant TZ pick "May 5" and get
  end-of-May-5 in *tenant* TZ (not their local) — they may briefly think
  the key expires earlier/later than expected. Mitigated by the hint
  labelling both the date and the TZ.
- Adds `date-fns-tz` (~6KB gzipped) to the dashboard bundle. Negligible.

### Open follow-ups

- Migrate the rest of the dashboard's date pickers and timestamp displays
  to use tenant TZ. Roughly 8-12 surfaces; do per-feature when each is
  next touched.
- Add per-user `users.timezone` when a multi-operator DP requests it.
- Add three-field civil round-trip (`expires_at_civil` + `_tz`) when a
  multi-tenant-TZ scenario surfaces.
- Add `Reports` API timezone parameter (Stripe-style) when the reports
  surface exists.
