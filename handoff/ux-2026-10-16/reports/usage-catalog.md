I've audited all seven pages in my area. The most serious problem: the usage page reports numbers that don't match how they're billed. In short, an operator can't set up pricing without docs today. The catalog never shows how plans, meters and prices connect. Prices are made in one place and linked to meters in another. A plan's "locked" banner contradicts three meter and price actions that still change what that plan bills.

I created and submitted no records. Every dialog was opened and then cancelled, and the grant confirm was cancelled too. The `ux-calls` meter and the extra plan, meter and two prices seen in later shots were made by other sessions.

Two corrections to the shared evidence:
- The mobile shots in `/tmp/claude-0/ux/shots` (621–866px wide) are an artifact of the injected style. Measured at a real 390px, no page in my area overflows horizontally except the Credits customer view (432px).
- Prices (97) became Prices (99) during my run; other sessions were creating records.

My interactive shots are in `/tmp/claude-0/ux/work-usage-catalog/` (script: `shoot.cjs`). Below, `work/` means that folder, `slices/` means `/tmp/claude-0/ux/slices/`, and code paths are relative to `/home/user/velox/web-v2/src/` unless they start with `internal/`.

## A) Verdict per page

- **Usage Events:** The filters, range caption, URL state and the two empty states (nothing yet / nothing matches) are solid. The numbers are not: "Total Units" adds tokens, calls and GB together, and the Storage meter (billed on its maximum) is shown summed (550 GB). The page also can't answer the AI operator's main question, which is usage broken down by model or token type. The event log is a long list of chips with no way to open a single event.
- **Pricing (catalog):** Three flat tabs with no visible link between them. Prices are not clickable and not grouped (99 rows, 95 of them from the recipe). Plans show only a meter count. Create dialogs report errors one at a time in a toast. There is no currency picker and no tier calculator.
- **Plan detail:** The lock banner is a good idea, but the page contradicts itself ("3 subscriptions use this plan" next to "Active Subscriptions 2"). The banner's action ("Go to plans") leads nowhere useful. Facts are repeated up to three times. The seat plan never says "per seat".
- **Meter detail:** Fine for a single flat price. For the tokens meter it is a 95-row table, about 5,000px tall, where every priority is 100. It shows no usage, no sample events and no way to edit the meter.
- **Recipes:** Strong idea, with a real preview. But the dialog's buttons scroll out of view, and the preview renders below the fold. An installed recipe links to nothing it created. It also quietly creates a payment-retry policy and a webhook pointing at example.com.
- **Provider costs:** Works, but costs are shown as "$ per token" (0.0000012) while prices are shown per 1M tokens, so margin can't be read by eye. Deleting a rate takes one click with no confirm. Token type is free text.
- **Credits:** Stark shows a bare $0.00. The API already returns $500 granted and $500 used, but the page doesn't show either. The customer view is a hidden mode of the same page, and on mobile its Grant button is cut off.

## B) Findings

| id | page | severity | category | evidence | recommendation | size |
|---|---|---|---|---|---|---|
| UC1 | Usage | broken | function | `work/usage-storage-all.png`: Storage (a Maximum meter) shows **550 GB** = 140+210+120+80, across customers and months. `internal/usage/postgres.go:346` always uses `SUM(quantity)`; `UsageEvents.tsx:198-210` | Aggregate each meter with its own mode (Maximum shows peak per customer and period), or label it "sum of reported values" and hide totals for non-sum meters. The page must not contradict billing. | M |
| UC2 | Usage | confusing | function | `slices/usage__desktop-light__p1`: "Total Units 840,140" adds tokens + calls + GB; the bars put different units on one scale. `UsageEvents.tsx:278, 209` | Drop "Total Units". Replace the bars with one row per meter: total in its unit, a sparkline, and change versus the previous window (Metronome/Orb billable-metric list). | S |
| UC3 | Usage | friction | interaction | `work/usage-dim-malformed.png`: typing "model" (on the way to "model=…") swaps the whole page for "Couldn't load usage events" with Retry. `UsageEvents.tsx:358-364` queries on every keystroke | Wait for typing to pause and check the input inline. Better: a key/value picker filled from dimension keys already seen (PostHog property filter). | S |
| UC4 | Usage | confusing | hierarchy | `work/usage-meter-tokens.png`: with one meter selected, "Usage by Meter" is a single 100% bar. There is no breakdown by model or token_type. | Add a "Group by" control on dimension keys, with a stacked bar over time and a table by model and token_type (Orb invoice-preview grouping, PostHog breakdown). | M |
| UC5 | Usage | friction | interaction | `slices/usage__desktop-light__p1-p2`: 4 stacked chips per row, rows can't be clicked, no event id or idempotency key, timestamps lack seconds | Make rows open a side sheet with the raw payload, id and cost stamp (Stripe Events / webhook log). Promote model and token_type to columns. | M |
| UC6 | Usage | confusing | function | `work/usage-events.csv`: timestamps like "Oct 16, 2026, 10:37 AM UTC" (no seconds, not ISO), no event id or customer external_id. The browser walks pages up to a cap; the code comment at `UsageEvents.tsx:216-228` says `/v1/exports/usage-events.csv` exists | Point the button at the server export; use ISO-8601 and include the ids. | S |
| UC7 | Usage | polish | visual | `work/usage-customer-combobox.png`: the dropdown is as wide as its trigger (208px), so entries truncate ("Wayne Enterprise…", "Hooli — pay@hoo…") | Make the dropdown at least 320px wide, with name and email on two lines. | S |
| UC8 | Usage, Credits | polish | consistency | Native `<select>` at `UsageEvents.tsx:319,329` and `Credits.tsx:310`, next to the shadcn Combobox (different chevron and height, visible in every Usage shot) | Use the shared Select component. | S |
| UC9 | Usage | polish | mobile | `slices/usage__mobile-light__p1`: four stat cards stack to about 450px before any data; the Setup pill sits on top of them | Use a compact two-by-two strip of key numbers on small screens. | S |
| UC10 | Pricing | confusing | hierarchy | `slices/pricing__desktop-light__p1`, `work/pricing-meters.png`, `work/pricing-prices.png`: nothing shows which plans use a meter or which price feeds which meter. "Used by 1 meter" is not a link and price rows can't be clicked (`Pricing.tsx:381-420`) | Restructure the catalog (see C1): plans list their charges, meters show "used by", prices link to their meter. | L |
| UC11 | Pricing | friction | hierarchy | `work/pricing-prices.png` and `pricing-prices-full.png`: 99 flat rows, 95 from the recipe, no grouping or paging | Group by meter (collapsible), with filter chips for meter, mode and "not linked" (Stripe product catalog groups prices under a product). | M |
| UC12 | Pricing | polish | interaction | `work/pricing-prices-search-empty.png`: a search with no matches shows only the column headers | Add a "No prices match 'zzz' · Clear" row. `Pricing.tsx:376` | S |
| UC13 | Pricing, Plan, Meter, Recipes | confusing | consistency | Identifiers are "Key" (meter, price), "Code" (plan) and "Plan ID" (recipe, `Recipes.tsx:326`). Graduated pricing shows as "tiered" (meter list) and "graduated" / "Graduated Tiers" (badge, dialog). Aggregation shows as "Latest Value" (`Pricing.tsx:75`), "Latest value" (`meterLabels.ts`), "Last value in period" (`MeterDetail.tsx:53`), and raw "sum"/"max" (`Pricing.tsx:955`, `PlanDetail.tsx:468`, `work/dlg-plan-attach-meter.png`) | Write one glossary (meter key, price key, plan code) and route every aggregation label through `meterAggregationLabel`. | S |
| UC14 | Pricing | friction | interaction / a11y | `work/dlg-create-plan-empty-submit.png`, `dlg-create-meter-empty-submit.png`: an empty form submits to the server and gets one toast ("Code is required") although Name is empty too; no field is marked. Labels have no `htmlFor` (`Pricing.tsx:767,772,781,889,901,921`). Edit Plan and Grant Credits already show errors inline | Use the same form-and-schema setup as `EditPlanDialog` and Grant Credits: errors under each field, all at once. | M |
| UC15 | Pricing | confusing | interaction | `work/dlg-create-price-package.png`: "Tier 2: 'Up to' must be greater than 100" still shows after switching to Package; a flat-mode error also stays when the mode menu opens (`dlg-create-price-mode-menu.png`). `setMode` never clears `formError` (`Pricing.tsx:538,627`) | Clear the error when the pricing model changes, and show errors next to the field. | S |
| UC16 | Pricing | confusing | function | `Pricing.tsx:543` says graduated means "Price decreases as usage increases", but the seeded Storage price goes from $0.00 to $0.10 (`slices/meter-storage_gb__desktop-light__p1`). The tier editor has no worked example | Correct the copy ("Each tier's rate applies to units within that tier"). Add a live "Usage of N units costs $X" calculator (Stripe's price preview). | S / M |
| UC17 | Pricing | confusing | function | `work/dlg-create-plan.png`: no currency field. The "Base Price ($)" label uses the workspace symbol even when currency is taken from the chosen meters' prices (`Pricing.tsx:859-866,915`). The meter checklist shows raw "max · GB" and no price status, so a meter with no price can be attached unknowingly | Add a currency select (prefilled from the meters, locked when they disagree). Show "Priced / Not priced" on each meter row. | M |
| UC18 | Pricing, Meter | confusing | function | Pricing a meter takes three steps: create a price on the Prices tab, open the meter, then link it. "Add a price for specific values" can only pick an existing price (`work/dlg-meter-add-dim-price-agg.png`; `MeterDetail.tsx:742`) | Offer "Create new price" inside the price picker, opening the price form inline (Orb/Lago define prices inside the plan editor; Stripe inside the product). | M |
| UC19 | Pricing | confusing | function | `work/pricing-new-version-dialog.png`: "Publish v2" doesn't say how many meters, plans and live subscriptions it reprices. It also allows switching the pricing model | Add a "This reprices N meters · M plans · K live subscriptions from their next period" line. Lock the pricing model unless deliberately changed. | M |
| UC20 | Plan | confusing | function | `slices/plan-pro__desktop-light__p1`: "3 subscriptions use this plan" (server lock count) next to "Active Subscriptions 2" (counts only `status==='active'`, so the trialing one is missing). `PlanDetail.tsx:148-150` vs `155` | Show one number, the live count, with a status breakdown ("2 active · 1 trialing"). | S |
| UC21 | Plan | friction | function | The lock banner's action is "Go to plans" (`PlanDetail.tsx:248`), which sends the operator to a blank create form to retype everything | Add a "Duplicate as new plan" action that prefills the base price, timing and meters (Stripe "Duplicate product"; Orb plan versions). | M |
| UC22 | Plan, Meter, Pricing | confusing | function | The plan says "price, billing timing and meters are locked", yet the meter page's "Change" default price, the per-price trash buttons and "New version" all change what that locked plan bills (`MeterDetail.tsx:239,371`; `Pricing.tsx:412`; `work/dlg-meter-remove-dim-price.png`) | Reword the lock to "base price, timing and meter list". Put the live-subscription impact count on every meter or price change. | M |
| UC23 | Plan | friction | hierarchy | `slices/plan-pro__desktop-light__p1`: status appears 3 times, code 3 times, ID twice; interval and currency appear in both the summary strip and Properties | Use a summary header plus a right rail of properties (Stripe product page) and remove the separate Properties card. | M |
| UC24 | Plan | confusing | function | `slices/plan-seats__desktop-light__p1`: "Base Price $15.00" and "this plan charges only its base price", while Globex holds quantity 8 (checked via the API). The subscriptions table has no Quantity column | Write "$15.00 per seat / month", add Quantity and an MRR column per subscription. | M |
| UC25 | Plan | polish | visual | `slices/plan-pro__desktop-light__p1`: the meter price pill is truncated and hidden under the floating Setup pill. "Detach" stays red even when disabled (`PlanDetail.tsx:359-366`). Attach Meter is one click with no price shown (`work/dlg-plan-attach-meter.png`) | Make Detach a ghost item in a ⋯ menu; show the price status in the attach list. | S |
| UC26 | Plan | friction | function | Inferred from code: `PlanDetail.tsx:94` fetches unfiltered `listSubscriptions()` and filters on the client, so a plan whose subscriptions are past the first page would undercount | Use the server filter `plan_id=`. | S |
| UC27 | Meter | confusing | hierarchy | `slices/meter-tokens__desktop-light__p1-p4`: 95 rows, all priority 100 (the column says nothing), each row repeating the model chip; "Used by 1 plan" sits about 5,000px down | Show a pivot table: models as rows, token types as columns, price per 1M in each cell, plus search and a "raw rules" toggle (see C2). | L |
| UC28 | Meter | friction | function | `slices/meter-api_calls__desktop-light__p1`: no usage chart, no recent events, no "send an event" snippet, and no Edit (the API has `updateMeter`) | Add an overview: 30-day volume, top customers, last 5 events, a curl snippet with the key (Lago and Orb billable-metric pages), plus an Edit name/unit action. | M |
| UC29 | Meter | polish | consistency | `work/dlg-meter-add-dim-price-agg.png`: aggregation options in monospace (`MeterDetail.tsx:717`); 5 modes here versus 4 in Create Meter; the header "Sum" badge repeats the property row | Use one label set and drop the header badge. | S |
| UC30 | Meter, Pricing | friction | mobile | `work/mobile-meter-tokens-table.png`, `work/mobile-pricing-prices.png`: the Price column (the one that matters) is off-screen to the right; priority or key come first | On small screens, show each rule as a card with the price first. | S |
| UC31 | Recipes | friction | interaction | `work/dlg-recipe-configure.png`: the action buttons are cut off at the dialog's bottom edge. After clicking Preview (`dlg-recipe-preview.png`), nothing visibly changes; the preview is below the fold (`Recipes.tsx:229`) | Pin the footer and scroll to the preview, or use a two-column layout: settings on the left, live preview on the right. | M |
| UC32 | Recipes | friction | function | `work/dlg-recipe-installed-view.png`: an installed recipe says "edit the plan on its page and the prices on the Pricing page" with no links | List what it created (plan, meter, 95 prices) with links. | S |
| UC33 | Recipes | confusing | function | `work/dlg-recipe-preview-bottom.png`: installing also creates "AI default retry — up to 4 retries" and a webhook to `https://example.com/webhooks/velox`; the card calls these "1 retry policy · 1 notification endpoint" | Make each extra a checkbox, off for the webhook unless a URL is entered. | S |
| UC34 | Recipes | polish | visual | `slices/recipes__desktop-light__p1`: the card title line is a snake_case key; there are two purple "Configure" buttons; Currency is a free-text box | Use the name as the title and the key as small muted text; make secondary buttons outline style; make Currency a select. | S |
| UC35 | Provider costs | friction | interaction | Delete is a single click with no confirm (`ProviderCosts.tsx:177-178`) | Add a confirm or an undo toast. | S |
| UC36 | Provider costs | confusing | function | `slices/provider-costs__desktop-light__p1` shows "$ / token 0.0000012"; `slices/meter-tokens` shows "$3.00 / 1M tokens". Only claude-sonnet-4.5 has costs although about 19 models are priced; the helper lists only input/output/cache_read, while prices also use cache_write_5m and cache_write_1h | Show per-1M and add a coverage view ("models with prices but no cost: 18"). Best: put a cost column in the meter's price table for margin at a glance (Metronome). | M |
| UC37 | Provider costs | polish | consistency | `components/Layout.tsx:42,45`: Pricing and Provider costs use the same Tag icon; Dunning and Dunning policies share one too (lines 47-48) | Give each its own icon. | S |
| UC38 | Credits | confusing | hierarchy | `slices/credits__desktop-light__p1`: Stark $0.00 with no context. `/v1/credits/balances` returns `total_granted 50000, total_used 50000`, which the table drops (`Credits.tsx:537-576`) | Add Granted / Used / Expired / Remaining / Next expiry columns (Orb and Metronome credit ledgers). | S |
| UC39 | Credits | confusing | function | Inferred from code: `formatCents()` without a currency (`Credits.tsx:282,415,417,570,844`) and `getCurrencySymbol()` (`:751`) use the workspace default, while `CustomerDetail.tsx:259-265` uses the customer's billing currency. A EUR customer like Wayne would see "$" here and "€" on their customer page | Pass the customer's currency into this page. | M |
| UC40 | Credits | polish | interaction | `work/credits-stark-filter-empty.png`: "No transactions of type 'adjustment'" (raw lowercase value) followed by "Grant credits to get started" for a customer with history; CSV disappears | Say "No adjustments for Stark · Show all types". | S |
| UC41 | Credits | friction | mobile | `work/mobile-credits-stark.png`: Grant is cut off at the right edge (main scrollWidth 432 > 390). The customer view is a `?customer=` mode with only a back arrow, no breadcrumb | Wrap the header actions, use `DetailBreadcrumb`, or move it to `/credits/:customerId`. | S |
| UC42 | Credits | polish | visual | `work/credits-stark-ledger.png`: the solid purple "Grant" badge looks like a button. A fully used grant still says "Expires Dec 15, 2026". Amounts are monospace while the header balance is not | Use a soft success badge and "Used $500 of $500"; one money font. | S |
| UC43 | Credits | polish | interaction | `Credits.tsx:697-699`: "Select a customer" appears only after amount and description pass (`work/dlg-credits-grant-empty-submit.png` shows the amount and description errors without the customer one) | Put the customer check in the same validation pass. | S |
| UC44 | all | polish | visual | The floating "4 of 5 · Setup" pill covers the meter price pill (`slices/plan-pro__desktop-light__p1`), the trash icon in row 3 (`slices/meter-tokens__desktop-light__p1`) and mobile stat cards | Reserve bottom padding in `main`, or dock the pill in the sidebar. | S |

## C) Proposed anatomy

**C1. Pricing catalog** (replaces three flat tabs). Model: Stripe product catalog, with Orb plan → price structure.
```
Pricing                                   [Use a recipe] [+ New ▾ plan|meter|price]
How billing works:  Event ─▶ Meter (counts) ─▶ Price (rates) ─▶ Plan (sells)   [dismiss]
┌ Plans 7 │ Meters 4 │ Prices 99 ┐  [search all]  [Currency ▾] [Status ▾]
├───────────────────────────────────────────────────────────────────────────────────┤
│ PLAN            CHARGES                                 SUBS   MRR      STATUS      │
│ Pro  pro        $49/mo base · API calls $0.005/call ·   3      $98      active 🔒  │
│                 Storage tiered/GB                                                  │
│ Team seats      $15/seat/mo                              1      $120     active 🔒  │
│ AI API          95 token rates (Tokens meter) ⚠ no def.  1      —        active 🔒  │
├─ Meters tab: NAME · KEY · AGG · PRICED BY · USED BY (plan chips) · 30d VOLUME ─────┤
├─ Prices tab: grouped by meter ▸ Tokens (95)  ▸ API calls (1)  ▸ Not linked (2) ────┤
```
Why: each row shows the whole chain and its live footprint (subscriptions, MRR, lock), so plans, meters and prices make sense without docs.

**C2. Meter detail.** Model: Metronome billable metric plus Orb price matrix.
```
← Meters / Tokens                              [Send test event] [Edit] [⋯]
tokens · Sum · unit: tokens · Used by: AI API (1 live sub)  · Default price: none ⚠
┌ Overview │ Pricing │ Events │ Settings ┐
Overview: [30d volume chart, group by model ▾]  Top customers  Last 5 events  curl snippet
Pricing:  Default price [Link / Create]   — shows "affects 1 live subscription"
          ┌ model ▾ \ token_type → input   output  cache_read  cw_5m   cw_1h   cost/margin ┐
          │ claude-opus-4.5         $5.00   $25.00   $0.50     $6.25   $10.00   —          │
          │ claude-sonnet-4.5       $3.00   $15.00   $0.30     $3.75   $6.00    63% ●      │
          └ per 1M tokens · click a cell to edit/publish v2 · [Raw rules (95)] [+ Add model] ┘
Events:   filtered Usage Events table (meter preselected)
```
Why: 95 rows collapse into a 19×5 table that fits on one screen. Price, cost and margin sit side by side, and changes that affect live subscriptions say so.

**C3. Plan detail.** Model: Stripe product page (header, right rail, tabbed body).
```
← Plans / Pro   active · 🔒 in use by 3 (2 active, 1 trialing)   [Duplicate as new plan] [Edit name/status]
$49.00 / month base (billed at end of period) + usage
┌ main ──────────────────────────────────────────────┐ ┌ rail ────────────────┐
│ Charges                                             │ │ Code   pro      ⧉    │
│  Base fee         $49.00 / month                    │ │ ID     vlx_pln… ⧉    │
│  API calls        $0.005 / call       [meter →]     │ │ Currency USD         │
│  Storage (max GB) first 100 free, then $0.10 [→]    │ │ Interval Monthly     │
│  [Estimate a bill: 10k calls, 250 GB → $64.00]      │ │ Created Oct 16       │
│ Subscriptions (3)  customer · status · qty · period │ └──────────────────────┘
└─────────────────────────────────────────────────────┘
```
Why: "what does this plan charge and who is on it" comes first. Each fact appears once. The lock leads to a working next step (Duplicate).

## D) Cross-cutting patterns
- **One form standard:** schema-based validation with inline per-field errors and `htmlFor` labels. Today only Edit Plan and Grant Credits do this; the Create Plan, Meter and Price dialogs use toasts (UC14, UC15).
- **One vocabulary module:** aggregation labels, pricing model names and identifier names, all routed through `lib/meterLabels.ts` and a new pricing-terms module (UC13).
- **An impact line for changes to live pricing:** "Affects N meters · M plans · K live subscriptions from next period", reused by New version, Change default price, Remove price and Detach (UC19, UC22).
- **Money and units:** one money font (not monospace in some places), always pass the record's currency, and quote token rates per 1M everywhere, including provider costs (UC36, UC39, UC42).
- **Dialog shell:** a pinned footer and a scrolling body. The existing eslint-disable comment at `Recipes.tsx:228` already flags this as tracked (UC31).
- **Shared Select / Combobox:** stop using native `<select>`; size the dropdown to its content (UC7, UC8).
- **Detail pages:** always use `DetailBreadcrumb`, a summary header and a right rail, never a hidden "mode" of a list page (UC23, UC41).
- **No-match state for client-side search** in every searchable table (UC12).

## E) Keep these
- The usage range caption ("Totals for the last 30 days"), filters kept in the URL, and the separate "none yet" and "no match" empty states with Clear filters.
- The plan lock concept and its plain-language sentence (it needs a better action and a consistent scope, not removal).
- The amber "won't be billed" warnings on meters with no price.
- The typed-confirm on removing a price, whose copy states the consequence (`work/dlg-meter-remove-dim-price.png`).
- Per-1M token price display (`priceRate` / `unitScale`).
- "Simulated" badges on test-clock rows.
- Recipe preview listing every object before install; the read-only view of an installed recipe.
- Grants: a confirm step, an idempotency key per dialog open, an expiry floor that follows a test clock's time, server-side ledger paging, a ledger row linking to its invoice, and the promotional-credit explanation.
- Plan edit blocks a base price of $0 with an inline reason; "No changes" disables Save.
- Dark mode reads well on every page I checked; there were no contrast failures in my area.
