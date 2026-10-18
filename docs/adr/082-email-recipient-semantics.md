# ADR-082: Email recipient semantics — additional_emails + CC coverage matrix

Date: 2026-07-06
Status: Accepted. Amended 2026-07-19 by [ADR-098](098-postmark-delivery-webhook-ingestion.md). Amended 2026-10-18: [addresses must be bare](#amendment-2026-10-18-every-address-must-be-one-bare-address).
Design basis: docs/design/cc-emails.md (two-lens divergent panel +
adversarial judge; plumbing claims verified against the code before
acceptance).
Register items: "Invoice/receipt/dunning emails go to exactly one
recipient" (fix-now) + the promoted `POST /v1/credit-notes/{id}/send`
hours-slice.

## Summary

A customer can hold up to 10 extra email addresses (`additional_emails`, encrypted like the main address), so billing emails reach more than one contact. They go in a visible `Cc:` on one message, for six email types: invoice, payment receipt, payment failed, both dunning emails and credit note, now sent with `POST /v1/credit-notes/{id}/send`. The two payment-setup emails, password reset and member invite are never copied, because each carries a personal link; their send functions take no CC list. A bounce on a copied address never marks the main address as bounced, and a suppressed main address blocks the whole send. Status: active, amended 2026-07-19: [ADR-098](098-postmark-delivery-webhook-ingestion.md) adds Postmark delivery, bounce and spam-complaint reports. Amended 2026-10-18: every address, main or copied, must be one bare address such as `ap@example.com`. A display name, quotes or angle brackets are refused with 422 instead of being stripped, and the sender checks each address again before it sends.

## Context

Every transactional email targeted the single `customers.email`; the
operator send dialog took one address. The DP's customer wants
ap@customer.com AND their engineering lead copied — and the workaround
(make the primary a distribution alias) isn't in the DP's control for
THEIR customers. Peers: Orb CCs the customer's additional_emails on
every invoice email; Lago supports multi-recipient + resend override;
Stripe supports additional/CC recipients on Billing emails. Credit
notes additionally had NO send surface at all — engine-auto-issued
clawback CNs and card-refund CNs moved real money with no document
reaching the customer.

## Decision

1. **Storage**: `customers.additional_emails` (migration 0140) holds
   the same-encryptor ciphertext of a JSON string array — a plaintext
   TEXT[] would dump-leak exactly the PII the sibling `email` column
   encrypts, and no DB CHECK can inspect ciphertext, so the cap lives
   in the service. Normalization (shared
   `domain.NormalizeAdditionalEmails`, used identically by the customer
   service and both send overrides): trim → validate → **bare
   lowercased addr-spec** (ParseAddress accepts display-name forms;
   storing raw input would let CRLF material reach the Cc header
   builder) → case-insensitive dedupe → never the primary → cap 10.

2. **Transport**: ONE MIME message per send — primary in `To:`,
   additional in a **visible `Cc:`** (peer semantics), one SMTP
   transaction with RCPT per recipient. Rejected: N separate envelopes
   (one outbox row with N retry states = duplicate sends on retry) and
   BCC (no peer blind-copies invoices).

3. **Coverage matrix** — CC-eligible (6): invoice, payment_receipt,
   dunning_warning, dunning_escalation, payment_failed, credit_note.
   **Never-CC (4)**: payment_setup_request, payment_setup_link (both
   carry single-use tokenized payment-credential URLs — CC = credential
   spread), password_reset, member_invite (personal account
   credentials). The exclusion is structural: those four signatures
   have no cc parameter to misuse. This split is permanent unless the
   credential model itself changes.

4. **Per-recipient failure policy** (the adversarial finding): sendRich
   attributes any `deliver` error's bounce to the PRIMARY recipient, so
   deliver handles CC failures internally — a CC RCPT *protocol*
   rejection is WARN + bounce-reported **for the CC address** + dropped
   (send proceeds; failing the outbox row would re-deliver to the
   primary on retry); a CC *transport* error aborts the whole send
   (DATA not yet sent — retry-safe). A leaked CC 550 would have flipped
   the primary customer to bounced and suppression-gated their real
   invoices.

5. **Suppression**: primary suppressed → whole send blocked loudly at
   both gates (enqueue + dispatch), never downgraded to CC-only. CC
   filtering at dispatch time only (a bounce recorded between enqueue
   and send is honored), per-address, fail-open, logged. A CC bounce
   flips `email_status` only when that address is some customer's
   primary (blind-index resolution); a never-a-customer alias is
   log-only in v1 — no per-CC persistent suppression state.

6. **Operator override** (both send endpoints): body
   `{email, additional_emails *[]string}` — absent → the customer's
   stored list (legacy `{email}` bodies now CC by default — the
   Orb-parity behavior change), `[]` → primary only, list → validated
   exact override.

7. **CN send rider**: `POST /v1/credit-notes/{id}/send`, issued-only,
   shares the download path's PDF assembly (extracted so the emailed
   document can never diverge from the downloaded one), new
   `credit_note` outbox type (no email_type CHECK — append-only), new
   template (no CTA — no hosted CN page exists; the PDF is the
   document), audit `send` on resource credit_note (no recipient
   address — GDPR convention), NO outbound webhook. The `credit_note`
   type is added to the ListByInvoice/ListByCustomer allowlists so the
   send surfaces on the applied invoice's timeline and the customer's
   Sent-emails section.

## Consequences

- Binary skew is degraded-but-correct in both directions: an old
  dispatcher reading a new row ignores the `cc` key (primary-only
  delivery); a new dispatcher reading an old row gets nil cc.
- Multi-membership login determinism, bounce attribution, and the
  suppression gates are all pinned by tests (cc_transport_test.go,
  outbox round-trips, customer store round-trip, tri-state handler
  tests).

## Still cut (named triggers)

customer_email_contacts table (per-contact names/routing/deliverability
— phase 3, trigger: DP asks for per-recipient preferences); per-CC
persistent suppression / auto-removal (trigger: repeated hard-bounces
on a CC alias in WARN logs); per-email-type routing; email-sent webhook
events + the credit_note.* lifecycle family (design whole when a DP
automation asks) — **partially un-cut by ADR-098 (2026-07-19)**: inbound
provider delivery/bounce/complaint ingestion now exists (Postmark), with
per-cause customer.email_bounced/email_complained outbound events;
outbound email-*sent* lifecycle events remain cut; hosted CN page;
auto-email at finalize (separate register item); BCC / Reply-To /
operator self-copy.

## Amendment 2026-10-18: every address must be one bare address

Decision 1 said: trim, validate, then keep the bare address that `ParseAddress` returns, so `Bob <bob@x.com>` was stored as `bob@x.com`. That step is replaced. An address is now refused with 422 unless, trimmed, it is exactly the address `net/mail` parses from it. So a display name, angle brackets, a comment, a quoted local part or a second address are refused, not stripped.

Why. `ParseAddress` returns a quoted local part unquoted, so the stored address was not the one typed, and the SMTP envelope could name a recipient the `Cc:` header did not show. Refusing keeps the typed, stored, shown and sent address the same.

What changes:

- **One check for every address.** `domain.ValidateEmail` returns the parsed address, and callers store and send to it. It covers the customer's email and CC list, and the `email` and CC list of both send endpoints (decision 6). Before, that `email` was only checked for being present. It also covers team invites, the company email in settings and the bootstrap owner email. The customer service's copy of the check is deleted.
- **Stored addresses are not rewritten.** The sender checks To and every Cc again before it connects. If one fails, the send fails as a permanent error: the outbox row is marked `failed` at once, no bounce is recorded, and the operator fixes the address and sends again. A failing Cc fails the whole send, like a Cc with a control character did before; it is not dropped.

The dashboard forms already refused these values, so only API callers see the new 422.
