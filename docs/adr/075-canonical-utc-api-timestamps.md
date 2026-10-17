# ADR-075: The process runs in UTC so API timestamps are canonical, not host-dependent

**Date:** 2026-07-04
**Status:** Accepted

## Summary

The `velox` binary sets `time.Local = time.UTC` as the first statement of `main()`, so timestamps read back from Postgres are UTC and every API timestamp is a canonical `…Z` string on any host. Only the text form changed; billing date math never reads `time.Local` because it uses explicit timezones. Invoice and credit-note PDFs, the hosted invoice page and the dunning retry email render dates in the invoice's stored billing timezone, or UTC when none is stored. A CI lint checks that the pin is present ([ADR-076](076-enforcing-the-timezone-invariant.md)), and the test database helper sets it too. Status: active; the billing timezone is now one org setting ([ADR-077](077-org-level-billing-timezone.md)), not the per-subscription zone named below.

## Context

Velox serializes `time.Time` fields (`created_at`, `billing_period_start/end`,
`issued_at`, …) directly to JSON on the API wire. Go's `time.Time.MarshalJSON`
emits RFC3339 **in whatever timezone the value carries** — and that zone was not
constant.

The application *mints* every instant in UTC: `clock.Now()` returns
`time.Now().UTC()` by design. But timestamps that **round-trip through Postgres**
don't stay UTC. pgx decodes `timestamptz` via `time.Unix(...)`, which returns a
value in **`time.Local`** — the server *process's* zone. So on a host that isn't
UTC, a value read back from the DB serialized with the host offset:

```
# same instant, two representations, depending on where the binary runs:
IST host  → "2026-05-02T00:00:00+05:30"
UTC host  → "2026-05-01T18:30:00Z"
```

Empirically confirmed against the running dev server (an IST box): the API
returned `current_billing_period_start: 2053-06-08T00:00:00+05:30`.

Consequences of a host-dependent wire format:

1. **Non-canonical contract.** The API is *correct* (an RFC3339 offset is an
   unambiguous instant, and the frontend parses it via `new Date()`), but the
   string form varies by deployment. Production containers are UTC → `…Z`; a
   local dev box or a mis-provisioned node → `+HH:MM`. "Works in prod, differs in
   dev" is exactly the class of divergence that hides bugs.
2. **A subtle trap for consumers.** Any client that extracts a civil date by
   *slicing the string* (`iso.slice(0,10)`) rather than parsing the instant would
   read a different date depending on the server's zone — a midnight-IST boundary
   `00:00+05:30` is the same instant as `18:30Z the previous day`, so the sliced
   date shifts. The offset also invites mistaking the server zone for the
   subscription's billing zone (ADR-074), which it is not.
3. **The server's clock leaking into the representation.** The wire should
   describe the data, not the machine that served it.

Industry norm is canonical UTC (Stripe uses Unix epochs; most JSON APIs emit
`…Z`). We already had the *intent* (UTC clock); only the DB-read path dissented.

## Decision

**Pin the process to UTC.** `cmd/velox/main.go` sets `time.Local = time.UTC` as
the **first statement of `main()`**, before any DB connection or goroutine (so
the global assignment cannot race a concurrent `time.Local` read). This makes:

- pgx decode `timestamptz` as UTC (its `time.Unix` now lands in UTC), so the
  DB-read path agrees with the mint path;
- every `time.Time` serialize as canonical `…Z`, independent of the host zone;
- `time.Now()` return UTC (the clock package already did `.UTC()` explicitly, so
  this is belt-and-braces, not a change).

**Why the computation is safe.** Billing date-math never reads `time.Local` — it
anchors in explicit zones (`LoadLocationOrUTC`, the sub's `billing_timezone`,
ADR-058/074), which is the whole point of those ADRs. Display of civil-day ranges
goes through the tenant-TZ helpers, also explicit. So pinning the process zone
changes only the *representation* of instants, never any computed value. The full
unit + real-Postgres suite passes unchanged.

**Blast-radius audit and the companion fix.** An adversarial audit (frontend
date-slicing, outbound payloads/signatures, Go string formatting, test-clock)
cleared the frontend and outbound-payload surfaces but found **four
customer-facing document/email renderers formatting a DB-read timestamp with a
naive `.Format(...)`** — no `.UTC()`, no tenant-TZ helper — so they were
implicitly rendering the civil date in `time.Local` (the host zone). These are a
*pre-existing* tenant-TZ gap, not new bugs: on a UTC container they already print
UTC dates rather than the business's, and on a non-UTC host they printed the host
zone. Pinning to UTC would only swap one wrong zone for another. The correct fix
is to anchor them in the tenant/billing timezone like the period display, which
this change does **in the same PR** so the pin never regresses a customer
document:

| Site | Field(s) | Now rendered in |
| --- | --- | --- |
| `invoice/pdf.go` | Issued / Due / Paid / Voided | invoice `billing_timezone` (ADR-074), UTC fallback |
| `creditnote/pdf.go` | Issued / Invoice Date / Voided | original invoice's `billing_timezone`, UTC fallback |
| `dunning/service.go` | "we'll try again on ⟨date⟩" email | the invoice's `billing_timezone`, UTC fallback |
| `subscription/handler.go` | proration line-label boundary date | the sub's `billing_timezone` (`subLoc`) |

Legacy/ad-hoc rows with no snapshot fall back to UTC (canonical, host-independent)
rather than the old host zone — an improvement even where it isn't the tenant zone.

**Product-wide follow-up sweep.** A second, exhaustive audit (every backend module +
every frontend date surface) confirmed the four fixes above are correct and closed
four more residuals in the same class, none reached by the first pass: the
**cancel-proration** and **plan-swap-refund** credit-note descriptions in
`billing/engine.go` (`.UTC().Format` → `.In(subscriptionLocation)`); the **public
hosted-invoice page**, which as an unauthenticated route had no tenant timezone and
rendered Due/Received/Voided in the *viewer's browser zone* — fixed by adding
`billing_timezone` to the hosted payload and rendering in it (UTC fallback, matching
the PDF); and the subscription-timeline "Period Start" dot, which used the live
display TZ while its paired "Period End" used the billing TZ. The sweep found all
other emails and modules clean, and deliberately left operator-dashboard analytics
bucketing (UTC `date_trunc`) as a separate reporting-timezone product decision, not a
customer-document defect.

**Tests run under the same pin.** `testutil.SetupTestDB` sets `time.Local =
time.UTC` via a `sync.Once`, so integration tests observe production behavior on
any host (a dev box on IST would otherwise scan `timestamptz` back in `+05:30`
and diverge from CI/prod).

## Alternatives considered

- **`TZ=UTC` in the container only.** Works in prod but relies on the deploy
  environment — the "settings aspirational vs runtime enforcement" trap. Code
  enforcement is deterministic regardless of how the binary is launched. (Setting
  `TZ=UTC` in the image as well is harmless defense-in-depth, but the code is the
  contract.)
- **Normalize at the JSON boundary.** `encoding/json` has no global hook for
  nested `time.Time`; it would require a custom wrapper type on every field
  across dozens of structs — invasive and easy to miss on a new field.
- **Custom pgx `timestamptz` codec returning UTC.** Possible but fiddly with the
  `database/sql` stdlib adapter, and it wouldn't cover bare `time.Now()`. Pinning
  the process is one line, global, and covers every path.

## Consequences

- API timestamps are now canonical `…Z` on every deployment. The absolute
  instants are unchanged; only the string form is normalized — no consumer that
  parses instants (`new Date`, any RFC3339 parser) is affected.
- Logs and any bare `time.Now()` render in UTC. An improvement for correlation.
- New failure mode to remember: this is a **global mutation**. It must stay the
  first line of `main()`; a future entrypoint (a new `cmd/…` binary, a worker)
  that serves or reads DB timestamps must do the same. Enforced for the test path
  via `SetupTestDB`.

## Test locks (mutation-verified 2026-07-04)

`TestInvoice_Timestamps_SerializeAsCanonicalUTC` (real Postgres): a timestamp
round-tripped through the store scans back with `Location() == UTC` and
serializes with a `…Z` suffix (no `+HH:MM`) on `created_at`, `updated_at`,
`billing_period_start/end`. Mutation: neuter the `time.Local = time.UTC` pin in
`SetupTestDB` and run on a non-UTC host → all four fields come back `+05:30` and
the assertions fail.

`TestProrationLabels_RenderInBillingTZ`: the four proration line-label builders
render the boundary date in the passed billing-TZ location (Kolkata → "Jun 1",
UTC → "May 31" for the same instant). Mutation: drop `.In(loc)` from a label →
the UTC case keeps the raw zone and fails. The invoice/CN PDF and dunning-email
renderers apply the identical `t.In(LoadLocationOrUTC(billingTZ))` primitive; PDF
byte output isn't asserted (encoded/compressed text), so those rely on the shared
primitive plus the audit's confirmation of the site set.
