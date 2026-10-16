I've audited all six pages in Invoices & Collections, plus the invoice PDF. The biggest problems are on what customers see. Draft PDFs look like real issued invoices. The PDF's seller name defaults to "Velox". The hosted Pay button is shown even though Stripe isn't configured, and pressing it gives the customer a raw server error. On top of that, the counts and status labels don't match from page to page.

I didn't submit anything destructive and didn't create any records. I opened every dialog listed in the brief and cancelled it.

Paths below use two short forms:
- `S/` = `/tmp/claude-0/ux/slices/` (the existing crawl slices)
- `W/` = `/tmp/claude-0/ux/work-invoices/` (my interaction shots, the PDFs and their PNG renders, and the scripts `shoot.cjs` and `shoot2.cjs`)

Code paths are relative to `/home/user/velox/web-v2/src/` unless they start with `internal/`.

## A) Verdict per page

**Invoices list.** You can find invoices by number and status, but the page describes one invoice state in four different words. Tab "Open", status badge "finalized", subtitle "Showing finalized", and the detail page's "Open" pill all mean the same thing. Search only matches invoice numbers: "Globex" returns nothing. The date filter doesn't say which date it filters on. Export ignores the filters. Each row carries up to four amber signals (left border, dot, "pending" badge, "Simulated" badge), and draft rows get the amber border too. On mobile the status tabs are cut off.

**Invoice detail (VLX-000001 to 000007).** It's strong on safety. There is one primary action per state, Void and Write-off need a typed confirmation, and the explanation notices are good. The layout is a long single column, though: test-clock line, notice, a full document "paper" card, a collapsed Payment details card, then Activity. Amount due, customer and next step are never summarised at the top. The document repeats the number and ID, and shows placeholder seller text ("VELOX" / "Your Company") instead of a prompt to fix settings. On drafts you can only add lines, not edit or remove them, and the menu offers "Void invoice" instead of "Delete draft".

**Hosted invoice (customer-facing).** The layout is clean and close to Stripe's hosted invoice, with Amount due shown big at the top right. But the Pay button appears even though no Stripe keys are configured. Pressing it shows the customer the raw message "failed to create checkout session", under a "Secured by Stripe" line. The From block is an empty heading, the page title is just "Invoice", and on mobile the Pay button is squashed to about 20px tall.

**Invoice PDF.** Readable and plain, but not yet a document you could send:
- The seller is "Velox" (the fallback in `internal/invoice/pdf.go:337`), with no seller address or tax ID.
- Drafts carry no DRAFT watermark; only VOID has one (`pdf.go:324`).
- The totals block doesn't line up with the Amount column.
- The memo is loose, unlabelled text.
- There is no pay link.

Next to Stripe's PDF it lacks a seller block, "Pay online" link, "Amount due by \<date\>" headline, page numbers and payment instructions.

**Credit Notes.** I only saw the empty state, because the seed has no credit notes. Creating one from this page uses a native `<select>` for the invoice, with no search and no amounts or customers shown. The amount label uses the tenant's currency instead of the chosen invoice's. The same action from the invoice page uses a different dialog that behaves differently.

**Dunning.** The KPI cards and resolve dialog work. But "Recovered" actually counts resolved runs, which include voids and write-offs, so the label overstates recovery. The sidebar badge says 2 while the page says Active 3. Rows only expand from an unlabelled chevron. The "Started" column mixes "yesterday" with "Aug 13, 2026, 12:00 AM UTC". On mobile the four KPI cards fill the whole first screen.

**Dunning policies.** The list card is fine. The edit form contradicts the saved data: the seeded policy has 4 retries and 4 schedule entries, while the form says "Need 3 entries for 4 retries". The form also uses jargon ("Go durations"), and server errors come back in snake_case ("Max_retry_attempts … retry_schedule"). The schedule is shown in hours ("336h") where days would be clearer.

## B) Findings

| id | page | sev | cat | evidence | recommendation | size |
|---|---|---|---|---|---|---|
| INV-1 | PDF | broken | function | `W/pdf-vlx_inv_db8vsm664plc2fltr4h0-1.png` (VLX-000007 is a draft but reads "Issued October 16"); `internal/invoice/pdf.go:324` has only a VOID watermark | Add a DRAFT watermark and change "Issued" to "Draft — not issued", as Stripe does on draft previews | S |
| INV-2 | PDF / detail / hosted | broken | function, consistency | Seller shows 4 ways: PDF "Velox" (`pdf.go:337`), detail "VELOX" and "Your Company" (`InvoiceDetail.tsx:1064,1101`), hosted empty "FROM" (`HostedInvoice.tsx:466-478`); API `supplier_snapshot:{}` | Never print a made-up seller. Block or warn at Finalize when the company name is unset ("Set your business details → Settings"), and on the detail page show an inline "Missing seller details" chip in place of the placeholder | M |
| INV-3 | Hosted | broken | function, honesty | `W/hosted-after-pay.png` shows "failed to create checkout session" under "Secured by Stripe"; `HostedInvoice.tsx:199,631` | Have the server set `pay_enabled=false` when Stripe isn't configured. Map errors to customer-safe copy ("Online payment isn't available — contact \<seller\>") | S |
| INV-4 | Hosted (mobile) | broken | mobile | `W/m-hosted-full.png`, Pay button collapsed to about 20px | The button has `flex-1` inside `flex-col` (`HostedInvoice.tsx:637`), which collapses its height. Use `sm:flex-1` | S |
| INV-5 | Invoices list (mobile) | broken | mobile | `W/m-invoices.png`: tabs past "Open" cut off, search shrinks to an icon, "To date" overflows, Amount column off-screen (`main` scrollWidth 563 at 390px) | Stack the header. Make tabs a horizontally scrollable strip or a select, put the filter row in a sheet, and switch to card rows below `sm` showing number, customer, amount and status | M |
| INV-6 | Sidebar vs list / dunning | confusing | consistency | Sidebar shows Invoices 2 and Dunning 2, while the Open tab shows 4 invoices and Dunning shows Active 3 (`S/invoices__desktop-light__p1.png`, `S/dunning__desktop-light__p1.png`). Cause: `analytics/overview.go:160-168` excludes simulated invoices, and the list pages don't | Use one predicate for both. Either include simulated items in test mode, or label the badge "2 live" | S |
| INV-7 | List / detail | confusing | consistency | "Open" tab vs "finalized" badge vs "Showing finalized" subtitle (`Invoices.tsx:229,277,439`) vs detail "Open" (`InvoiceDetail.tsx:1081`); raw "pending" payment status | Write one `invoiceStatusLabel()` and use it everywhere. Merge status and payment into a single badge like Stripe's (Draft / Open / Past due / Paid / Void / Uncollectible) | S |
| INV-8 | Invoices list | confusing | visual | Draft rows (000007/4/3) get an amber left border because the code colours by `payment_status` (`Invoices.tsx:401`); 000002 (past due) has no visible border (rendering cause not checked) | Drop the row border. One status badge plus a red "Past due" chip is enough (Stripe and Linear rows have no colour bars) | S |
| INV-9 | Invoices list | confusing | function | `W/list-search-customer.png` shows "No invoices match 'Globex'"; placeholder "Search by invoice number…" (`Invoices.tsx:312`); there is no customer filter except via deep link | Server search across number, customer name and email. Add a Customer filter chip next to the date filter (Stripe's filter-pill bar) | M |
| INV-10 | Invoices list | confusing | function | Date pickers say only "From date" and "To date"; Oct 1–31 includes 000004, whose period ends Oct 12 (`W/list-daterange-oct.png`) | Label the field ("Issued", "Due" or "Created") with a selector, like Stripe's "Date ▾ is between" pill | S |
| INV-11 | Invoices list | friction | function | Export CSV always exports the full dataset (`Invoices.tsx:212-218`) | Pass the current filters, or label it "Export all" with a "matching filters" option | S |
| INV-12 | Invoices list | friction | interaction | Empty tab (Paid/Voided) hides the search, date and Export row, and offers "Clear filters" for a tab (`W/list-tab-paid.png`; `Invoices.tsx:263,305,356`) | Keep the toolbar in place. Empty tab copy: "No paid invoices yet" with no clear button | S |
| INV-13 | Invoices list | friction | hierarchy | No columns for issued or due date (due only shows as a badge); Customer column can't be sorted; PDF button on every row, drafts included | Columns: Amount · Status · Number · Customer · Due · Created. Put PDF and other actions in a row `⋯` menu, plus a checkbox column for bulk send / finalize / export (Stripe invoice list) | M |
| INV-14 | Detail (draft) | confusing | function | Draft menu has Email / Preview / Download / **Void** (`W/detail7-menu.png`); line rows have no edit or remove control (0 buttons); "Issued Oct 16" and Due show on an unissued draft (`S/invoice-VLX-000007-draft__desktop-light__p1.png`) | Draft gets "Delete draft", inline edit and remove on line rows, and an "Issue date: on finalize" placeholder, like Stripe's draft editor. Hide Email on drafts | M |
| INV-15 | Detail | confusing | function | Memo ("Implementation services", "Draft add-on") appears on the PDF and hosted page but nowhere on the detail page (no `memo` in `InvoiceDetail.tsx`) | Show the Memo / footer in the document card and make it editable on drafts | S |
| INV-16 | Detail | confusing | function | Unit price "$0.01666666666667" (VLX-000002) and "$0.05238095238095" (000003): derived rates, `InvoiceDetail.tsx:1295` | Cap at 4–6 significant digits. For tiered or package lines, show "Tiered — view breakdown" with a popover (Orb and Metronome line-item drilldown) | S |
| INV-17 | Detail | friction | hierarchy | Invoice number in the h1 and again in the document header; ID in the header and the footer; "TAX: MANUAL" raw enum and no tax row at all (`InvoiceDetail.tsx:1489-1498`) | One ID (header, copyable). Always show a Tax row ("Tax — not calculated (manual)" or "$0.00") | S |
| INV-18 | Detail | confusing | function | VLX-000002 notice reads "Payment recovery ended · since 61d ago", but `attention.since` is the finalize date (Aug 13) and escalation was Aug 27. Notice is amber for an exhausted, 31-days-past-due invoice. "Due by Sep 12" shows as plain text. (`S/invoice-VLX-000002-finalized__desktop-light__p1.png`; `components/InvoiceAttention.tsx:88-90`) | Anchor "since" on the reason's own start time, make exhausted plus past-due critical (red), and show "31 days overdue" instead of "Due by" | S |
| INV-19 | Detail | friction | hierarchy | Every notice repeats "This server can't email setup links, so none was sent…" (000001, 2, 5, 6) | That's a setup problem, not an invoice problem. Show it once globally (setup checklist or settings banner) and keep one "Copy setup link" action in the notice | S |
| INV-20 | Detail | friction | interaction | PDF preview is a hand-built `div role=dialog` titled "Invoice Preview -- VLX-000001" (literal `--`), with a "Close" text button and different overlay (`InvoiceDetail.tsx:1774-1787`). The code has no Escape handler or focus trap (inferred). The frame was blank in headless Chromium, which is expected there | Use the shared `Dialog` component, an en dash, and an X close | S |
| INV-21 | Detail | friction | function | Record offline payment always records the full amount, with no date or method (`W/detail1-dlg-record-payment.png`) | Add paid-on date, method (wire / cheque / cash / other) and reference, like Stripe's "Mark as paid → Paid on / Payment method". Partial payments only if the domain supports them | M |
| INV-22 | Detail | polish | consistency | Title case varies: "Mark Invoice Uncollectible" (dialog) vs "Mark uncollectible" (menu); "Email Invoice" / "Send Email"; "Add Line Item"; dialog "Issue credit note" vs list "Issue Credit Note" | Sentence case everywhere (Stripe and Linear convention) | S |
| INV-23 | Detail (mobile) | friction | mobile | Setup pill floats over "Sub: ACME — usage" (`S/invoice-VLX-000001-finalized__mobile-light__p1.png`); line table scrolls sideways (574 vs 310px) | Hide the floating setup pill on detail pages at mobile size, and stack line items (description on top; qty × rate and amount below) | S |
| INV-24 | Hosted | friction | function | Header is just "Invoice" when the company name is unset; empty "FROM" heading; subtotal, Total and Amount Due are three identical rows | Don't render empty sections. Show Total only when discount, tax or credits differ from the subtotal (the dashboard already does this at `InvoiceDetail.tsx:1358`) | S |
| INV-25 | Hosted | polish | function | Bad token shows raw "invoice not found" in lower case with a clock ("expired") icon (`W/hosted-bad-token.png`) | Use "This link isn't valid or has been replaced" with a neutral icon | S |
| INV-26 | PDF | friction | visual | Totals end at x≈533 while the Amount column ends at ≈617; odd rules under Subtotal and Total; memo unlabelled; "Prepaid credits" vs dashboard "Credits Applied"; "Generated on Oct 16" on a simulated invoice issued Nov 16; no pay URL (`W/pdf-*.png`, `pdf.go:680,817,827`) | Right-align totals to the Amount column, label "Memo", use one credits label, add "Pay online: \<hosted link\>" and page x of y (Stripe PDF anatomy) | M |
| CN-1 | Credit Notes | friction | interaction | Create dialog uses a native `<select>` with "Select invoice…" (`CreditNotes.tsx:839`); placeholder "25.00" reads like a value; submit is enabled and errors appear on click, while the invoice-page dialog disables submit instead (`W/cn-create-dialog.png`, `W/cn-create-empty-submit.png`, `W/detail1-dlg-credit-note.png`) | One shared `IssueCreditNoteDialog`, with an invoice combobox showing number, customer and amount due. Prefill when opened from an invoice | M |
| CN-2 | Credit Notes | confusing | function | Amount label uses `getCurrencySymbol()` (tenant default), not the selected invoice's currency (`CreditNotes.tsx:924`); wrong for EUR invoices such as wayne's (inferred from code) | Take the symbol from the selected invoice | S |
| CN-3 | Credit Notes | polish | hierarchy | Empty state has two identical purple "Issue Credit Note" primaries (`S/credit-notes__desktop-light__p1.png`) | Keep the header button; the empty state links to "Open an invoice to credit it" | S |
| DUN-1 | Dunning | confusing | function | "Recovered" card and tab map to state `resolved` (`Dunning.tsx:219,255`), which includes manual void and write-off resolutions | Split into Recovered (payment_recovered) and Closed (void / write-off), or rename to "Resolved" | S |
| DUN-2 | Dunning | friction | a11y, interaction | Expand only via an icon button with no `aria-label` (`Dunning.tsx:327`); clicking the row does nothing; filter not kept in the URL (`:138`) | Make the row expand on click (or open a side drawer), add an aria-label, use `useUrlState` for the filter | S |
| DUN-3 | Dunning | polish | consistency | "Started" shows "yesterday" or "21m ago" for some rows and "Aug 13, 2026, 12:00 AM UTC" for others (`Dunning.tsx:84-95`); raw lowercase "active" / "escalated" | One format: absolute short date with relative time as a tooltip. Humanised badge labels | S |
| DUN-4 | Dunning | confusing | function | Resolve dialog preselects "Payment recovered", which marks the invoice paid; description uses `--` (`Dunning.tsx:558`; `W/dunning-resolve-dialog.png`) | No default selection, so Resolve stays disabled until a choice is made. Use an en dash | S |
| DUN-5 | Dunning (mobile) | friction | mobile | Four full-width KPI cards push the table below the fold (`W/m-dunning.png`); table is 1050px wide inside 358 | 2×2 compact KPI grid, card rows on mobile | S |
| DUN-6 | Nav | polish | visual | Dunning and Dunning policies use the same `AlertTriangle` icon (`components/Layout.tsx:47-48`) | Make Policies a sub-tab of Dunning (Runs / Policies), as Stripe's Revenue recovery groups them, or give it a settings icon | S |
| POL-1 | Dunning policies | confusing | function | Seeded policy "Max retries 4" with 4 schedule entries, while the form says "Need 3 entries for 4 retries" (`DunningPolicies.tsx:325`; `W/policy-edit-invalid.png`) | Decide the meaning and show it as a timeline: "Day 0 fail → retry day 1 → day 4 → day 11 → day 25 → then pause". Derive max retries from the schedule | M |
| POL-2 | Dunning policies | friction | interaction | "Comma-separated Go durations (h/m/s)"; "banana" gets only "Missing 2 entries"; the server toast leaks "Max_retry_attempts … retry_schedule"; list shows "24h · 72h · 168h · 336h" | Day-based chip inputs ("+ add retry after [3] days"), client-side duration check, humanised server errors | M |
| POL-3 | Dunning policies | confusing | function | "0 assigned" on the default policy, even though it governs all 4 active runs | Default shows "Applies to all customers without an override (N)" | S |

## C) Proposed page anatomy

**Invoice detail.** A summary header and a right rail, with the document as one tab. The model is Stripe's invoice page: amount, status and due date first; details in a side column; the document renders the same as the PDF.
```
← Invoices
VLX-000002  [Past due · 31d]  [Simulated]                [Send reminder]  [Collect ▾]  [⋯]
$566.00 due · Globex Corporation · due Sep 12, 2026 · Pro (Jul 13 – Aug 12)
┌─ Notice (only if attention; critical=red) ────────────────────────────────────────────┐
│ Payment recovery ended Aug 27 · 4 retries · No card on file   [Copy setup link] [Open customer] │
└──────────────────────────────────────────────────────────────────────────────────────┘
[ Summary | Line items | Payments & recovery | Activity | Document (PDF preview) ]
┌ Line items ─────────────────────────────────────┐  ┌ Details ───────────────────┐
│ Pro base fee            1 × $49.00       $49.00 │  │ Customer  Globex → link     │
│ API calls (usage) 103,000 × $0.005      $515.00 │  │ Subscription  Pro → link    │
│   └ View 103,000 events →                       │  │ Issued Aug 13 · Net 30      │
│ Storage (GB)   120 · tiered ⓘ             $2.00 │  │ Currency USD · Tax: manual  │
│                         Subtotal        $566.00 │  │ Hosted link [Copy] [Rotate] │
│                         Tax (not calc.)   $0.00 │  │ Memo  …                     │
│                         Amount due      $566.00 │  │ ID vlx_inv_… [copy]         │
└─────────────────────────────────────────────────┘  └ Credit notes (0) [+ Issue]  ┘
```
Why: what the operator needs to know (amount, how overdue, why, next action) is visible without scrolling. The paper-style document moves to its own tab, so it stops pushing everything else down the page.

**Invoices list.** Stripe's invoice list, with a filter-pill bar, combined status, and bulk actions.
```
Invoices                                                   [Export ▾] [+ Create invoice]
[ All 7 | Draft 3 | Open 4 | Past due 1 | Paid 0 | Void 0 ]   ← counts on tabs
[🔍 number, customer, email]  [+ Customer] [+ Date: Issued ▾ between…] [+ Amount] [Clear]
☐ Amount     Status          Number       Customer           Due            Created   ⋯
☐ $566.00    Past due 31d    VLX-000002   Globex Corp  ⚗     Sep 12, 2026   Aug 13    ⋯
☐ $4,160.00  Open            VLX-000006   Stark Industries   Oct 31 (15d)   Oct 16    ⋯
☐ $99.00     Draft           VLX-000007   Northwind          —              Oct 16    ⋯
[ 2 selected: Finalize · Send · Download PDFs ]                 Showing 1–7 of 7
```
Why: amount comes first because that's what people scan for. One status badge replaces three overlapping signals. Filters are visible, labelled, and kept in the URL, and Export follows them.

**Dunning.** Runs and Policies as tabs of one Collections page, like Stripe's Revenue recovery.
```
Collections            [Runs | Policies]
At risk $4,754.88 (USD) · Active 3 · Escalated 1 · Recovered 0 · Written off/voided 0   (one compact strip)
[All | Active | Escalated | Recovered | Closed]   [🔍 customer/invoice]
Customer        Invoice     Amount     Stage               Next step              Started
ACME Corp       VLX-000001  $3.88      No card · day 1     Retry Nov 17 (no card)  Nov 16   [Resolve]
 └ click row → right drawer: timeline, policy applied, actions
```

## D) Cross-cutting patterns to standardise
- **One status vocabulary:** `invoiceStatusLabel` / `dunningStateLabel` used on list, detail, hosted, PDF, badges and empty-state copy (INV-7, DUN-3, DUN-1).
- **One count source:** sidebar badges and page counts use the same predicate, including how simulated items are handled (INV-6).
- **A seller identity check:** finalize, PDF, hosted and email share one company-profile completeness check, and none of them ever falls back to "Velox", "VELOX" or "Your Company" (INV-2).
- **A shared `FilterBar`** (search, pills, URL state, toolbar that stays put on empty results), used by Invoices, Credit Notes and Dunning.
- **A shared `IssueCreditNoteDialog`** and an invoice combobox (CN-1).
- **Dialog hygiene:** always the shared `Dialog`, sentence-case titles, no `--`, no default selection on actions that move money (INV-20, INV-22, DUN-4).
- **Date display:** absolute date with relative time on hover, in the clock the page states (DUN-3, INV-18).
- **Responsive tables:** a card-row variant under `sm`, and the floating setup pill hidden on detail pages at mobile size (INV-5, INV-23, DUN-5).
- **Server error mapping** before showing anything to a customer or in a toast (INV-3, POL-2).

## E) Already good — keep
- One primary action per state, with grouped overflow menus and Void separated at the bottom (`InvoiceDetail.tsx:722-928`).
- Typed confirmation for Void ("VOID") and Write-off ("WRITE OFF"), with plain-language consequences (`W/detail1-dlg-void.png`, `W/detail1-dlg-mark-uncollectible.png`).
- Server-computed attention notices with reasons and prescribed actions, and the test-clock "Simulated time" line.
- Usage lines link straight to the events behind them; units show as "$3.00 / 1M tokens"; per-line period appears when it differs from the invoice.
- The Activity timeline separates when something happened from when it was recorded. Credits applied show in green in the summary.
- The hosted page's test-mode banner, Amount due headline and Download PDF; it correctly ignores dark mode.
- The dunning Resolve dialog's three outcomes each come with a clear explanation. The policy form's "When all retries fail" section and its consequence text are good.
- Dark mode contrast is acceptable on every page I checked.

**Limits.** The populated Credit Notes list, paid, voided and uncollectible invoice states, and the PDF preview couldn't be seen. The seed has no credit notes, and headless Chromium doesn't render PDFs. Both date-picker open attempts timed out, so there's no screenshot of an open picker.
