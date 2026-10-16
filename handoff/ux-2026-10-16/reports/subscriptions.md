I've audited the Subscriptions list, all 8 seeded subscription detail pages, the Test clocks list and both test clock detail pages. I captured every dialog you named in light, dark and mobile, opening each and cancelling. Nothing was submitted and the repo is unchanged.

The short answer to your question: an operator can tell when a subscription renews, but not what it costs, what the next invoice will be, or what an action will do to money.
- **Cost:** the page shows only each plan's base fee. Usage-priced plans read as "$0.00/mo".
- **Next invoice:** the preview card throws away the server's caveats and the period it covers.
- **Actions:** every dialog explains the rule well in words, but none shows an amount.
- **Canceled subscriptions:** the page still says "Renews Nov 16, 2026".
- **Mobile:** the Advance and Extend trial dialogs overflow their own edges, and on the list page the Add Subscription button is pushed off-screen.

Screenshots are in `/tmp/claude-0/ux/work-subscriptions/` (scripts `shoot.cjs` and `shoot2.cjs` are there too). Route shots are cited as `slices/<page>__<variant>__pN.png`, under `/tmp/claude-0/ux/`.

## A) Verdicts per page

**Subscriptions list**
- The table is clean, sorting is server-side, filter state lives in the URL, and the "Simulated" chip is a nice touch.
- It does not answer "what is this worth": there is no price/MRR column.
- A canceled subscription shows a Next Billing date.
- A subscription whose collection was paused by dunning (Globex — Pro) shows as plain "active" and does not appear under the Paused filter.
- At 390px the header does not wrap, so Add Subscription is off-screen.

**Subscription detail (all 8)**
- Each fact appears 2–3 times. Status appears 3 times; customer, code, ID and renewal date twice each.
- The two things that matter most sit at the bottom: the next invoice preview and invoice history. "Spend thresholds" and "Properties" sit above them.
- Specific failures:
  - Canceled Umbrella: the summary says "Renews Nov 16, 2026" and the timeline shows a "Next Billing" dot.
  - ACME, usage-only plan: the plan reads "$0.00/mo" and the preview table is empty, with no message.
  - Calendar seats: the preview says $120 for a partial first period. I believe the engine prorates this, so it is probably overstated (see SUB-12).
  - Globex — Pro: "Collection paused" never says dunning caused it or how much is outstanding.
- Dialog wording is honest and specific about timing. What is missing is the money preview.

**Test clocks list**
- Works, but each card shows only name, status, clock time and created date.
- There is no count of pinned customers or subscriptions, and no "ahead of / behind real time" offset.
- On mobile the clock name truncates to "globex-h…".

**Test clock detail**
- The basics are right: status, Advance, a delete dialog that names the blast radius, and a "Last advance results" card.
- But it is a column of identical cards. Subscriptions are listed by code ("globex-pro"), with no amounts.
- "Invoices generated 1" is not a link.
- The Advance dialog doesn't say which renewals the jump will cross or what they will bill.
- There is no history of past advances.

**Dialogs**
- Cancel (radio cards, safe default, typed confirm) and Pause collection (explains drafts) are good patterns.
- Validation errors appear as toasts outside the dialog.
- Change Plan and Add Item offer EUR plans on a USD subscription, which the backend then rejects.

## B) Findings

| id | page | severity | category | evidence | recommendation | size |
|---|---|---|---|---|---|---|
| SUB-01 | Sub detail | broken | function | `slices/subscription-umbrella-starter__desktop-light__p1.png` (canceled sub: "Billing cycle: Renews Nov 16, 2026" and a "Next Billing Nov 16" dot); `SubscriptionDetail.tsx:767-776`, `:643-650` (no canceled guard; Properties already guards at `:998`) | When canceled, show "Canceled Oct 16, 2026" (`canceled_at`) in the summary, and end the timeline with a red "Canceled" dot. | S |
| SUB-02 | Sub list | broken | function | `slices/subscriptions__desktop-light__p1.png`, Umbrella row "canceled · Nov 16, 2026"; `Subscriptions.tsx:383-385` | Show "—" or "Ended {canceled_at}" for canceled rows. | S |
| SUB-03 | Sub detail | broken | function | ACME `slices/subscription-acme-tokens__desktop-light__p1.png`: "Next Invoice Preview" has headers and no rows or subtotal; `SubscriptionDetail.tsx:1043-1069` maps `preview.lines` with no empty branch | When there are no lines, say "No charges yet this period — usage so far: 0 → $0.00", with the period dates. | S |
| SUB-04 | Sub detail | confusing | function | Preview card drops `warnings`, `billing_period_start/end` and `generated_at`. Grep finds none of them in `SubscriptionDetail.tsx`; the type is in `lib/api.ts:1424-1434`. The backend emits honesty warnings, e.g. "Estimate excludes mid-period proration…" (`internal/billing/preview.go:237,269`). | Title the card "Upcoming invoice · {period} · est. as of {generated_at}" and render warnings as an amber Callout (pattern from `CostDashboard.tsx:473-494`). Borrow Stripe's "Upcoming invoice" header. | S |
| SUB-05 | Sub detail | confusing | function | Price shown is the base fee only: ACME "AI API $0.00/mo" (acme p1); Globex Pro "$49.00/mo" while its invoices are $566 / $435 / $199 (globex-pro p1–p2); `SubscriptionDetail.tsx:724`, `:836` | Show the pricing model, e.g. "$49/mo + usage (API calls $0.005/call, Storage …)". In the header, show "Est. next invoice $143 on Nov 16" taken from the preview. Borrow Stripe's subscription "Pricing" table (price × qty = total) and Orb's "Current period usage" per meter. | M |
| SUB-06 | Sub detail | confusing | consistency | `20-cancel-choice-default.png`: "current period ends on Nov 16" and "keeps access until Nov 16", while the timeline and Properties say "Period End Nov 15" / "Oct 16 – Nov 15". `SubscriptionDetail.tsx:1295,1312` use `formatDate(period_end)` (exclusive boundary); the timeline uses `formatCivilDate` (`:612`) | Use one convention. "Access through Nov 15; no renewal on Nov 16" names both dates without contradiction. | S |
| SUB-07 | Sub detail | confusing | function | Globex Pro p1: grey "Collection paused — New invoices are saved as drafts" with no cause or amount; only Activity (p2) says "paused by dunning". There is $566 pending plus $634 in drafts. `SubscriptionDetail.tsx:397-410` | When dunning caused the pause: amber tone, "Paused by dunning on Aug 27 after N failed attempts · $1,200 unpaid across 3 invoices", plus a link to the dunning run. | M |
| SUB-08 | Sub list | confusing | function | `08b-list-paused.png` shows "No paused subscriptions" while Globex — Pro is collection-paused; the list row shows only "active". The detail-page "Pause" button (`SubscriptionDetail.tsx:456`) creates exactly this state. `Subscriptions.tsx:206-213` | Add a "Collection paused" chip next to the status badge, and make the Paused filter include `pause_collection`, or add its own tab. Also add a "Trialing" tab. | M |
| SUB-09 | Sub list | broken | mobile | `53-mobile-list-real.png`: filter tabs clipped at "Activ…"; Add Subscription off-screen; table needs horizontal scroll to reach Status / Next billing. `Subscriptions.tsx:192` (`flex justify-between`, no wrap) | Wrap the header; move the filter tabs to their own row with `overflow-x-auto`. On mobile, render rows as cards (name, customer, status, next bill). Linear's mobile list is the model. | M |
| SUB-10 | Sub detail / Test clock | broken | mobile, visual | `30-extend-trial.png` (desktop): time field, description and Extend button overflow past the dialog's right edge. `51-mobile-advance.png`: same in the Advance dialog (time field, text and buttons clipped). Both use `DateTimePicker` inside `DialogContent` (`SubscriptionDetail.tsx:1361`, `TestClockDetail.tsx:608`) | Let the DateTimePicker stack date over time below `sm` and use `min-w-0` / flex-wrap. Fix this once in `components/ui/datetime-picker`. | S |
| SUB-11 | Sub detail | friction | function | Change Qty 8→12 (`33-seats-change-qty-12.png`), Change Plan (`14-change-plan-now-checked.png`), Remove item (`17-remove-item-confirm.png`), Cancel immediately (`21-…`): rules are explained, amounts never are | Add a live "What changes" block to each dialog: "12 × $15 = $180/mo (+$60). Rest of period at new quantity, billed Nov 1." For in-advance plans, show "Charged now: $X". Stripe's update-subscription flow shows "Amount due today / next invoice". This likely needs a what-if preview endpoint (inferred: the only preview endpoints take an existing sub). | L |
| SUB-12 | Sub detail | confusing | function | Seats (calendar, period Oct 13–31): preview shows the full $120 (`slices/subscription-globex-seats__desktop-light__p1.png`). `preview.go:171-193` never prorates; the engine prorates stub base fees ("prorated N/M days", `engine.go:2856-2858`). **Inferred — verify which path applies to this plan.** | If they differ, either prorate stubs in the preview or emit a warning. Either way, show the period on the card (SUB-04). | M |
| SUB-13 | Sub detail | friction | interaction | Change Plan / Add Item list "Pro (EUR) — €45.00" for a USD sub (`13-change-plan-options.png`, `16-add-item-options.png`); the backend rejects it (`internal/subscription/service.go:481-511`). Filters at `SubscriptionDetail.tsx:1522-1526`, `:1629-1634` check interval and timing, not currency | Filter by currency and reuse the existing "N plans hidden because…" note. | S |
| SUB-14 | Sub detail | friction | interaction | Validation by toast, outside the dialog: `11-change-qty-same-value.png` "That is already the quantity"; `12-change-plan.png` shows a stray "Enter a whole number" toast. `SubscriptionDetail.tsx:1533-1536,1742-1749,1885-1908` | Inline `FormMessage` under the field, and disable Save while qty == current. Create Subscription already does this (`02-create-dialog-submit-empty.png`). | S |
| SUB-15 | Sub detail | confusing | hierarchy | globex-pro p1: status ×3 (header `:494`, summary `:787-791`, Properties `:962`); customer ×2; code ×2 (`:433`, `:946`); ID ×2 (`:431`, `:1026`); renew date ×3 (timeline, summary, Properties `:998`) | Remove the summary "Status" cell and the Properties Code / Customer / Status / Renews / Current period / ID rows. Keep one Details rail (wireframe C1). | M |
| SUB-16 | Sub detail | confusing | hierarchy | Order is Timeline → Summary → Items → Spend thresholds → Properties → Preview → Invoices → Activity, about 2,000px (`shots/subscription-globex-pro__desktop-light.png` 1440×2180). The money cards are below the fold. Spend thresholds (an empty, rarely used feature) takes a full card above them, even on canceled subs (`:893-937`; the button is hidden at `:902` but the card stays) | Reorder: header summary → Upcoming invoice + Items → Invoices → Activity, with Details + Thresholds in a right rail. Hide thresholds on terminal subs; render them as a one-line row when empty. | L |
| SUB-17 | Sub detail | friction | interaction | Trialing sub: Items "Actions" header over an empty column; no Add Item / Change plan during trial (initech p1; `:801`, `:870` gate on `status==='active'`) | If trial changes are allowed, enable them. If not, drop the column and say "Plan changes are available after the trial", or offer it through End trial. | S |
| SUB-18 | Sub detail | polish | function | ACME usage-only plan offers "Change Qty" (acme p1) | Hide Change Qty for plans with `base_amount_cents == 0`. | S |
| SUB-19 | Sub detail | friction | visual | Header carries 3–4 differently colored outline buttons plus 1–2 badges: amber Pause, red Cancel, purple End trial now, neutral Audit log / Extend trial (initech p1; `:437-497`). Each row has three bordered buttons (`:872-880`) | Status badge next to the title. One primary action, the rest in a "…" menu (Pause collection, Cancel, Audit log). Per row, a kebab menu. This is how Stripe and Linear do it. | M |
| SUB-20 | Sub detail | confusing | consistency | Button says "Pause" (`:456-458`); the dialog is "Pause collection" and the status stays "active". The cancel typed-confirm footer reads "Cancel" / "Cancel subscription" (`22-cancel-typed-confirm.png`) | Rename the button "Pause collection…". In the confirm, use "Keep subscription" / "Cancel subscription now". | S |
| SUB-21 | Sub detail | confusing | function | "No payment method — Renewals can't be charged" on a trialing sub (initech p1), where there is no renewal yet, only a first charge | When trialing: "The first charge of $49 on Nov 30 can't be collected until a card is added." | S |
| SUB-22 | Sub detail | friction | function | Activity shows only lifecycle audit rows ("Subscription created") even where invoices were issued (acme p2, globex-pro p2). Each row prints "Recorded … UTC · by Bootstrap Key (Test)" twice per event | Merge invoice issued/paid/failed and plan/qty changes into the feed, each with its amount. Demote "Recorded" to a hover. Stripe's subscription "Events" list is the model. | M |
| SUB-23 | Sub detail | friction | interaction | Invoices "View all" goes to unfiltered `/invoices` (`:1088`) | Link to `/invoices?subscription_id=…`. | S |
| SUB-24 | Sub detail | polish | visual | Trial summary wraps "Oct 16, 2026 — Oct 30, \n 2026" at text-lg (initech p1, `:735-737`) | Show "14-day trial · ends Oct 30" and put the range in muted text. | S |
| SUB-25 | Sub detail | polish | function | Billing alignment tooltip leaks API terms "immediate=false / immediate=true" (`24-billing-alignment-tooltip.png`, `:975-981`) | Replace with "Scheduled plan changes keep the renewal day; immediate interval changes restart the cycle on the change day." | S |
| SUB-26 | Create sub dialog | friction | function | `01-create-dialog-empty.png`: Plan is a native `<select>` (`CreateSubscriptionDialog.tsx:267`) while Change Plan uses a searchable Combobox. Billing cycle defaults to "Calendar" (`:69`). Over-limit Action is active while the cap is "Unlimited". There is no price or first-invoice summary | Reuse `planComboboxOptions` (price, interval, timing). Show a footer summary: "First invoice ≈ $X on {date} (prorated N days)". Disable Over-limit until a cap is set. | M |
| SUB-27 | Sub list | polish | visual | Last row ("Globex — Pro") has no colored status edge (`slices/subscriptions__desktop-light__p1.png`): `components/ui/table.tsx:36` `[&_tr:last-child]:border-0` also wipes `border-l` | Change to `[&_tr:last-child]:border-b-0`. | S |
| SUB-28 | Sub list | friction | function | No price/MRR column; "Code" (rarely needed) takes a full column; search covers name/code only, not customer (`Subscriptions.tsx:89-90`, `:303-308`) | Replace Code with "Price" ($49/mo + usage). Add search by customer. Stripe's Subscriptions list shows customer, product and amount. | M |
| SUB-29 | Sub detail | friction | mobile | Items table actions pushed off-screen (`54-mobile-items-real.png`; slice `subscription-globex-pro__mobile-light__p1.png` renders 578px wide) | A row kebab menu (SUB-19) fixes this. | S |
| SUB-30 | cross-cutting | friction | interaction | The floating "4 of 5 · Setup" pill sits over the Remove / Set thresholds buttons on every detail slice. A click on Set thresholds opened the Setup panel instead (`23-thresholds-dialog.png`) | Dock the pill into the sidebar footer, or reserve bottom padding on `<main>`. | S |
| CLK-01 | Clock detail | friction | function | `40-advance-default.png` / `41-advance-1mo.png`: only +1h / +1d / +1mo; no forecast of what the jump crosses | Add presets "Next renewal (Nov 1)", "+1w", "+1y". List what will happen: "Crosses: globex-seats renewal Nov 1 (est. $120), globex-pro renewal Nov 13 (est. $94); trial ends …". Use the subs' `next_billing_at` and preview. Stripe's test-clock "Advance time" shows upcoming events. | M |
| CLK-02 | Clock detail | friction | function | "Invoices generated 1" is plain text (`slices/clock-demo__desktop-light__p1.png`; `TestClockDetail.tsx:34-94`); only the last advance is kept | Link each count to the filtered list (invoices by clock/customer). Add an "Advance history" timeline (from → to, outputs, errors). | M |
| CLK-03 | Clock detail | confusing | consistency | Subs listed by code "globex-pro" with an outline neutral "active" badge; the list page shows "Globex — Pro" with a green badge (`TestClockDetail.tsx:384-385`, `:351`). Inferred: the endpoint returns an empty `display_name` | Use display name, `statusBadgeVariant`, and a row of "Plan · $/mo · Next bill (in N sim-days) · est. amount". Make it a table, not cards. | S |
| CLK-04 | Clock detail / list | friction | function | Clock time is shown without its relation to real time (demo is +32d; globex-history is 3 days *behind* today) | Show "Nov 17, 2026 10:36 UTC · 32 days ahead of real time", also on list cards. Add pinned customer and subscription counts to list cards. | S |
| CLK-05 | Clock detail | polish | consistency | "‹ Back to Test Clocks" link (`TestClockDetail.tsx:239-241`) instead of `DetailBreadcrumb`; two StatusBadge implementations (`TestClocks.tsx:40-53` uses `statusBadgeVariant`, `TestClockDetail.tsx:96-109` hardcodes `secondary`); "Test Clocks" vs "New test clock" vs "Add Subscription" casing | Use `DetailBreadcrumb` and one shared `TestClockStatusBadge`; pick sentence case. | S |
| CLK-06 | Clock list / detail | polish | mobile | `slices/test-clocks__mobile-light__p1.png` "globex-h…"; `slices/clock-globex-history__mobile-light__p1.png` sub names "glo…" collide with "Next bill" | Stack secondary text under the name below `sm`. | S |
| CLK-07 | Clock detail | polish | hierarchy | A full card for one line of clock time, then four similar cards stacked; a lot of empty canvas at 1440 | See wireframe C2. | M |

## C) Proposed anatomy

**C1. Subscription detail.** This moves the cost, the next invoice and the change history to the top, and folds the duplicated reference data into one rail. It follows Stripe's subscription page (summary header, upcoming invoice, rail) with Orb's usage block.
```
Subscriptions / Northwind — Pro
Northwind — Pro  [active] [Simulated · Nov 17 sim]            [Change plan] [ ⋯ ]
Northwind Traders · $49/mo + usage · Renews Nov 16 · Est. next invoice $143.00   (⋯ = Pause collection, Cancel, Extend trial, Audit log)
[one notice row: billing failure > no card > cancel scheduled > collection paused (+cause, $ outstanding)]
──────────────────────────────────────────────────────────────┬─────────────────────────
 Period  Oct 16 ━━━━━━━●━━━━━━━━━━━━ Nov 15   (day 1 of 31)   │ DETAILS
 UPCOMING INVOICE · Oct 16–Nov 15 · est. as of …     $143.00   │ Customer   Northwind ⧉
  Pro base fee            1 × $49.00              $49.00       │ Alignment  Anniversary ⓘ
  API calls          18,000 × $0.005              $90.00       │ Created    Oct 16, 2026
  Storage               140 GB                     $4.00       │ Code / ID  ⧉ ⧉
  ⚠ preview warnings (if any)                                  │ Usage cap  —
 ITEMS                                         [+ Add item]    │ SPEND THRESHOLDS
  Pro   $49/mo  qty 1   pending: → Starter Nov 16 [undo]  ⋯    │  None · [Set]
 Tabs: [Invoices (3)] [Activity] [Usage]                       │ PAYMENT
  VLX-000004 draft — $199.00 Oct 13 …   View all (filtered) →  │  No card · [Send setup link]
──────────────────────────────────────────────────────────────┴─────────────────────────
```

**C2. Test clock detail.** A test clock is a simulator, so this page answers "where am I in time, what will the next jump do, and what did the last ones produce". It follows Stripe's test-clock page with its advance history.
```
Test clocks / globex-history
globex-history [Ready]    Oct 13, 2026 10:37 UTC · 3 days behind real time   [Advance ▸] [ ⋯ Delete ]
──────────────────────────────────────────────────────────────────────────────────────────
 UPCOMING ON THIS CLOCK                            Quick advance: [→ Nov 1 renewal] [+1d] [+1w] [+1mo]
  Nov 1   globex-seats renews     8 × $15         est. $120.00
  Nov 13  globex-pro renews       $49 + usage     est. $94.00
 PINNED (1 customer · 2 subscriptions)
  Customer            Subscription        Plan            Status   Next bill        Est.
  Globex Corporation  Globex — Pro        Pro             active·collection paused  Nov 13  $94
                      Globex — Team seats Team seats × 8  active   Nov 1            $120
 ADVANCE HISTORY
  Sep 26 → Oct 13   1 invoice (VLX-000004 →)  0 errors
  Jul 13 → Sep 26   2 invoices, dunning paused collection  …
──────────────────────────────────────────────────────────────────────────────────────────
```

**C3. Subscriptions list (smaller change).** Columns: Customer · Subscription · Price (`$49/mo + usage`) · Status (with "collection paused" / "trial ends in 14d" chips) · Next bill (date + est. amount, or "Ended Oct 16"). Filters: All / Active / Trialing / Collection paused / Canceled / Billing failing, on their own row with Search. Cards on mobile.

## D) Patterns that should become shared components

- **Money-impact preview in change dialogs.** Quantity, plan, add/remove item and cancel-now should all render one "What changes: now / next invoice" block (SUB-11).
- **DateTimePicker inside dialogs.** It overflows in Extend trial and Advance today, and very likely in New test clock on mobile. Fix it once in `components/ui/datetime-picker`.
- **Form validation inside dialogs.** Use react-hook-form `FormMessage` everywhere. Several dialogs still `toast.error`.
- **Status display.** One `StatusBadge` driven by `statusBadgeVariant` (test clock pages use ad-hoc outline badges). One "secondary state" chip (collection paused, cancel scheduled, trial ends) used in both list rows and detail headers. Fix the TableBody `last-child` rule (SUB-27).
- **Detail page shell.** `DetailBreadcrumb` + title/status + one primary action + overflow menu + a right "Details" rail. Subscription detail and test clock detail diverge from it today.
- **Date conventions.** Inclusive period end vs exclusive renewal boundary is handled per call site (`formatDate` vs `formatCivilDate`). A helper `periodLabels(sub)` that returns {accessThrough, renewsOn, cancelsOn} would prevent the Nov 15/Nov 16 split and the canceled "Renews" bug.
- **Floating Setup pill** covers action buttons on every page (SUB-30).

## E) Keep these

- **Cancel dialog:** radio cards that default to the safe option, trial-aware copy ("the customer won't be charged"), and the typed `CANCEL` confirm for immediate cancel (`20-`, `21-`, `31-…png`).
- **Pause collection dialog:** the "drafts stay drafts after resume; finalize to collect" wording, with optional auto-resume and a collapsible Details.
- **Delete test clock dialog:** it names the blast radius ("1 customer and their 2 subscriptions … real data is never touched").
- **Advance dialog basics:** it names who moves ("1 customer (2 subscriptions)"), enforces the 1-year cap inline, and reserves a fixed slot for the warning so the buttons don't jump.
- **Timezone handling:** the simulated-time banner (`TestClockBanner`) on clock-pinned subs, timestamps that follow the clock, and pickers that read in tenant timezone with the timezone named.
- **Change Plan / Add Item filtering:** they hide incompatible plans and say how many were hidden and why. Extend that to currency (SUB-13).
- **Notice ranking:** at most one box, with the rest as one-line rows (`subscriptionNoticeLayout`). The ordering logic is sound.
- **List mechanics:** server-side search and sort, URL-held filter state, and the "Simulated" row chip.
