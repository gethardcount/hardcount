# Set up email

Velox sends email through your email provider's SMTP server. SMTP is the standard way programs hand email to a provider. This page is for the engineer who runs Velox: it connects a provider, checks that email arrives, and sets up bounce handling.

Think of Velox as the person who writes the letters and your email provider as the post office that delivers them. Velox never delivers email itself.

Velox sends:

- invoices, receipts and credit notes,
- [dunning](../glossary.md) emails (reminders after a card payment fails),
- payment-failed emails and links to add a payment method,
- password resets and team invites.

Velox sends at most 60 emails a minute (5 every 5 seconds), and you cannot change that. Emails above that rate wait in a queue and go out in order.

## Connect a provider

1. Set the SMTP settings on the Velox server. Your provider gives you these values; [Settings for each provider](#settings-for-each-provider) has them for common ones.

   ```bash
   SMTP_HOST=smtp.your-provider.com
   SMTP_PORT=587
   SMTP_USERNAME=your-username-or-apikey
   SMTP_PASSWORD=your-password-or-apikey-secret
   SMTP_FROM=billing@yourdomain.com
   SMTP_TLS=starttls         # starttls (default) | implicit | none
   ```

   `SMTP_TLS` is how the connection is encrypted: `starttls` for port 587, `implicit` for port 465. `none` sends in plain text: use it only for a local test inbox, never in production, where Velox refuses to send with it.

2. Set the web addresses that the links in emails point to. With the one-VM install, all of them use your dashboard's address.

   ```bash
   HOSTED_INVOICE_BASE_URL=https://billing.example.com
   PAYMENT_UPDATE_URL=https://billing.example.com/update-payment
   DASHBOARD_BASE_URL=https://billing.example.com
   CUSTOMER_PORTAL_URL=https://billing.example.com
   ```

   | Setting | What it is for |
   |---|---|
   | `HOSTED_INVOICE_BASE_URL` | The "View & pay invoice" and "View receipt" buttons. Velox adds `/invoice/<token>`. |
   | `PAYMENT_UPDATE_URL` | The link in the email that asks a customer to add a payment method. Velox adds `?token=…`. |
   | `DASHBOARD_BASE_URL` | Password-reset and team-invite links. |
   | `CUSTOMER_PORTAL_URL` | Not an email link. It is where Stripe sends a customer after they save a card. Velox adds `/payment-method-added`. |

3. Restart Velox. With Docker Compose, run `docker compose up -d`.

   **You see:** a start-up log line `SMTP configured` with your host. A missing setting logs a warning instead; see [What breaks when a setting is missing](#what-breaks-when-a-setting-is-missing).

4. In the dashboard, open **Settings → Email**.

   **You see:** the badge reads **Ready**, and each setting under "Email links" reads **Set**.

5. Click **Send test email**.

   **You see:** "Sent to" and your address. The email arrives within a few minutes. If it does not, check spam and the server log.

Settings → Email is new in v0.8.0. On v0.7.0 and earlier, test with a password reset instead: sign out, click **Forgot password?**, and enter your email.

## Keep email out of spam

Before you go live, set up your sending domain at your provider, so emails are trusted. Velox cannot do this for you, because the domain's DNS is yours.

- **SPF**: a DNS record that lists the servers allowed to send for your domain.
- **DKIM**: a signature on each email. Most providers turn it on when you add their DNS records.
- **DMARC**: a policy for email that fails the two checks above. Start with `p=none`, which only reports, then move to `p=quarantine` once SPF and DKIM pass.

Each provider has step-by-step DNS instructions. You do this once per sending domain.

## What breaks when a setting is missing

Velox starts without any of these. It logs one warning at start-up for each one that is missing:

| Missing | Start-up warning | What breaks |
|---|---|---|
| `SMTP_HOST` | `SMTP NOT CONFIGURED — …` | No email is sent. Queued emails are retried for about 72 hours, then marked failed. Actions that send email right away (send an invoice or a credit note, send a setup link, invite a teammate) refuse with 409 `email_not_configured`. |
| `HOSTED_INVOICE_BASE_URL` | `HOSTED_INVOICE_BASE_URL NOT SET — …` | Invoice, receipt, dunning and payment-failed emails have no link in their button. |
| `PAYMENT_UPDATE_URL` | `PAYMENT_UPDATE_URL NOT SET — …` | The "add a payment method" email is not sent. Velox sends it when an invoice has no payment method on file, and when you click Resend setup link. Payment-failed and dunning emails link to the invoice page instead, so they do not need it. |
| `DASHBOARD_BASE_URL` | `DASHBOARD_BASE_URL NOT SET — …` | Password-reset emails are not sent, and team invites fail. Velox never builds these links from the address in a request. |
| `CUSTOMER_PORTAL_URL` | `CUSTOMER_PORTAL_URL NOT SET — …` | After saving a card, Stripe sends the customer to `http://localhost:5173`, which exists only in local development. The card is still saved. |

## Settings for each provider

Each example assumes you have already set up the sending domain at the provider.

### SendGrid

```bash
SMTP_HOST=smtp.sendgrid.net
SMTP_PORT=587
SMTP_USERNAME=apikey
SMTP_PASSWORD=SG.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

`SMTP_USERNAME` is the word `apikey`; the password is your API key. Free tier: 100 emails a day. [SendGrid SMTP docs](https://docs.sendgrid.com/for-developers/sending-email/integrating-with-the-smtp-api)

### Postmark

```bash
SMTP_HOST=smtp.postmarkapp.com
SMTP_PORT=587
SMTP_USERNAME=YOUR-SERVER-API-TOKEN
SMTP_PASSWORD=YOUR-SERVER-API-TOKEN
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

The user name and the password are both the same Server API Token. Postmark is for transactional email only, so send billing email through it, not marketing. `SMTP_FROM` must be a verified Sender Signature or on a verified domain: Postmark accepts email from an unverified address and then drops it, so Velox shows it as sent while nothing arrives. Postmark can also report bounces back to Velox; see [Bounces and spam complaints](#bounces-and-spam-complaints). [Postmark SMTP docs](https://postmarkapp.com/developer/user-guide/send-email-with-smtp)

### AWS SES, port 587

```bash
SMTP_HOST=email-smtp.us-east-1.amazonaws.com   # use your region
SMTP_PORT=587
SMTP_USERNAME=YOUR-SMTP-USERNAME-FROM-IAM
SMTP_PASSWORD=YOUR-SMTP-PASSWORD-FROM-IAM
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

The user name and password are not your AWS credentials. Create SMTP credentials in the SES console (IAM → Create SMTP credentials). Verify your sending domain in SES first. A new SES account starts in sandbox mode, where it sends only to verified addresses. [SES SMTP docs](https://docs.aws.amazon.com/ses/latest/dg/send-email-smtp.html)

### AWS SES, port 465

```bash
SMTP_HOST=email-smtp.us-east-1.amazonaws.com
SMTP_PORT=465
SMTP_USERNAME=YOUR-SMTP-USERNAME-FROM-IAM
SMTP_PASSWORD=YOUR-SMTP-PASSWORD-FROM-IAM
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=implicit
```

Use this when your firewall blocks port 587. Only the connection differs.

### Mailgun

```bash
SMTP_HOST=smtp.mailgun.org
SMTP_PORT=587
SMTP_USERNAME=postmaster@yourdomain.mailgun.org
SMTP_PASSWORD=YOUR-MAILGUN-SMTP-PASSWORD
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

`SMTP_USERNAME` is the SMTP user from the Mailgun dashboard (domain → SMTP credentials), not your account email. [Mailgun SMTP docs](https://documentation.mailgun.com/en/latest/quickstart-sending.html#send-via-smtp)

### Resend

```bash
SMTP_HOST=smtp.resend.com
SMTP_PORT=587
SMTP_USERNAME=resend
SMTP_PASSWORD=re_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

`SMTP_USERNAME` is the word `resend`. Free tier: 3,000 emails a month, 100 a day. [Resend SMTP docs](https://resend.com/docs/send-with-smtp)

### Mailtrap (test inbox, not for production)

Mailtrap catches every email in its own inbox, so nothing reaches a real person. Use it on a staging server.

```bash
SMTP_HOST=sandbox.smtp.mailtrap.io
SMTP_PORT=2525
SMTP_USERNAME=YOUR-MAILTRAP-USERNAME
SMTP_PASSWORD=YOUR-MAILTRAP-PASSWORD
SMTP_FROM=billing@example.com
SMTP_TLS=starttls
```

[Mailtrap getting started](https://help.mailtrap.io/article/12-getting-started-guide)

### Mailpit (local development)

Mailpit is a local email catcher with a web inbox. The repository's root `docker-compose.yml` runs it next to Postgres and Redis.

1. Start it.

   ```bash
   docker compose up -d mailpit
   ```

2. Point Velox at it. Set the link settings too, or the links in the emails are empty.

   ```bash
   SMTP_HOST=localhost
   SMTP_PORT=1025
   SMTP_USERNAME=
   SMTP_PASSWORD=
   SMTP_FROM=billing@local.test
   SMTP_TLS=none
   HOSTED_INVOICE_BASE_URL=http://localhost:5173
   CUSTOMER_PORTAL_URL=http://localhost:5173
   PAYMENT_UPDATE_URL=http://localhost:5173/update-payment
   DASHBOARD_BASE_URL=http://localhost:5173
   ```

3. Send an email from Velox.

   **You see:** it in the inbox at <http://localhost:8025>. Nothing leaves your machine.

Local development uses the same sending code as production. There is no "print to the log" mode.

## Common mistakes

| Mistake | What you see | Fix |
|---|---|---|
| Sending domain not set up | Emails land in spam | Set up SPF and DKIM at your provider |
| Wrong user name | `535 authentication failed` | The user name differs per provider (see above) |
| `SMTP_FROM` not verified at the provider | `550 not authorized` | Verify the sending domain, or use a verified address |
| Provider still in sandbox mode | `554 recipient not verified` | Move the account out of sandbox mode (SES), or verify the recipient |
| Firewall blocks port 587 | Connection timeout | Use port 465 and `SMTP_TLS=implicit` |
| Provider limits your sending rate | `421 throttled` | Raise the limit in your provider's dashboard |

## Check a real invoice email

Send one invoice email and follow it.

1. Set three shell variables:
   - `API`: your Velox address, such as `https://billing.example.com`.
   - `KEY`: a secret API key (`vlx_secret_…`) in the same mode, test or live, as the invoice. See [API keys](../api-keys.md).
   - `INV_ID`: the ID of a finalized invoice.
2. Send it.

   ```bash
   curl -X POST -H "Authorization: Bearer $KEY" \
     -H "Content-Type: application/json" \
     -d '{"email":"you@yourdomain.com"}' \
     "$API/v1/invoices/$INV_ID/send"
   ```

   **You see:** a 2xx answer, and soon after, the email in your inbox.

3. Open the customer's page in the dashboard and find **Sent emails**.

   **You see:** the email with status `dispatched`, which means your provider accepted it. With the Postmark webhook below, it then changes to `delivered`.

If the email stays `pending`, Velox is retrying it. The server log shows the provider's error; fix the cause it names. Velox retries for about 72 hours, then marks the email failed.

## Bounces and spam complaints

A bounce is an email that could not be delivered. Velox stops emailing an address that bounced or that marked Velox's email as spam.

Both ways below need `VELOX_EMAIL_BIDX_KEY` (64 hex characters, `openssl rand -hex 32`). Velox uses it to find the customer by email address, because stored emails are encrypted. Without it, Velox cannot mark a customer as bounced, and it logs a warning at start-up.

### When the SMTP server rejects the address

Velox marks the customer's `email_status` as `bounced`, and sends no more email to that address. A rejection is a 550, 551, 552 or 553 reply, or any 5xx reply whose status starts with 5.1 or 5.2. This catches failures that happen while Velox is still connected.

These do not mark a customer bounced, and Velox retries them:

- an authentication error (535),
- a policy rejection (a 5.7.x status such as `550 5.7.1 relaying denied`),
- a 4xx reply,
- a network error such as a timeout.

A 550 to 553 reply with no 5.7.x status counts as a bounce at any stage of the conversation, including a rejected From address.

### When Postmark reports a bounce later

Some bounces arrive later: the provider accepts the email, and the recipient's mailbox rejects it afterwards. Velox hears about these only from **Postmark**, through a webhook (a message Postmark sends to Velox) at `POST /v1/webhooks/postmark`.

1. Pick a user name and a password for the webhook, and set them on the Velox server:

   ```bash
   POSTMARK_WEBHOOK_USER=velox-postmark
   POSTMARK_WEBHOOK_PASS=$(openssl rand -hex 24)
   ```

   On the Compose stack, put them in `.env`.
2. Restart Velox.

   **You see:** no `POSTMARK_WEBHOOK_USER/POSTMARK_WEBHOOK_PASS not set` warning at start-up.
3. In Postmark, open your server, then **Webhooks**, and add this URL, with **Delivery**, **Bounce** and **Spam Complaint** checked:

   ```text
   https://<user>:<pass>@your-api-host/v1/webhooks/postmark
   ```

   Postmark does not sign its webhooks, so the user name and password in the URL prove that a call comes from your Postmark account.
4. Send a test invoice email.

   **You see:** on the customer page, under **Sent emails**, the email's status changes from `dispatched` to `delivered` once Postmark reports it.

What Velox does with each report:

- **Delivery**: marks the email `delivered`.
- **Bounce**: a permanent bounce (Postmark calls it a hard bounce, or has deactivated the address) marks the email `bounced` and the customer `bounced`, so Velox sends no more email to that address. A temporary bounce changes nothing; Postmark retries it.
- **Spam Complaint**: shows the email as `marked as spam`, marks the customer `complained`, and stops email to that address.

Velox also sends the `customer.email_bounced` or `customer.email_complained` [webhook event](../webhooks.md) when it marks a customer.

If the two settings are unset, the route answers 401 to every call, and emails stay in the state Velox last knew.

**Other providers (SES, SendGrid, Mailgun, Resend):** Velox cannot receive their bounce reports. Turn on suppression (a do-not-send list) in the provider's dashboard, so it stops sending to addresses that bounce.

## Sending a lot of email

- **Velox sends at most 60 emails a minute.** Check that your provider's limit is at least that.
- **Use one provider and sending domain for billing email only.** Some teams send marketing email through a different provider and domain.
- **Ask your provider about dedicated IP addresses** before you send more than about 50,000 emails a day from one sender.

Velox sends only through SMTP. It has no direct connection to a provider's own API, so it cannot show opens or clicks.
