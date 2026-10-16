I audited the Customers list and all 8 customer detail pages: every slice in desktop light, dark and mobile, plus the interactive states. **The detail page is the weak spot:** it doesn't fail on any single card, but its layout and several wrong or silent numbers make it hard to trust.

- **Biggest problems:**
  - The two sections you flagged both mislead.
    - "Sent emails" lists emails that were never sent and hides the reason.
    - "Activity" opens on a window that shows $0 for the main demo customer.
  - The EUR customer is shown in dollars everywhere except the forecast.
  - Stark's $500 credit leaves no trace on the page.
  - The "Generate URL" button can silently kill a link the customer already uses.
  - Invoices and subscriptions, the things an operator comes for, start about 2,500px down.
- **Safety:** I submitted nothing. A guard in the Playwright script blocked every write request, Archive was opened and cancelled, and "Generate URL" was clicked against a mocked server response.
- **Interactive states not seen:** the Margin "Custom" date pickers fell below the viewport in my shot, and expanding the Globex "API calls" row timed out. Everything else was captured.

All screenshot paths below are in `/tmp/claude-0/ux/` (`work-customers/` is my interactive-state folder). Code paths are in `web-v2/src/`.

## A) Verdicts per page

**Customers list (`customers__*`).** It works for finding a customer: server-side search, sort, filters, a clear empty state and a test-clock badge. But it only tells you who exists, not who matters. There are no billing columns (plan, outstanding, credit, current spend). Every row reads "active", including Umbrella, whose subscription is canceled. On mobile the page scrolls sideways.

**Customer detail, all 8 customers.** One column of 13–15 cards that all look equally important. The page is 2,863–3,642px tall on desktop and 3,679–4,886px on mobile. Static data (a Details card repeating the header, a large empty billing-profile card, a 5-row dunning policy) sits above the money. Several numbers are wrong or silent:
- EUR shown in $.
- 100% margin when no provider costs are set.
- "Projection unavailable" when the real answer is $0.
- Emails marked "pending" that will never send.
- Credits that vanish without a trace.

**Per customer:**
- **acme:** the demo customer looks idle by default. Usage only shows on the 90d tab, and the chart's axis labels are cut off.
- **globex:** the richest page, and the one that shows the Sent-emails and draft-invoice problems.
- **hooli:** about 2,900px of mostly empty cards.
- **wayne:** shows $ for a EUR customer.
- **stark:** the $500 credit is invisible.
- **umbrella:** the canceled subscription is hidden behind a collapsed row, while the header strip still says "Subscriptions 1".

## B) Findings

| id | page | severity | category | evidence (shot + code) | recommendation | size |
|---|---|---|---|---|---|---|
| CU-01 | detail · Sent emails | broken | function | `slices/customer-globex__desktop-light__p3.png`, `work-customers/d-14-sent-emails.png`. The API returns `last_error: "email: SMTP not configured (SMTP_HOST unset)"` on all 4 rows, but the UI shows the error only when `status==='failed'` (`CustomerDetail.tsx:1183`). The rows read "pending" forever with no reason. | Show the error for every status. Give stuck rows a distinct label: "Not sent — SMTP not configured", with a link to Settings → Email. Badge tone should be warning, not neutral grey. | S |
| CU-02 | detail · Sent emails | confusing | function | Same shots. Titled "Sent emails" but the list includes pending, failed and skipped rows. 3 identical "Payment retry — action required" rows with no attempt number, all at the same minute (`:1166-1193`, labels `:108-119`). | Rename to "Emails". Add "Retry 1/2/3 of 4" from the dunning run. Group by invoice, Resend-style: a row per email, a status dot, and an expandable event trail (queued → sent → delivered/bounced). | M |
| CU-03 | detail · Sent emails | confusing | interaction | The rows can't be clicked and have no hover state (`d-14`). The invoice link goes to the invoice *list* search (`:1174`), not the invoice. No preview, no resend. | Clicking a row opens a side panel with recipient(s) including CCs, subject, a rendered preview, provider message id and Resend. Link the invoice to `/invoices/:id`. Borrow Stripe's customer "Emails" log and Resend's email detail view. | M |
| CU-04 | detail · Sent emails | confusing | function | Globex is on simulated Oct 13, but the email rows show Oct 16 (wall clock). The test-clock banner above says "Dates on this page follow the test clock". "Last 30 days" has no anchor (`:1160,1190`). | Mark the section "real time" or show both times. Either way, the banner must not over-claim. | S |
| CU-05 | detail · Sent emails | friction | mobile | `slices/customer-globex__mobile-light__p4.png`: the subject wraps to 4 lines and the recipient is cut to "to finance@g…" because the badge and date column don't shrink (`:1187`). | On narrow screens, stack the status and date under the subject. | S |
| CU-06 | detail · Activity | broken | function | `slices/customer-acme__desktop-light__p2.png`, `work-customers/acme-activity-Cycle.png`, `acme-activity-90d.png`. Acme has 775k tokens / $3.88 of usage (API checked), but the default 30d window (Oct 18–Nov 17, simulated) and "Cycle" both show $0.00 with an empty chart. The usage only appears on 90d. Default is `'last_30d'` (`CostDashboard.tsx:362`). | Default to "Cycle"; if the cycle is empty, fall back to the last period that has usage. When the window is empty, say "No usage Oct 18–Nov 17 · last usage Oct 16 ($3.88) → show". | S |
| CU-07 | detail · Activity | broken | visual | `work-customers/acme-activity-90d.png`: y-axis labels cut to "00000" because `YAxis width={40}` (`CostDashboard.tsx:576`). | Use a compact number format (`800K`) and a wider axis. | S |
| CU-08 | detail · Activity | confusing | function | `work-customers/d-11-chart-tooltip.png`: API calls and GB of storage are stacked on one unitless axis (`:600-607`). No legend; the storage segment is nearly invisible. | Chart in money ($/day, stacked by meter, one currency), or one small chart per meter. Add a legend. Borrow Orb's or OpenAI's usage page: a spend chart plus a per-meter table. | M |
| CU-09 | detail · Activity | confusing | function | Labelled "Billed usage" (`:451`) but it is usage priced for any window, not invoiced. Initech shows "$15.00 billed" during a trial (`slices/customer-initech__desktop-light__p2.png`). Margin calls the same figure "Usage billed" (`MarginCard.tsx:116`). | Rename to "Usage charges (est.)" in both places. Note "trial — not invoiced" when the customer is trialing. | S |
| CU-10 | detail · Activity | confusing | function | Globex has 2 subscriptions. "Cycle" shows Oct 13 → Nov 1 (Team seats' period) but $45, which is Pro's usage (`work-customers/d-10-activity-Cycle.png`). Cycle and Upcoming also disagree on end dates (Dec 16 vs "Nov 16 – Dec 15"). | Name the subscription the cycle belongs to, with a picker. Use one end-date convention everywhere. | M |
| CU-11 | detail · Activity | friction | hierarchy | One Card per meter row (`:655`) plus a separate card for the total. The empty state shows a blank chart grid when meters exist at zero (`:459`). | One card: total + chart + a table of meters (meter, qty, rate, amount, share bar) with expandable rows. Show a real empty state when all meters are 0. | M |
| CU-12 | detail · Upcoming | confusing | function | Acme: "Projection unavailable for this cycle" (`slices/customer-acme__desktop-light__p1.png`). The preview API returns 200 with no lines, i.e. $0, and the card's test treats that as unavailable (`CostDashboard.tsx:208`). Real fetch errors are also swallowed (`:155-162`). | Empty → "$0.00 so far · usage-only plan". Error → "Couldn't load projection · Retry". | S |
| CU-13 | detail · Upcoming | confusing | consistency | The "Last invoice" line has no status filter (`:170-182`; the comment says finalized/paid) and shows `payment_status` ("pending") for VLX-000004, which is a **draft** (`work-customers/d-09-upcoming-expanded.png`). Recent Invoices shows "draft" / "finalized" for the same invoices. It also uses `amount_due` while Recent Invoices uses `total`. | Exclude drafts, or label them "draft". Use one status vocabulary on the page: invoice status, plus payment status as a second badge only after finalization. | S |
| CU-14 | detail · Upcoming | friction | interaction | Only one subscription shows; the others sit behind a grey "+ 1 more subscription" text button (`:122-130`). The card titles use the plan name ("Team seats") while the Subscriptions card uses the subscription name ("Globex — Team seats (calendar)"). | Show every active subscription as compact rows (name · period progress · projected $), expandable. Use the subscription name with the plan underneath. | M |
| CU-15 | detail · Margin | broken | function | Globex, Northwind and Initech show "Provider cost $0.0000 / Margin 100.0%" because no provider rates are configured (`slices/customer-globex__desktop-light__p2.png`; `MarginCard.tsx:127-129`, notes only when `unresolved_events>0` `:178`). | If cost is 0 and no rate matched: "No provider costs configured → Provider costs", and show margin as "—", not 100%. | S |
| CU-16 | detail · Margin | friction | consistency | A native `<select>` with presets 7/30/90/Custom (`MarginCard.tsx:81-90`) beside Activity's segmented control Cycle/7d/30d/90d. The two windows don't sync. Cost uses 4 decimals ($0.0000) while billed uses 2. | One page-level date range shared by Activity and Margin, using the same component. Use 2 decimals unless the value is under $0.01. | M |
| CU-17 | detail · all money | broken | function | Wayne (EUR plan): Credit Balance, Billed usage and Margin all show **$0.00** (`slices/customer-wayne__desktop-light__p1.png` and `__p2.png`). Only Upcoming shows €45. The currency falls back to the workspace default when there is no billing profile or invoice (`CustomerDetail.tsx:263-265`, `CostDashboard.tsx:397`, `MarginCard.tsx:62`). | Take the currency from the active subscription's plan before the workspace default. | S |
| CU-18 | detail · credits | broken | function | Stark: a $500 grant was fully applied to VLX-000006 (ledger API checked). The page shows "Credit Balance $0.00", no burndown card (`listCreditGrants` excludes used-up grants by default, `api.ts:336`, gated at `:885`), and Outstanding $4,160 vs invoice $4,660 with no explanation. | Credits card: balance, granted/used/expired totals (the API already returns `total_granted`/`total_used`), the last 3 ledger entries, and a "Show used-up grants" toggle. Add a **Grant credits** action on the page; today it only exists on /credits. | M |
| CU-19 | detail · cost-dashboard URL | broken | function | The card never knows whether a URL already exists. On every load it says "Generate URL" (`CustomerDetail.tsx:2383-2386`), and clicking it rotates the token with no confirm, killing any embedded link. The toast says "URL rotated" even on the first generate (`work-customers/hooli-generate-url-mocked.png`, mocked). The button is a purple primary. | Load the token status: "Active since <date> · Rotate…" with an AlertDialog confirm, or "Not generated · Generate". Make it a secondary button. Move it to a Settings/Integrations tab. | M |
| CU-20 | detail · anatomy | confusing | hierarchy | 13–15 equally weighted cards. Subscriptions and invoices start at about y≈2560 of 3212 on acme. The Details card (`:644-675`) repeats email, status, created and ID from the header and strip; email shows 3 times. Dunning (5 rows, `:822-866`) and an empty Billing Profile (about 290px, `:798-816`) sit above the money. | Redesign per wireframe C1. | L |
| CU-21 | detail · summary strip | confusing | hierarchy | The strip shows Email and Created, which aren't KPIs. "Outstanding" disappears at 0 (`:620`), so the strip is 4 or 5 columns depending on the customer. "Subscriptions" counts canceled ones: Umbrella shows 1 with none active (`:633`). Outstanding red has weak contrast in dark mode (`slices/customer-globex__desktop-dark__p1.png`). | Fixed KPIs: MRR / current-period charges, Outstanding (show $0.00), Credit balance, Next invoice (date + projected $). Email and Created go to the right rail. | M |
| CU-22 | detail · header | friction | visual | Five header controls plus the status badge in one row (`slices/customer-acme__desktop-light__p1.png`, `:473-533`). The badge floats at the far right after Archive. Archive is an equal-weight red button. On mobile it wraps to 2 lines (`slices/customer-globex__mobile-light__p1.png`). | Status badge next to the name. Primary actions "New invoice" and "Create subscription", then a "…" overflow menu (Edit, Grant credits, Audit log, Archive). Borrow Stripe's customer header. | S |
| CU-23 | detail · buttons | polish | consistency | Up to 6 purple primaries on one page: New invoice, Set Up Billing Profile, Generate URL, Add payment method, Create Subscription, Rotate again (`:475,810,947,1066,2384`). Button casing is mixed: "Set Up Billing Profile" / "Create Subscription" vs "New invoice" / "Add override". | One primary per view; card-level actions are outline or ghost. Use sentence case everywhere. | S |
| CU-24 | detail · billing profile | friction | hierarchy | The empty state is a large centered card. When filled it takes 4 sections. The New invoice dialog has no warning about a missing profile (inferred). | Right rail "Billing details" block: legal name, address, tax ID, currency, with an inline "Add" link when empty. | M |
| CU-25 | detail · payment methods | polish | consistency | Two empty states for the same thing: an amber callout when a card is needed, a centered icon otherwise (`:954-971`). Umbrella and Hooli differ from the rest. | One compact row in the right rail: "No card on file · Request card", amber only when a subscription bills automatically. | S |
| CU-26 | detail · empty states | polish | consistency | Center-aligned ("No subscriptions", "No metered usage", "No negotiated prices") vs left-aligned ("No invoices", "No emails sent") (`:1113`, `:1141`, `:1196`). | One shared `EmptyRow` component. | S |
| CU-27 | detail · invoices | friction | function | Recent Invoices has no due date, no payment status, no subscription, and no "View all" link. Globex has a Sep 13 draft ($435) a month old with no flag (`work-customers/d-14-sent-emails.png`). | An invoices tab with a table: number, status, payment status, due, amount, subscription. Flag "Draft older than 7 days". Add a "View all →" link to `/invoices?customer=`. | M |
| CU-28 | New invoice dialog | friction | interaction | Unit price is entered in **cents** ("Amounts are in cents (e.g. 2500 = $25.00)") while the override dialog takes dollars ($0.01). The footer scrolls out of view in a 90vh dialog, so Save is not visible at 900px height. Native date inputs (`work-customers/d-02-new-invoice.png`, `:2002`, `:2213`, `:2190`/`:2225`). | Dollar input with currency formatting. Sticky dialog footer. Use the shared DatePicker. | M |
| CU-29 | Billing profile dialog | polish | visual | Tax status shows the raw value "standard" (`work-customers/d-04-billing-profile.png`). Currency is below the fold. | Human labels ("Standard (taxable)"). Put Currency near the top. | S |
| CU-30 | list | confusing | function | Columns are Name / Email / Status / Created only (`Customers.tsx:319-322`). Umbrella (subscription canceled) and Hooli (no subscription) read "active" like everyone else (`slices/customers__desktop-light__p1.png`). | Add columns for plan/subscription state, MRR, outstanding (red when > 0) and credit balance, plus a filter "Has outstanding". Borrow Stripe's customer list and Orb's customer table. | M |
| CU-31 | list | broken | mobile | The mobile capture is 620px wide for a 390px viewport (`shots/customers__mobile-light.png`), so the table forces sideways scrolling. | Below the `md` breakpoint, show a card list (name, email, status) or hide Email and Created. | S |
| CU-32 | list | polish | visual | A green left stripe per row repeats the "active" badge (`Customers.tsx:329`). It is not visible on the last row (Globex). | Drop the stripe on this list, or reserve it for states that need attention. | S |
| CU-33 | list | polish | consistency | "Add Customer" button → "Create Customer" dialog → "Create Customer" submit (`:229/433/531`). The subtitle "· 8 total" is really the filtered count and disappears when a search matches nothing. The search empty state says "Try a different filter" (`work-customers/list-03-search-empty.png`). | One verb ("Add customer", sentence case). Show "8 customers" unfiltered and "3 of 8" when filtered. Empty-search copy should mention the search. | S |
| CU-34 | Create Subscription dialog | polish | consistency | Native `<select>` for plan and cycle, Display Name before Plan (`work-customers/d-08-create-sub.png`). | Pick the plan first, auto-fill the name, use Combobox (shared with the Subscriptions area). | S |

## C) Proposed page anatomy

**C1. Customer detail.** Borrow Stripe's customer page: summary header, a right rail of key details, and a tabbed body. Borrow Orb's and Metronome's per-customer usage/billing tabs for the money.

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ ← Customers                                                                      │
│ Globex Corporation  [active] [⏱ Simulated · Oct 13]       [New invoice] [Create  │
│ globex · vlx_cus_…2k0 ⧉                                    subscription] [ ⋯ ]   │
├──────────────────────────────────────────────────────────────────────────────────┤
│ MRR $169 │ Outstanding $566 · 1 overdue → │ Credit $0 (of $0) │ Next inv Oct 31 $120│
├──────────────────────────────────────────────────────────────────────────────────┤
│ ⚠ No card on file — renewals can't be charged.  [Request card]     (one notices  │
│ ⚠ 4 emails not sent — SMTP not configured.       [Settings]          stack, max 3)│
├───────────────────────────────────────────────────────┬──────────────────────────┤
│ [Overview] [Usage] [Invoices] [Credits] [Emails & events] [Settings]             │
│                                                       │ DETAILS          [Edit]  │
│ OVERVIEW                                              │ Email finance@globex     │
│ ┌ Subscriptions ───────────────────────────────────┐  │ +2 CC                    │
│ │ ● Team seats (calendar)  Oct13–Oct31 ▓░░ $120 →  │  │ External ID globex ⧉     │
│ │ ● Pro                    Oct13–Nov12 ▓░░  $94 →  │  │ Created Jul 13           │
│ │ ▸ 0 past                                         │  │ Currency USD             │
│ └──────────────────────────────────────────────────┘  │ ──────────────────────── │
│ ┌ Invoices ─────────────────────── View all → ─────┐  │ BILLING      [Add]       │
│ │ VLX-000004  draft      Oct 13         $199       │  │ No legal name / address  │
│ │ VLX-000003  draft ⚠30d Sep 13         $435       │  │ ──────────────────────── │
│ │ VLX-000002  open·failed due Sep 12    $566       │  │ PAYMENT METHOD           │
│ └──────────────────────────────────────────────────┘  │ None · Request card      │
│ ┌ Usage this cycle (Pro) ──────── Open Usage → ────┐  │ ──────────────────────── │
│ │ $45.00  ▁▁▁▁▁▁▇  API calls $45 · Storage $0      │  │ DUNNING                  │
│ └──────────────────────────────────────────────────┘  │ AI default retry ·       │
│ ┌ Recent activity ─────────── Emails & events → ───┐  │ 4 tries          Change  │
│ │ Oct 16 ✉ Retries exhausted  VLX-000002  not sent │  │ ──────────────────────── │
│ │ Oct 16 ✉ Retry 3/4 reminder VLX-000002  not sent │  │ PRICE OVERRIDES          │
│ │ Oct 13 ⎘ Invoice VLX-000004 drafted              │  │ None (list price)  Add   │
│ └──────────────────────────────────────────────────┘  │                          │
└───────────────────────────────────────────────────────┴──────────────────────────┘
 Usage tab:    one date-range control → spend chart (stacked $ by meter, legend)
               + meter table (qty · rate · $ · share, expand by price)
               + Margin strip (usage $ / provider cost / margin, or "no rates set")
 Credits tab:  balance, grants table (incl. used-up), ledger, [Grant credits]
 Emails & events: Resend-style log; row → side panel with preview, attempts, Resend
 Settings tab: public cost-dashboard URL (status + Rotate…), test clock, archive
```

Why: money and attention items move into the top 600px. Static configuration moves to a compact rail. Rarely used tools (dashboard URL, dunning, overrides) stop competing equally for the scroll.

**C2. Usage tab (replaces the Activity section).**

```
Usage   [Cycle: Pro Oct 13–Nov 12 ▾] [7d] [30d] [90d] [Custom]     Last usage: Oct 13
┌ $45.00 usage charges (est.) · not yet invoiced ─────────────────────────────────┐
│  $ ┤            ▇                                        ■ API calls ■ Storage │
│    └──────────────────────────────────────────────────────────────────────────  │
├─────────────────────────────────────────────────────────────────────────────────┤
│ Meter       Quantity        Rate           Amount   Share                       │
│ ▸ API calls 9,000 calls     $5.00/1K       $45.00   ██████████ 100%             │
│ ▸ Storage   0 GB            $0.10/GB        $0.00                               │
├─────────────────────────────────────────────────────────────────────────────────┤
│ Margin  usage $45.00 · provider cost — · ⓘ No provider rates set → Provider costs│
└─────────────────────────────────────────────────────────────────────────────────┘
```

Why: one date range, one currency axis, and one table instead of separate cards. Margin sits right next to the usage it is computed from.

**C3. Customers list.**

```
Customers  128                                        [Export] [Add customer]
[All|Active|Past due|Trialing|No subscription|Archived]  [Search…]  [Has outstanding ☐]
Name / ext id        Plan              MRR    Outstanding   Credit   Created
Globex ⏱ globex      Pro + Team seats  $169   $566 (red)    $0       Jul 13
Umbrella  umbrella   — (canceled)      $0     $25 (red)     $0       Oct 16
```

Why: the list becomes a place to triage who owes money, not just a directory. This also fixes the mobile overflow (CU-31).

## D) Cross-cutting patterns
- **One status vocabulary:** an `InvoiceStatus` badge component showing invoice status plus payment status, used by Upcoming, Recent Invoices and the invoice pages (CU-13).
- **A shared `DateRangeControl`** (presets + custom, one look) to replace the segmented tabs, the native `<select>` and native date inputs. Margin, Activity, New invoice and the Create Subscription selects all use different controls today.
- **A shared `MoneyInput`** in major units (dollars) with the currency symbol; cents entry exists only in the New invoice dialog (CU-28).
- **A customer-currency resolver** (subscription plan → billing profile → invoices → workspace default) used by every money display (CU-17).
- **A `NoticeStack` under page headers**, so warnings (no card, SMTP not configured, unpriced usage, stale drafts) appear once at the top instead of inside cards.
- **One `EmptyRow`/`EmptyState` style** (CU-26), and a one-primary-button-per-view rule (CU-23).
- **Sticky dialog footers** for any dialog with `max-h-[..vh] overflow-y-auto`.
- **The test-clock banner** should name which sections run on simulated time (CU-04).

## E) Already good — keep
- The Customers list itself: server-side search, sort and filter state in the URL, "Clear filters" empty states, a stale-page guard, the "Simulated" badge, Export using the server stream.
- Create-customer validation shows clear inline errors (`work-customers/list-02-add-validation.png`). The test-clock pin explains that it is permanent.
- The Archive confirm says exactly what happens ("unpaid invoices are still collected") with a destructive button (`work-customers/d-03-archive-confirm.png`).
- The Dunning dialog previews the policy's effect before saving (`work-customers/d-05-dunning-change.png`).
- The price-override dialog explains when the override takes effect and the test-clock caveat (`work-customers/d-07-add-override.png`).
- The add-payment-method flow (email or copy a single-use link, no card data in the dashboard; `work-customers/d-06-add-pm.png`).
- The usage breakdown by price, with dimension chips, rates per 1M tokens and share bars (`work-customers/acme-meter-expand.png`). Keep it inside the new meter table.
- Margin's honest header copy ("not accounting margin") and its currency-mismatch handling.
- The cost-dashboard card's warning that the URL is a bare path because `VELOX_API_BASE_URL` is unset.
- A credit balance that failed to load shows "—" with Retry, never $0.
- Dark mode is generally solid; only the Outstanding red needs a lighter shade.
