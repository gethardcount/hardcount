# ADR-076: The timezone-display invariant is enforced, not just documented

**Date:** 2026-07-04
**Status:** Accepted

## Summary

Every calendar date shown to a person, such as an invoice date or a billing period, must be rendered in a named timezone. Code review alone let about eight wrong-day bugs through, so automatic checks enforce the rule. In Go, the `internal/arch` test `TestCivilDayFormat_UsesExplicitZone` fails CI on a date-only `.Format` call with no `.In(loc)`; a genuine UTC or filename case carries a `//tz:ok` comment. In the dashboard, `formatCivilDate` and `formatCivilPeriod` require a timezone argument, so TypeScript rejects a call that leaves it out. Status: active; on 2026-10-16 the `make lint-tz` script described below became this Go test, and a second test checks the UTC pin in `cmd/velox/main.go`.

## Context

ADR-058/074/075 established a correct timezone *model*:

- **Instants** (storage, the API wire) are UTC.
- **Billing math** anchors in the subscription's frozen `billing_timezone` (ADR-074).
- **Human-facing CIVIL-DAY dates** ("Issued June 1", "period Jun 1 – Jun 30",
  "we'll retry on Jun 2") render in the owning entity's **billing/tenant
  timezone** — never the process/host zone, and never a stray `.UTC()` on a
  customer calendar date.

The model is right. The way it was *maintained* was not: **correct by developer
discipline.** A Go `time.Time` carries no signal about which zone it should be
displayed in, so every `.Format(...)` is a decision point where the author must
remember the instant-vs-civil-day distinction and hand-pass the right zone. The
frontend has the same gap: `formatCivilDate(iso)` silently defaulted to the live
tenant/display zone when a caller forgot the argument.

The evidence that discipline doesn't hold: the ADR-074/075 arc, across two
exhaustive adversarial audits, found and fixed **~8 render-site bugs of exactly
this shape** — bare `.Format` (host zone) and `.UTC().Format` (wrong civil day) on
customer credit notes, invoice/CN PDFs, a dunning email, proration labels, the
public hosted page, and a subscription-timeline dot. Each was a fresh instance of
the same class. Audits are snapshots; nothing prevented the *next* one. The
invariant needed to move from convention into a mechanism that fails loudly and
automatically.

## Decision

Enforce the invariant in the two places that fail automatically — **CI and the
type system** — matching how this repo already guards invariants (grep-based
`make lint-clock` / `lint-funding-set` / `lint-manual-test`, ADR-030/048/038).

1. **`make lint-tz` (CI gate, `scripts/lint-tz.sh`).** A date-only `.Format(...)`
   (layout carries a date token, no clock `:`) must render through an explicit
   `.In(loc)`. Bare `.Format` and `.UTC().Format` on a calendar date both fail —
   the latter is the exact cancel/plan-swap credit-note bug ADR-075's follow-up
   found. **A genuine UTC/instant/filename case carries a same-line `//tz:ok`
   waiver** (currently one site: the CSV download-filename stamp). The gate also
   asserts the ADR-075 process-UTC pin (`time.Local = time.UTC`) is present in
   `cmd/velox/main.go` — the one piece of the arc that was previously guarded only
   by code review. Wired into the CI lint job (no extra runner).

   A green `lint-tz` is a **machine-checked proof** that every civil-day render in
   Go carries an explicit zone — strictly stronger than an audit, and permanent.

2. **Frontend: the civil-day helpers require their zone.** `formatCivilDate(iso,
   tz)` and `formatCivilPeriod(start, end, tz)` made `timezone` a **required**
   parameter (typed `string | undefined`, so a legacy row's optional
   `billing_timezone` can still be passed and falls back to the tenant TZ — the
   type forbids *omitting* the argument, not passing undefined). TypeScript now
   flags at compile time any civil-day render that forgot to name its zone — the
   FE analogue of `lint-tz`, and the exact class of the "Period Start dot" and
   hosted-page bugs. Instant helpers (`formatDate`/`formatDateTime`) keep their
   optional tenant-TZ default, which is correct for instants.

Both gates are **mutation-verified**: reintroducing a bare `.Format`, removing the
UTC pin, or omitting a FE zone argument each fails its gate.

## Alternatives considered

- **A `civil.Date` type** (make illegal states unrepresentable — Java `LocalDate`,
  JS `Temporal.PlainDate`): the canonical fix in languages where a calendar date
  and an instant are *distinct types*, so `date.format()` simply has no zone to
  omit. **It cannot be complete in Go, and therefore cannot replace the lint.** Go
  fuses instant and civil-time into one `time.Time`, and `time.Time.Format` is a
  method on *every* `time.Time` value. A `civil.Date` type would make the *right*
  path nicer and give compiler enforcement at leaf helpers that accept it — but any
  function that still holds a `time.Time` (every document/engine site does, for the
  math) can always call the bare `.Format` the type was meant to prevent. Only a
  linter can forbid that method. So `civil.Date` would be a **second, partial
  mechanism that does not remove the lint** — exactly the belt-and-suspenders our
  "one path" rule rejects when the second path adds churn without closing a gap the
  first leaves open. Considered and **not adopted**: in Go the lint *is* the
  complete-as-the-language-allows enforcement, so it is the single backend path.
  (This is standard Go practice — linters guard type-level footguns `time.Time`,
  `context.Context`, and `error` all expose, e.g. `contextcheck`, `errcheck`.)
- **A full `go/analysis` analyzer** instead of grep: more precise, but heavier to
  build/maintain and inconsistent with the repo's existing grep-lint convention.
  The grep gate's precision is adequate (it matches only `.Format("…date…")`, not
  bare numbers/comments) and the `//tz:ok` escape hatch handles the long tail.
- **Backend-authoring every date string** (extend `billing_period_display` to
  every field): centralizes the zone decision, but bakes presentation into the API
  wire — at odds with the ADR-075 "wire is canonical UTC data, not presentation"
  principle — and proliferates DTO fields. Kept for server-rendered surfaces
  (PDF/email) where it already fits; not adopted for the API/FE.

## Consequences

- The ~8-bug class is closed structurally: a ninth instance fails CI (Go) or
  `tsc` (frontend) before merge. The reactive audits are now a permanent gate.
- New date-rendering code must make the zone explicit — a small, self-documenting
  tax that encodes the instant-vs-civil-day distinction at the point of use.
- The `//tz:ok` waiver and the `undefined`-allowed FE type keep genuine
  UTC/instant/legacy cases expressible without weakening the default.
- Not a silver bullet: `lint-tz` guards `.Format("…date…")` on `time.Time`; a value
  already in the wrong zone before it reaches `.Format`, or a civil date built some
  other way, is out of scope. In Go there is no type that closes that residual gap
  (see the `civil.Date` note above) — it is a code-review concern, not a mechanized
  one, and the escape hatch is the same `//tz:ok`.
