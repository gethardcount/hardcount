# fix(invoices): invoice page dates use the invoice's billing timezone, like the PDF

## What & why

The invoice detail page (`web-v2/src/pages/InvoiceDetail.tsx`) rendered the header Paid/Voided date and the Issued/Due dates with `formatDate(x)`, which uses the **live** tenant timezone. The PDF (`internal/invoice/pdf.go`, `docLoc := domain.LoadLocationOrUTC(inv.BillingTimezone)`) and the hosted page (`HostedInvoice.tsx`, `formatDate(x, invoice.billing_timezone || "UTC")`) use `invoice.billing_timezone`, the org zone stored on the invoice at issue (ADR-077).

Scenario: invoice issued while the org is `Asia/Kolkata`, `due_at = 2027-07-10T18:30:00Z`. The org later switches to `America/New_York`.

| Surface | Before | After |
|---|---|---|
| PDF / hosted page | Jul 11, 2027 | Jul 11, 2027 |
| Dashboard invoice page | **Jul 10, 2027** | Jul 11, 2027 |

Fix: `formatInvoiceDate(iso, billingTimezone)` in `web-v2/src/lib/dates.ts` renders "MMM d, yyyy" (same output shape as `formatDate`) in `billingTimezone || tenant TZ`. The argument is required (`string | null | undefined`), the same pattern as `formatCivilDate`, so a caller has to choose the zone. All four document dates on the page use it: header Voided and Paid, Issued (or created_at for a draft), Due.

Differences to know:

- Empty `billing_timezone` falls back to the tenant zone, as the page did before. The PDF falls back to UTC in that case; I kept the dashboard's existing fallback (as asked) rather than change it.
- Before `/v1/settings` loads, `formatDate` fell back to the browser zone; `formatInvoiceDate` falls back to UTC (the `dates.ts` convention). This only matters for an invoice with no `billing_timezone` during the boot window.
- Other timestamps on the page (timeline events, tax deferral, dunning next action) are not printed on the PDF and stay in the tenant zone.

### Test

`web-v2/tests/dates.test.ts`: `formatInvoiceDate uses the invoice billing timezone, not the tenant zone` (Kolkata → "Jul 11, 2027", New York → "Jul 10, 2027") and `formatInvoiceDate falls back to the tenant zone when the invoice has none`.

### Mutation check

Changed the helper to ignore `billingTimezone` (`const tz = tenantTZ()`):

```
not ok 4 - formatInvoiceDate uses the invoice billing timezone, not the tenant zone
  expected: 'Jul 11, 2027'
  actual: 'Jul 10, 2027'
```

Restored → pass. Also passes under `TZ=America/Los_Angeles`.

### Checks

- `npx tsc -b`: pass
- `npx eslint src/lib/dates.ts src/pages/InvoiceDetail.tsx tests/dates.test.ts`: 0 errors, 1 warning (existing `lineItems` useMemo warning at line 244, not touched)
- `npm test`: 235 pass, 0 fail
- `make lint-manual-test`: OK
- No Go touched.

### Screens to look at

1. Org timezone `Asia/Kolkata`. Create a customer + subscription and finalize an invoice whose `due_at` is near UTC midnight (e.g. the day-1 invoice; `due_at` 18:30Z).
2. Open `/invoices/{id}`: note Issued and Due. Download the PDF: same dates.
3. Settings → timezone `America/New_York`. Reload `/invoices/{id}`: Issued and Due are **unchanged** and still match the PDF (before the fix they moved back one day).
4. Pay the invoice, then check the "Paid · <date>" next to the badge matches the PDF's "Paid on" line. Same for a voided invoice.
5. Negative control: a one-off invoice with empty `billing_timezone` (if any legacy row exists) shows dates in the current org zone.

## Checklist

- [x] Tests cover the new behavior (unit)
- [ ] `go test ./... -race -short -count=1` passes — no Go changed; not run
- [ ] `make lint` passes — no Go changed; not run
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (FLOW TZ1, new `[ ]` box)
- [ ] `make gen` re-run — n/a
- [x] Frontend touched? `npx tsc -b`, `npm test`, `eslint` on changed files pass (`npm run build` not run separately)

## Money path only

Display only. No invoice state, writer, effect or gated reader changes. Readers of `billing_timezone` for document dates: `internal/invoice/pdf.go` (Issued, Due, Paid on, Voided), `HostedInvoice.tsx` (Due, Received, Voided), and now `InvoiceDetail.tsx` (Issued, Due, Paid, Voided).

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
