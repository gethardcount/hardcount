# ADR-099: Simulated-time disclosure — once per scope, one word, banner teaches / chip flags

**Date:** 2026-07-21
**Status:** Accepted (amended 2026-10-15: the banner is one line, see the end)
**Extends:** ADR-030 (simulated time on clock-pinned entities), ADR-086 §4 (FE relative-time anchors).
**Supersedes (partially):** the per-row Activity chip clause of ADR-030's 2026-07-18 amendment — its sim-primary / wall-subline rendering rule stands; only the redundant chip is gone (noted in place there).

## Summary

Velox marks simulated data (dates from a test clock) once per scope, with the word "Simulated" and a flask icon. A detail page for one clock-pinned customer, subscription or invoice says it once, in its page banner; in a list where rows can differ, each simulated row gets a chip. One flag per scope avoids both failures seen before: simulated dates that read as real, and repeated flags that operators learn to ignore. Amended 2026-10-15: the banner is a single neutral line with the clock time and a View clock link, and its explanation sits in a collapsed Details. Status: active, amended 2026-10-15; it partly supersedes [ADR-030](030-simulated-time-everywhere-on-clock-pinned-entities.md) by removing the per-row Activity chip.

## Context

The clock-design review (2026-07-21) closed four surfaces that showed
simulated dates with *no* indicator. The opposite failure immediately became
visible: a clock-pinned subscription's detail page disclosed simulation
**three times** — the amber page banner, a "Test clock" chip in the header,
and another chip on *every* Activity row — all saying the same thing. The
operator asked for a rule: simple, visible, jargon-free, no redundancy.

Two failure modes, one policy needed:
- **Silence**: a simulated date shown bare reads as a real date (the gap the
  review fixed).
- **Noise**: the same flag repeated N times per page teaches operators to
  ignore amber (banner blindness) — which re-creates silence.

## Decision

**Disclose once per scope, at the highest element that covers everything
below it. The word is "Simulated". The banner teaches; the chip flags.**

1. **Scope rule.** Every surface has exactly one simulation indicator, at the
   smallest level where simulated and real data can actually mix:
   - **Detail page of one clock-pinned entity** (customer, subscription,
     invoice): the amber **page banner** is the sole indicator. Nothing below
     it on the same page repeats the flag — no header chip, no per-row chips
     on child cards. Child rows still *show* their simulated timestamps and
     the "Recorded ⟨wall⟩ · by ⟨actor⟩" provenance sublines: those are
     information, not flags.
   - **Card scoped to one customer on a page with no banner** (the Credits
     ledger card): one chip on the card header.
   - **Mixed lists**, where adjacent rows can differ (invoices, credit notes,
     subscriptions, customers, usage events, dunning runs, the dashboard
     feed, a plan's subscribers, the audit log): one chip **per row** — the
     row is the scope.
2. **Vocabulary.** One visible word everywhere: **"Simulated"**, with the
   flask icon. "Test clock" is builder vocabulary; operators think "this
   data is simulated". Tooltips explain in plain language ("Dates are
   simulated (test clock) — not real time."); link variants add "click to
   open the clock". Raw clock IDs never appear in visible copy — tooltips
   and link targets may carry them for navigation.
3. **Banner = teacher, chip = flag.** The banner keeps the full explanation,
   the View-clock link, and the deleted-clock variant (amended 2026-10-15:
   the explanation now sits in the banner's collapsed Details). Chips never explain;
   one word plus a tooltip.

## Consequences

- Removed as redundant (their page banner already discloses): the
  subscription/customer detail header chips, the invoice detail header
  "Simulated" pill, and the per-event chip on the subscription Activity
  timeline.
- Retained: every mixed-list row chip, the Credits card-header chip, the
  audit log's per-row chip (wall-primary forensic list — the chip carries
  the simulated instant there).
- `TestClockBadge` / `SimulatedBadge` share the one-word vocabulary; the
  "Pinned to test clock vlx_tclk_…" tooltip phrasing is gone.
- MANUAL_TEST assertions that pinned the removed duplicates (B7's
  invoice-header badge leg) are truthed to the policy in the same PR.
- Trade-off accepted: a screenshot of *only* an invoice header no longer
  carries the badge — the banner sits directly above and is part of any
  honest capture; the PDF never had a badge and is unchanged.

## Amendment 2026-10-15: the banner becomes one line

**What changed.** The page banner is still the one place on a detail page
that says "simulated". It is now a single quiet line, not an amber box with a
paragraph:

> Simulated time: ⟨clock time⟩. Dates on this page follow the test clock. [View clock]

The explanation that used to fill the box moves into a collapsed
**Details** under that line: dates written during a clock advance carry the
simulated time, and a "Recorded" line under a row shows the real time. The
deleted-clock variant stays, shortened to one line: "Test clock deleted.
Dates on this page follow that clock's calendar, not today's date."

**Why.** Rule 3 above said "the banner keeps the full explanation". In
practice the banner sat above every other notice on the page and took 43
words to say "these dates are simulated". On an invoice with a payment
problem it pushed the notice that needed action below a box that needed
none (operator-message audit, 2026-10-14). Amber also says "act soon";
simulated time is a fact about the page, not a problem, so the line uses the
neutral tone with the flask icon.

**What stays.** The scope rule (one indicator per page), the vocabulary
("Simulated", with the flask icon), the View-clock link, and the rule that
nothing lower on the page repeats the flag. Rule 3 now reads: the banner
names the state in one line and keeps the explanation in Details; chips
never explain.
