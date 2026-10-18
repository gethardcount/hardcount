# ADR-047: Invoice-level `tax_rate` displays the statutory rate, not the effective rate

**Date:** 2026-06-05
**Status:** Accepted

## Summary

The invoice-level `tax_rate` is the statutory rate when every taxed line has the same rate, for example 8.875% in New York City. Only when taxed lines carry different rates does it fall back to the blended effective rate (tax divided by subtotal). The effective rate is distorted by cent rounding (8.88% instead of 8.875%), so it is not shown as the rate when a real one exists. Rates print with up to four decimals on the dashboard, the hosted page and the PDFs. Status: active; multi-component taxes such as Canada's GST plus QST still show one blended rate until a design partner needs them itemized.

## Context

A $100 invoice taxed by `stripe_tax` for a New York customer (combined NYC
rate **8.875%**) stored `invoices.tax_rate = 8.8800` and showed the customer
"Sales Tax (8.88%)". The amount was correct ($8.88), but **8.88% is not a real
NY rate** — it is `tax_amount × 100 ÷ subtotal` (`888 ÷ 10000`), the *effective*
rate, distorted upward by the half-cent rounding (`$8.875 → $8.88`). The true
statutory rate `8.8750` was already stored correctly on the line items (per
ADR-046's sibling fix, PR #165) but never reached the customer-facing tax line.

This is an `stripe_tax`-only defect. The manual provider sets
`EffectiveRate = m.rate` (`internal/tax/manual.go`) — it already reports the
configured statutory rate — so manual invoices were always correct. Only
`stripe_tax` computes `EffectiveRate = totalTax × 100 ÷ subtotal`
(`internal/tax/stripe.go`), which diverges from the statutory rate whenever
rounding bites.

The displayed rate, not the amount, is the issue. Industry practice is
unanimous that the rate shown is the *real* rate and the amount is rounded
independently:

- **Stripe** — the tax `percentage` is stored to **4 decimal places** and is
  "only used for rendering purposes to be shown on the invoice, separate from
  the actual calculated tax amount." Stripe has no single invoice-level "rate";
  it aggregates tax `per tax rate` and shows the statutory rate per line/rate.
  ([Tax rates](https://docs.stripe.com/billing/taxes/tax-rates),
  [Manual tax amounts](https://docs.stripe.com/invoicing/taxes/manual-tax-amounts))
- **Zuora** — tax line items "display three decimal places when appropriate"
  for the rate, while subtotal/total "calculate to three decimal points then
  round after." ([Display tax information on invoices](https://knowledgecenter.zuora.com/Zuora_Billing/B_Set_up_Zuora_Billing/Apply_taxes/Additional_resources_on_taxes/B_Display_tax_Information_in_invoices))
- **General rule (Dynamics 365 / TaxJar)** — calculate at higher precision
  (3–4 dp) internally; display the rate at its real precision and round
  *amounts* to currency precision after calculation.
  ([Dynamics 365 tax rounding](https://learn.microsoft.com/en-us/dynamics365/finance/localizations/global/tax-calculation-rounding-rules))

No surveyed platform back-computes `amount ÷ subtotal` and shows *that* as the
rate.

A separate, related question — "should we show per-line tax on the invoice?" —
was checked against the same sources: **no**. Stripe aggregates tax into a
single invoice-level line `per tax rate`; per-line tax rates exist only in
exports/reporting, not the rendered invoice. Velox's existing shape (one
aggregate tax line + a per-jurisdiction breakdown only when lines span >1
jurisdiction) already matches this; **unchanged** by this ADR.

## Decision

Persist the **statutory** rate as the invoice-level `invoices.tax_rate` when it
is well-defined, and only fall back to the effective rate when it is not.

New helper `displayTaxRate(lines, effectiveRate)` in
`internal/billing/engine.go`, called at the single write site in
`ApplyTaxToLineItems` (`app.TaxRate = displayTaxRate(res.Lines, res.EffectiveRate)`):

- If every **taxed** line (`TaxAmountCents > 0`) shares one rate → that
  statutory rate. Untaxed/exempt lines are ignored, so a taxed + exempt mix
  still counts as single-rate.
- If taxed lines carry **different** rates (genuine multi-jurisdiction, where no
  single statutory rate exists) → the blended `effectiveRate`.

Because every invoice-level `tax_rate` consumer is propagation, persistence, or
display (verified by full caller audit — see below), correcting the one write
site fixes all surfaces with no per-surface logic.

Two formatting bugs that compounded the field bug are fixed in the same pass —
both truncated even a correct `8.8750` down to `8.88`:

- **Frontend** `tax_rate.toFixed(2)` → new `formatTaxRate()` (`web-v2/src/lib/api.ts`):
  up to 4 dp, trailing zeros trimmed. Applied to `HostedInvoice.tsx` (customer),
  `InvoiceDetail.tsx` (operator), `Settings.tsx` (config echo).
- **Go PDF** `fmt "%.4g"` → new `formatTaxRate()` / `cnFormatTaxRate()` in
  `internal/invoice/pdf.go` and `internal/creditnote/pdf.go`. `%g` uses
  *significant figures*, which silently drops precision on rates ≥ 10
  (`13.875 → "13.88"`); fixed-decimal formatting prints the rate verbatim.

## Why this design

Correcting the stored value once (rather than deriving a display rate on each of
the three surfaces) is the single-source fix: it unifies `stripe_tax` with the
manual provider, needs no new API field, and leaves no room for the surfaces to
drift apart (`feedback_no_heuristic_proxies` — plumb the authoritative fact, don't
re-derive it per consumer; `feedback_no_belt_and_suspenders` — one path).

The effective rate is kept *only* for the genuinely-blended multi-jurisdiction
case, where it is the honest summary and a per-jurisdiction breakdown carries
the real per-rate detail. The header field has to stay effective there because
no single statutory rate exists — which is also why Stripe exposes no single
invoice-level rate.

`res.EffectiveRate` is still produced by both providers and remains on the
`tax.Result`; only the engine's choice of what to *persist* changed.

## Alternatives considered

- **A. Keep `invoices.tax_rate` effective; derive the display rate per surface.**
  Three implementations of the same "uniform line rate?" logic (Go PDF, two
  TSX), guaranteed to drift, and a frontend heuristic over data the backend
  already knows authoritatively. Rejected per `feedback_no_heuristic_proxies`.

- **B. Add a new authoritative `tax_display_rate` API field.** Correct but adds a
  column/serializer/codegen surface for a value we can already compute at the
  one write site. Over-engineered (`feedback_no_overengineering`) given no
  consumer needs both the effective and statutory rate.

- **C. Store statutory always, even for multi-jurisdiction.** There is no single
  statutory rate for a multi-rate invoice — the field would have to pick one
  line's rate arbitrarily. Rejected; the effective blend is the honest summary
  for that case.

## Consequences

### Positive
- Customer- and operator-facing invoices, PDFs, and credit notes show the real
  statutory rate (NYC: **8.875%**), matching Stripe/Zuora and what an auditor
  recomputes.
- `stripe_tax` and manual now agree on what `invoices.tax_rate` means.
- The `≥ 10%` PDF precision-loss bug (`%g`) is gone for all rates.

### Risks / open items
- **Semantic change to an existing field.** `invoices.tax_rate` now means
  "statutory when single-rate, else blended effective." Documented here, in
  `MANUAL_TEST.md` FLOW B2, and the `engine.go` helper comment. No code consumes
  the old effective-always meaning (full audit confirmed).
- **No backfill.** Pre-launch; existing finalized invoices keep their stored
  rate. New finalizations get the corrected rate. Per
  `feedback_no_speculative_backfill`.
- **Multi-jurisdiction still shows a blended header %** — acceptable; the
  per-jurisdiction breakdown (already rendered when >1 jurisdiction) carries the
  real per-rate detail.

## Revisit trigger

**The first design partner who registers in a multi-component tax jurisdiction
and needs a compliant itemized invoice.** Concretely: **Canada** —
Quebec/BC/Manitoba/Saskatchewan, where federal **GST** and provincial
**QST/PST** are levied and administered separately — or **domestic India**
(intra-state **CGST + SGST**). For one customer in one geographic jurisdiction,
Stripe returns a **multi-entry** `tax_breakdown` array (e.g. Quebec: GST 5% +
QST 9.975%). Today the engine collapses that to the **blended effective rate**
(~14.975%): the amount is correct (Stripe sums the components into the per-line
`amount_tax`, copied verbatim) but the components are not itemized. US sales tax
(combined into one entry) and India OIDAR/digital (IGST, single 18%) stay
single-entry and are unaffected — this is **not** triggered by them.

The fix is **additive — a new per-line tax-components collection, not a rewrite
of the scalar model**:

1. **Storage.** Add a child `invoice_line_tax_components` table (or a JSONB
   column on the line) holding `[{tax_name, jurisdiction, rate, amount_cents}]`
   per line — mirroring Stripe's `tax_breakdown` (Stripe supports up to 10 tax
   rates per line item). Additive migration; the existing scalar
   `invoice_line_items.{tax_rate, tax_jurisdiction, tax_amount_cents}` stay as
   the blended summary, so nothing downstream breaks.
2. **`invoices.tax_rate` is unchanged.** It remains the summary scalar;
   `displayTaxRate` already returns the blended effective rate for the
   multi-component case, which is the correct header value. No semantic break to
   this ADR's decision — components are *additional* detail, not a replacement.
3. **Mapping.** In `mapResult`, map the **full** per-line `tax_breakdown` array
   into the new collection instead of reading only `tax_breakdown[0]` — this is
   the latent `[0]`-only read called out in the **Risks** above; it MUST be
   fixed in the same change or the new path silently drops the second component
   (QST/SGST). Revisit the deliberate decision **not** to expand
   `line_items.data.tax_breakdown` (we rely on the document-level breakdown
   today, see `stripe.go`); either flip the expand and harden against its
   per-calc error mode, or derive components from the document-level array.
4. **Rounding.** Each component rounds to the cent; the components must still
   reconcile to the line's `tax_amount_cents` (and the invoice total) — apply
   the largest-remainder apportionment from ADR-046 *per component* so the sum
   invariant holds.
5. **Display.** Render itemized component lines ("GST (5%)", "QST (9.975%)")
   instead of the single combined tax line, on the invoice PDF, credit-note PDF,
   hosted invoice page, and operator detail. `aggregateTaxByJurisdiction`
   (`internal/invoice/pdf.go`) generalizes to aggregate by component; the
   hosted page (which today shows no per-rate breakdown) gains one.

What does **not** change: the tax amount (already correct), the provider
abstraction, and this ADR's statutory-vs-effective rule for the scalar field.
The manual provider stays single-rate (it applies one flat tenant rate) unless a
separate decision adds multi-rate manual support.

## References

- ADR-042 / ADR-043: tax rate as `NUMERIC(7,4)` (the 4-dp precision this relies on).
- ADR-046: manual-tax largest-remainder apportionment (sibling document-vs-line
  rounding discussion); PR #165 (per-line statutory rate preserved from Stripe's
  document-level breakdown — the data this fix now surfaces).
- Memories: `feedback_no_heuristic_proxies`, `feedback_no_belt_and_suspenders`,
  `feedback_verify_stripe_parity_claims`, `feedback_no_overengineering`,
  `feedback_no_speculative_backfill`.
- [Stripe — Tax rates](https://docs.stripe.com/billing/taxes/tax-rates) ·
  [Stripe — Manual tax amounts](https://docs.stripe.com/invoicing/taxes/manual-tax-amounts) ·
  [Zuora — Display tax information on invoices](https://knowledgecenter.zuora.com/Zuora_Billing/B_Set_up_Zuora_Billing/Apply_taxes/Additional_resources_on_taxes/B_Display_tax_Information_in_invoices) ·
  [Dynamics 365 — Tax calculation rounding rules](https://learn.microsoft.com/en-us/dynamics365/finance/localizations/global/tax-calculation-rounding-rules)
