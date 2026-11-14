# What data Hardcount stores

This page is for whoever evaluates or runs Hardcount. It lists the personal and secret data Hardcount keeps, where it keeps it, how each piece is protected, what leaves your server, and how long it stays. The threats these controls answer are in the [threat model](threat-model.md).

Everything below is in your Postgres database. Besides it, Hardcount keeps only short-lived rate-limit counters in Redis, some of them named by client IP address.

## How data is protected at rest

| Protection | Key or method | Used for |
|---|---|---|
| **Encrypted** | AES-256-GCM with `HARDCOUNT_ENCRYPTION_KEY` | Customer names, emails, phone numbers and tax IDs, signing secrets, Stripe keys, hosted invoice links |
| **Blind index** | HMAC-SHA256 with `HARDCOUNT_EMAIL_BIDX_KEY` | Finding a customer by email (for bounce reports) without reading the encrypted address |
| **Hashed** | bcrypt, or SHA-256 | Passwords, API keys, session IDs, link tokens |
| **Plain** | None | Everything else, including the copies listed under [Known gaps](#known-gaps) |

`HARDCOUNT_ENCRYPTION_KEY` is required in production. Elsewhere, without it, the "encrypted" fields are stored in plain text and start-up logs a warning. Both keys: [Settings, Security and keys](../settings.md#security-and-keys). Hardcount has no built-in rotation for either key.

## Personal and sensitive data

"Encrypted" means encrypted when `HARDCOUNT_ENCRYPTION_KEY` is set.

| Data | Where | At rest | Also appears in |
|---|---|---|---|
| Customer display name | `customers.display_name` | Encrypted | Customer CSV export, audit log (plain), Stripe, emails, outbound webhooks |
| Customer email and CC addresses | `customers.email`, `customers.additional_emails` | Encrypted, plus the blind index `customers.email_bidx` | Customer CSV export, email queue (plain), server log, Stripe, emails, outbound webhooks |
| Legal name, phone | `customer_billing_profiles.legal_name`, `.phone` | Encrypted | Stripe; the legal name in the audit log (plain) |
| Postal address | `customer_billing_profiles.address_line1` to `.country` | Plain | Stripe, Stripe Tax, tax records, issued invoices, invoice PDFs |
| Tax ID | `customer_billing_profiles.tax_id` | Encrypted | `invoices.tax_id` and tax records (plain), Stripe, invoice PDFs |
| Issued invoice's buyer details | `invoices.bill_to_snapshot`, `credit_notes.bill_to_snapshot` | Name, email and tax ID encrypted; address plain | Invoice and credit note PDFs |
| Card details | `payment_methods`: brand, last 4 digits, expiry month and year, Stripe fingerprint and ID; `invoices.payment_card_brand`, `.payment_card_last4` | Plain | Dashboard, API |
| Invoice contents | `invoices`, `invoice_line_items`, `credit_notes` | Plain | Invoice CSV export, PDFs, emails, outbound webhooks |
| Usage events | `usage_events`, with your `properties` JSON | Plain | Usage CSV export |
| IP addresses | `audit_log.ip_address`, `dashboard_sessions.ip` (with the browser's user agent) | Plain | Sign-in log lines, traces (with the user agent), the Compose stack's nginx log (with the user agent) |
| Dashboard users' emails | `users.email`, `member_invitations.email` | Plain | Audit log display (joined when read, not stored there), email queue (plain), server log |
| Passwords | `users.password_hash` | bcrypt, cost 12 | Nowhere |
| Sign-in cookie | `dashboard_sessions.id_hash` | SHA-256 | The browser holds the raw value in an `HttpOnly`, `SameSite=Lax` cookie, `Secure` in staging and production |
| API keys | `api_keys.key_hash`, `.key_salt`, `.key_prefix` | Salted SHA-256 ([ADR-141](../adr/141-api-key-storage-and-rotation.md)) | Shown once, when created |
| Link tokens: password reset, invite, payment update | `token_hash` in each table | SHA-256 | The email queue holds the link until the email is sent or skipped, and keeps it if the email fails for good |
| Hosted invoice link | `invoices.public_token_encrypted`, `.public_token_hash` | Encrypted, plus SHA-256 for lookup | Emails; the email queue, as for link tokens |
| Cost dashboard link | `customers.cost_dashboard_token_hash` | SHA-256 | Shown once, when made |
| Outbound webhook signing secrets | `webhook_endpoints.secret_encrypted`, `.secondary_secret_encrypted` | Encrypted; last 4 characters plain | Shown once |
| Stripe keys | `stripe_provider_credentials.secret_key_encrypted`, `.webhook_secret_encrypted` | Encrypted; last 4 characters and key prefix plain; publishable key plain | Nowhere |
| Sign-in throttle counters | `login_throttle.subject` | HMAC of the email, alone or with the address or a browser marker, keyed from `HARDCOUNT_ENCRYPTION_KEY` | Nowhere |
| Audit log | `audit_log` | Plain; a trigger refuses update and delete | Audit CSV export |
| Your own company details | `tenant_settings`, `invoices.supplier_snapshot` | Plain | Invoices, PDFs, emails |

The database holds no SMTP password, database password or encryption key. Those are settings, read from the environment.

## What Hardcount never stores

- **Card numbers and security codes.** Hardcount has no card form. A customer types a card into a Stripe Checkout page, and Hardcount reads back only the brand, last 4 digits, expiry and Stripe's fingerprint.
- **Raw API keys, passwords and session IDs.** Only their hashes, as listed above. Link tokens are stored as hashes too, but an email carrying a link keeps the raw link in the email queue until the email is sent or skipped, and for good if it fails for good.
- **Invoice PDFs.** Hardcount builds a PDF each time one is asked for. A PDF attached to an email is removed from the email queue once the email is sent or skipped. An email that fails for good keeps it.

## What leaves your server

| Destination | When | What is sent |
|---|---|---|
| **Stripe: customers**, with the account's own keys | Off until the account connects Stripe in Settings → Payments | Email, name (the legal name if set), phone, address, tax-exempt status, tax ID, and Hardcount's account and customer IDs |
| **Stripe: payments** | The same | Amount, currency, "Invoice" and its number, and Hardcount's invoice number and invoice, account and customer IDs. A hosted Checkout page gets this as one line, "Invoice" and its number, for the amount due. A refund sends the amount and the credit note ID. A card setup page sends Hardcount's account and customer IDs, and the invoice ID when it comes from a payment-update link. No invoice lines and no PDF. |
| **Stripe Tax** | Off. On when the account picks Stripe Tax as its tax provider (the default is manual rates) | The customer's address, tax ID and its type, the currency, each line's amount, quantity and tax code, and Hardcount's invoice ID (or credit note reference) on the recorded transaction |
| **Your email server** | Off until `SMTP_HOST` is set | Recipient and CC addresses, the customer's name, invoice number and amount, links, and the invoice PDF, which shows the buyer's name, address and tax ID. Invites and password resets go to dashboard users' addresses; an invite names the inviter's email. |
| **Webhook endpoints** | Off until an account adds an endpoint | Each event, signed, with the resource as the API returns it. A customer event carries the customer's name and email. Only `https://` addresses, except `localhost` outside production. Private, link-local and cloud metadata addresses are refused, and loopback addresses in production, when the endpoint is saved and on every connection. |
| **Your OpenTelemetry collector** | Off until `OTEL_EXPORTER_OTLP_ENDPOINT` is set | Request spans: method, route, path with link tokens removed, status, client IP, the address of the proxy in front, the browser's user agent, host name, body sizes and timings. Billing-run spans carry account, subscription, customer and test clock IDs. No query strings, other headers, bodies or database queries. |

Hardcount sends nothing to its maintainers: no usage statistics and no update check. The dashboard loads no scripts or fonts from other sites. The one thing loaded from elsewhere is your logo, from the Logo URL you set in Settings: the dashboard shows it in Settings, and your customers' browsers and mail clients load it on the payment pages and in emails.

## Logs

Logs are JSON on standard output. Where they go, and how long they stay, is up to you.

- **One line per request:** method, route (or the path, when no route matched), status, duration and request ID. No query strings, request bodies or names. Link tokens are removed from paths ([SECURITY.md](../../SECURITY.md#hardening-status)). The line also has `actor_type`, `actor_id` and `ip` fields, which are not filled in yet: they say `system` and hold no address or account.
- **One line per email sent or failed:** it names the recipient's address and the subject. A refused CC address is logged too.
- **Sign-in lines:** a failed or throttled sign-in names the client IP, the user ID when the email has an account, and a short hash of the email, never the email itself.
- **The Compose stack's nginx log:** client IP, method, path with link tokens removed, status, referrer, user agent and `X-Forwarded-For`.

## Retention and deletion

| Data | What the code does |
|---|---|
| Customers | Never deleted: archiving keeps every field. Deleting a test clock deletes its test-mode customers with their invoices, subscriptions, usage, billing profiles and emails; their audit log rows and outbound webhook events stay. |
| Accounts | No delete path. |
| Dashboard users | Removing a member deletes the membership and ends their sessions. The user row, with email and password hash, stays. |
| Invoices and credit notes | Never deleted, outside test clocks. The buyer details are fixed when the invoice is issued. |
| Audit log | Kept for good. A trigger refuses any update or delete. |
| Usage events, outbound webhook events and deliveries, Stripe events, tax records | Kept. No cleanup job. |
| Email queue | Links and the PDF are removed once an email is sent or skipped. An email that fails for good keeps them, so it can be sent again. The row, with recipient and customer name, stays. |
| Idempotency answers | Kept 24 hours, then deleted by a background job. |
| Payment-update tokens | Deleted 7 days after they expire. |
| Sign-in throttle counters | Deleted after 24 hours without an attempt; a browser that signed in is remembered for 180 days. |
| Sessions and other link tokens | Expire (a session after 7 days). The rows stay. |
| Stripe keys | Deleted when the account disconnects Stripe. |
| Webhook endpoints | Deleting one marks it deleted. The encrypted secret stays. |
| Data at Stripe | Hardcount never deletes a Stripe customer. |

**There is no built-in way to erase a person.** Editing a customer replaces the current value on the customer row only. The copies above, your backups, your logs, Stripe, your email provider and your webhook receivers keep what they received. Plan erasure requests with that list.

## Known gaps

Protect the whole database, its backups and its logs as personal data: encrypt the disk and limit who can connect. These fields are in plain text even with `HARDCOUNT_ENCRYPTION_KEY` set:

- **Postal addresses**, on the billing profile and on issued invoices.
- **The buyer's tax ID on each invoice** (`invoices.tax_id`), and the address and tax ID in each tax record (`tax_calculations.request`).
- **Customer names in the audit log.** A customer's display name or legal name is written into `audit_log` when it is created or changed. The audit log cannot be edited, so Hardcount cannot remove it.
- **Recipient addresses and customer names in the email queue** (`email_outbox.payload`), and recipient addresses in the server log. The queue also holds each email's links and PDF until it is sent or skipped, and for good if it fails for good.
- **Customer names and emails in outbound webhook events** (`webhook_events`, `webhook_outbox`).
- **Whole Stripe event bodies** (`stripe_webhook_events.payload`), which can include the payer's billing details as Stripe sent them.
- **API answers kept for idempotency** (`idempotency_keys.response_body`), for 24 hours.

Your own fields are stored as you send them. A customer's `external_id` is also written into the audit log, so do not use an email address or a name as `external_id`. The same goes for usage event `properties`.
