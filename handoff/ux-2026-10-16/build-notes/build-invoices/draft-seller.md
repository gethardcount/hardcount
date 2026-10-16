## What & why

First PR of the Invoices & Collections chain (base `feat/app-shell`). Resolves **M-INV-1 (merged-b)**, **M-INV-6 (merged-a)**, **M-X-13** and the memo half of **M-INV-9 / INV-15**.

- **Drafts no longer look issued.** The PDF of a draft carries a DRAFT watermark, prints "Status: Draft — not issued" instead of Issued / Due Date, and its footer says nothing is owed yet. The invoice page shows the same: "Draft — not issued", a faint DRAFT mark, and for a composed draft Due "—" with "Net 30 from finalizing" (a cycle draft keeps the due date it was built with, because finalize keeps it).
- **No made-up seller.** The invoice PDF fell back to "Velox", the credit-note PDF to "Velox", the invoice page to "VELOX" and "Your Company". All four fallbacks are gone. With no company name, nothing is printed in the seller slot (address and contact lines still print when set; before, they were dropped whenever the name was empty). The invoice page shows a **Missing seller details → Settings** chip and "No business name set. Add it in Settings…" in the From block.
- **Memo** now shows on the invoice page under a Memo label (it was only on the PDF and hosted page).

**Rule picked for seller details at Finalize: warn, never block.** Cycle invoices are issued by the billing engine with no operator step, so a block could only apply to composed drafts and would make the rule inconsistent. The warning is shown on the detail page here, and in the Finalize confirmation added later in this chain (`ux/invoices-draft-editing`).

Not changed (noted for the maintainer): the invoice email template still falls back to "Velox Billing" as the sender name (`internal/email/templates.go:31`).

## Before / after

| | Before (main, :5173) | After (this branch, :5183) |
|---|---|---|
| Draft VLX-000007 page | "VELOX" heading, From "Your Company", Issued Oct 16, Due Nov 15, no memo | No seller heading, warning chip + From hint linking to Settings, "Draft — not issued", Due "—" / "Net 30 from finalizing", faint DRAFT mark, Memo "Draft add-on" |
| Draft VLX-000007 PDF | "Velox" seller, Issued + Due Date rows | No seller name, DRAFT watermark, "Status Draft — not issued", draft footer |
| Issued VLX-000002 (empty seller copy) | "VELOX" / "Your Company" | Warning chip + From hint |

## Evidence (screenshots looked at)

`/tmp/claude-0/ux/build-invoices/draft-seller/`: `before-*` and `after-*` for VLX-000007 and VLX-000002 at 1440 light, dark and 390px; `pdf-8080-1.png` (main) and `pdf-8183-1.png` (this branch) renders of the draft PDF.
- After, light/dark: chip reads "Missing seller details → Settings" in warning tokens, readable in both themes; DRAFT mark faint behind the document, not over text contrast.
- After, 390px VLX-000002: From hint wraps inside the half-width column; chip sits beside the Open badge.
- PDF after: DRAFT watermark mid-page, no seller name, "Status Draft — not issued", memo under the draft footer.

## Tests and mutation checks

- Go `internal/invoice/pdf_draft_seller_test.go`: `TestIssueDateRows_DraftIsNotIssued`, `TestWatermarkFor`, `TestSellerName_NeverAPlaceholder`, `TestRenderPDF_DraftWithoutSeller`. Mutation (draft branch removed, DRAFT case removed, "Velox" fallback restored): all three red — `draft rows = [[Issued October 16, 2026] [Due Date November 15, 2026]]`, `watermarkFor(draft) = "", want "DRAFT"`, `no company: got "Velox", want empty`.
- Web `tests/invoiceDocument.test.ts` (6 tests). Mutation (draft branch disabled, `sellerMissing` → false): 4 of 6 red ("a composed draft is not issued…", "an issued invoice uses its own seller copy…").

## Checks

gofmt clean · `go vet ./...` ok · golangci-lint `./internal/invoice/... ./internal/creditnote/...` 0 issues · `make lint-clock lint-tz lint-funding-set lint-manual-test` ok · `go test ./... -short -count=1` ok (without `-race`) · integration `flock … go test -p 1 -short=false ./internal/invoice/... ./internal/creditnote/...` ok · `npx tsc -b` ok · eslint 0 errors (1 pre-existing warning in InvoiceDetail.tsx) · `npm test` 248/248 · `npm run build` ok.

## Checklist

- [x] Tests cover the new behavior
- [x] `go test ./... -short -count=1` passes
- [x] golangci-lint passes on touched packages
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow I6 updated
- [x] Frontend: build, test, lint pass

## Money path only

Display only: no state, writer or effect changes. `RenderPDF` reads `inv.Status` to choose rows and the watermark; it writes nothing.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
