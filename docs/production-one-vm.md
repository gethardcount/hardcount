# Run Hardcount in production on one VM

This page takes a fresh Linux server to a Hardcount that bills real customers, with Docker Compose.

Hardcount is down while it restarts. If that is not acceptable, see [High availability](ops/high-availability.md).

Load-tested on a €6.49/month Hetzner CX23 (ingest, billing and dashboard calls, without HTTPS, Stripe or email): see [How much traffic Hardcount handles](scale.md#what-one-small-server-handles).

At the end you have:

- Hardcount at `https://billing.example.com`, with a certificate that renews itself.
- Emails that reach your customers, with working links.
- Your Stripe account connected, so invoices are charged to the customer's card.
- A nightly backup, and a restore you have tested.

## Before you start

- A Linux VM with Docker 24 or newer and the Compose plugin. Check with `docker compose version`. To install both on Ubuntu, follow [Docker's install guide](https://docs.docker.com/engine/install/ubuntu/). Its convenience script, `get.docker.com`, is quickest on a fresh server, but Docker does not recommend it for production.
- A domain name for Hardcount, such as `billing.example.com`, with a DNS `A` record pointing at the VM.
- Ports 80 and 443 open to the internet. Port 80 is needed to get the HTTPS certificate.
- An account with an email provider that gives you SMTP settings, such as Postmark, SendGrid or AWS SES.
- A Stripe account.

Every example uses `billing.example.com`. Replace it with your own address.

## 1. Install a pinned release

A release is a tested version with a number, such as `v0.9.1`. Production runs a release, not the newest code: the newest code can hold changes that are not released yet. Find the newest release on the [releases page](https://github.com/gethardcount/hardcount/releases). This page uses `v0.9.1`. A Velox install (v0.8.x) cannot be upgraded to Hardcount: install fresh.

1. Get the files for that release.

   ```bash
   HARDCOUNT_VERSION=v0.9.1
   sudo git clone --branch "$HARDCOUNT_VERSION" https://github.com/gethardcount/hardcount.git /opt/hardcount
   sudo chown -R "$USER" /opt/hardcount
   cd /opt/hardcount/deploy/compose
   cp .env.example .env
   ```

   **You see:** `Note: switching to '…'` from git. That is normal: you are on a release, not on a branch.

2. Generate the four secrets, pin the release images and keep port 80 free for HTTPS. These commands fill in the lines `.env` already has, so each setting appears once. Run them in the same shell as step 1, which set `HARDCOUNT_VERSION`.

   ```bash
   sed -i \
     -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" \
     -e "s|^HARDCOUNT_APP_DB_PASSWORD=.*|HARDCOUNT_APP_DB_PASSWORD=$(openssl rand -hex 24)|" \
     -e "s|^HARDCOUNT_ENCRYPTION_KEY=.*|HARDCOUNT_ENCRYPTION_KEY=$(openssl rand -hex 32)|" \
     -e "s|^HARDCOUNT_BOOTSTRAP_TOKEN=.*|HARDCOUNT_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)|" \
     -e "s|^# HARDCOUNT_IMAGE=.*|HARDCOUNT_IMAGE=ghcr.io/gethardcount/hardcount:${HARDCOUNT_VERSION}|" \
     -e "s|^# HARDCOUNT_DASHBOARD_IMAGE=.*|HARDCOUNT_DASHBOARD_IMAGE=ghcr.io/gethardcount/hardcount-dashboard:${HARDCOUNT_VERSION}|" \
     -e "s|^NGINX_HTTP_PORT=.*|NGINX_HTTP_PORT=127.0.0.1:8080|" \
     .env
   chmod 600 .env
   grep -cE '^(POSTGRES_PASSWORD|HARDCOUNT_APP_DB_PASSWORD|HARDCOUNT_ENCRYPTION_KEY|HARDCOUNT_BOOTSTRAP_TOKEN|HARDCOUNT_IMAGE|HARDCOUNT_DASHBOARD_IMAGE)=.+' .env
   grep NGINX_HTTP_PORT .env
   ```

   **You see:** `6`, then `NGINX_HTTP_PORT=127.0.0.1:8080`.

   What each line is for:

   | Line | What it is for |
   |---|---|
   | `POSTGRES_PASSWORD` | The database admin password |
   | `HARDCOUNT_APP_DB_PASSWORD` | The password of the limited database user that API requests run as |
   | `HARDCOUNT_ENCRYPTION_KEY` | Encrypts stored personal data and Stripe keys |
   | `HARDCOUNT_BOOTSTRAP_TOKEN` | Lets you create the first account in step 2 |
   | `HARDCOUNT_IMAGE`, `HARDCOUNT_DASHBOARD_IMAGE` | Run the published release images instead of [building from source](../deploy/compose/README.md#build-from-source-instead) |
   | `NGINX_HTTP_PORT=127.0.0.1:8080` | Hardcount listens only on the VM itself, on port 8080. The HTTPS server in step 3 sits in front of it. |

3. Start Hardcount.

   ```bash
   docker compose up -d
   docker compose exec hardcount-api /usr/local/bin/hardcount version
   ```

   **You see:** five containers start, then `hardcount v0.9.1`.

4. Check that it is healthy. On the first start Hardcount sets up its database (runs its migrations), which takes about 10 seconds. Until then the check fails with a 502, so this command retries every 2 seconds and gives up after 2 minutes.

   ```bash
   timeout 120 bash -c \
     'until curl -fsS http://localhost:8080/health/ready; do sleep 2; done'
   ```

   **You see:** perhaps a few `curl: (22) The requested URL returned error: 502` lines, then `{"checks":{"api":"ok","database":"ok","scheduler":"..."},"status":"ok"}`. If the command ends with no `"status":"ok"`, see the fixes linked below.

If a step fails, [If something goes wrong](../deploy/compose/README.md#if-something-goes-wrong) lists each start-up error and its fix.

## 2. Create your account and first login

An account (a "tenant" in the API) is one business, with its own data, dashboard users and API keys.

Run this on the VM. The reply holds your password and a live API key, so it must not cross the network before HTTPS is set up.

```bash
set -a; . ./.env; set +a
curl -X POST http://localhost:8080/v1/bootstrap \
  -H "Authorization: Bearer ${HARDCOUNT_BOOTSTRAP_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"tenant_name":"Acme","owner_email":"you@example.com","owner_password":"a-strong-password-12ch"}'
```

**You see:** JSON with `owner_email`, `owner_password`, `secret_key_test` (`hc_secret_test_…`) and `secret_key_live` (`hc_secret_live_…`).

Save it in your password manager now. It is shown once, and Hardcount keeps only hashes, so it cannot be shown again. A second call answers `409 already_bootstrapped`.

## 3. Add HTTPS with Caddy

Caddy is a web server that gets a free HTTPS certificate from Let's Encrypt and renews it by itself. Any HTTPS proxy works. This page uses Caddy because it needs no certificate setup.

1. Install Caddy on the VM: follow [caddyserver.com/docs/install](https://caddyserver.com/docs/install) for your Linux.
2. Replace `/etc/caddy/Caddyfile` with these three lines:

   ```text
   billing.example.com {
       reverse_proxy 127.0.0.1:8080
   }
   ```

3. Reload Caddy.

   ```bash
   sudo systemctl reload caddy
   curl -fsS https://billing.example.com/health
   ```

   **You see:** `{"status":"ok"}`. The first request can take a few seconds while Caddy gets the certificate.

4. Open `https://billing.example.com` in your browser and sign in with the login from step 2.

   **You see:** the dashboard, in test mode. Nothing in test mode charges real money.

`.env` sets `APP_ENV=production`, so the sign-in cookie is `Secure`: sign in at your `https://` address ([before HTTPS](../deploy/compose/README.md#sign-in-before-https)).

Hardcount reads each visitor's real address from the headers Caddy and nginx forward. Caddy's requests reach Hardcount through Docker's private network, which the Compose stack's default `TRUST_PROXY` already trusts, so leave that setting as it is.

## 4. Set the web addresses

Emails and Stripe send people to pages in Hardcount: your customers to the invoice page and the page to add a card, and your team to the password-reset page. Hardcount needs your address to build those links. It never guesses the address from a request.

1. Add four lines to `.env`. With this setup the dashboard and the customer pages share one address.

   ```bash
   cat >> .env <<'EOF'
   HOSTED_INVOICE_BASE_URL=https://billing.example.com
   PAYMENT_UPDATE_URL=https://billing.example.com/update-payment
   DASHBOARD_BASE_URL=https://billing.example.com
   CUSTOMER_PORTAL_URL=https://billing.example.com
   EOF
   docker compose up -d
   ```

   **You see:** `hardcount-api` is recreated. What each one is for: [Email setup](ops/email-setup.md#what-breaks-when-a-setting-is-missing).

2. Check the start-up log.

   ```bash
   docker compose logs hardcount-api \
     | grep -E "(HOSTED_INVOICE_BASE_URL|PAYMENT_UPDATE_URL|DASHBOARD_BASE_URL|CUSTOMER_PORTAL_URL) NOT SET"
   ```

   **You see:** nothing. Each of these settings that is still missing logs a warning that starts with its name and `NOT SET`.

### Other start-up warnings

Hardcount logs a few more warnings when it starts. To list them all:

```bash
docker compose logs hardcount-api | grep '"level":"WARN"'
```

| Warning starts with | What it means | Do you need to act? |
|---|---|---|
| `SMTP NOT CONFIGURED` | No email is sent. | Yes. [Step 5](#5-connect-email) fixes it. |
| `HARDCOUNT_EMAIL_BIDX_KEY not set` | Hardcount cannot find a customer by email address, so it cannot stop emailing an address that bounced. | Yes, before your first customer. See below. |
| `POSTMARK_WEBHOOK_USER/POSTMARK_WEBHOOK_PASS not set` | Hardcount turns away Postmark's reports of late bounces. | Only if you send email through Postmark. See [Email setup](ops/email-setup.md#when-postmark-reports-a-bounce-later). |
| `METRICS_TOKEN not set` | `/metrics` answers 401 to everyone. | Only if you collect metrics with Prometheus. See [What to watch](self-host.md#what-to-watch). |
| `HARDCOUNT_API_BASE_URL NOT SET` | The link you copy to share a customer's cost dashboard (a page of their usage and charges) is a path, not a full address. | Only if you share cost dashboards. Set it in `.env` to your HTTPS address. |
| `RUN_MIGRATIONS_ON_BOOT=true in production` | Hardcount updates its database as it starts. | No. On one VM this is how upgrades apply. The warning is for setups with several copies of Hardcount. |
| `DASHBOARD_BASE_URL is an http:// address`, or the same for another address setting | That setting starts with `http://`, so Hardcount is served without HTTPS there. Sign-in needs HTTPS, and links to that address are not encrypted. | Yes. Change it to your `https://` address, as in [step 4](#4-set-the-web-addresses). |

To set `HARDCOUNT_EMAIL_BIDX_KEY`, add a random key and restart. Set it before you add customers: Hardcount indexes a customer's email only when the customer is created or edited.

```bash
echo "HARDCOUNT_EMAIL_BIDX_KEY=$(openssl rand -hex 32)" >> .env
docker compose up -d
sleep 10
docker compose logs hardcount-api | grep "email blind index enabled"
```

**You see:** a line with `email blind index enabled`. Do not change this key later: a new key does not match the customers indexed with the old one.

## 5. Connect email

Hardcount sends invoices, receipts, payment reminders, password resets and team invites through your email provider's SMTP server.

1. Add your provider's SMTP settings to `.env`. Use an address on a domain you have set up with your provider for `SMTP_FROM`.

   ```bash
   cat >> .env <<'EOF'
   SMTP_HOST=smtp.example.com
   SMTP_PORT=587
   SMTP_TLS=starttls
   SMTP_USERNAME=your-smtp-user
   SMTP_PASSWORD=your-smtp-password
   SMTP_FROM=billing@example.com
   EOF
   docker compose up -d
   ```

   **You see:** `hardcount-api` is recreated. `docker compose logs hardcount-api | grep "SMTP configured"` shows your host.

2. Send yourself a password-reset email. Sign out, click **Forgot password?** on the sign-in page and enter your email.

   **You see:** the reset email arrives within a few minutes, and its link opens `https://billing.example.com/reset-password`. If it does not arrive, check spam, then `docker compose logs hardcount-api`.

**Settings → Email** shows whether each email setting is in place and has a **Send test email** button.

Settings for each provider, sender-domain setup (SPF and DKIM) and bounce handling are in [Email setup](ops/email-setup.md).

## 6. Connect Stripe

Hardcount works out each bill and makes the invoice. Stripe charges the card. You connect your own Stripe account in the dashboard, so the money goes straight to you. There is no Stripe setting in `.env`.

Start in test mode. Connect live mode the same way when you are ready to take real money.

1. In Stripe, open **Developers → API keys** and copy the secret key (`sk_test_…`) and the publishable key (`pk_test_…`).
2. In Hardcount, open **Settings → Payments**. Under step 1, **Connect your Stripe API keys**, paste both keys and save.

   **You see:** "Verified as" and your Stripe account name.

3. Step 2, **Deliver Stripe events to Hardcount**, shows a webhook URL. A webhook is a message Stripe sends to Hardcount when something happens, for example when a card payment succeeds. Copy the URL. It looks like `https://billing.example.com/v1/webhooks/stripe/hc_spc_…`.
4. In Stripe, open **Developers → Webhooks** and add an endpoint. Paste the URL and pick the events that Hardcount lists under the URL. They include the three `charge.dispute.*` events, so Hardcount records card disputes ([Card disputes](ops/runbook/card-disputes.md)).
5. Stripe shows the endpoint's signing secret (`whsec_…`). Paste it into step 2 in Hardcount and click **Finish setup**.

   **You see:** the card's badge reads **Connected**.

Without step 5, Stripe's page tells a customer their card is saved, but Hardcount never hears about it.

To check a whole charge in test mode, from saving a card to a refund, follow [Stripe end-to-end test](dev/stripe-end-to-end-test.md).

## 7. Back up every night

Two things must be backed up, and kept apart:

- **The database.** It holds every customer, invoice and payment.
- **The `.env` file.** It holds `HARDCOUNT_ENCRYPTION_KEY`, without which a backup cannot be read ([why](ops/backup-considerations.md#1-keep-the-encryption-key)). Copy `.env` to your password manager or a secrets store now, not next to the backups.

The repository ships `scripts/backup.sh`. It dumps the database, checks that the dump can be read, and deletes dumps older than 30 days. The commands below run it in a Postgres 16 container, so the VM needs no Postgres tools.

1. Create the backup command.

   ```bash
   sudo mkdir -p /var/backups/hardcount
   sudo chmod 700 /var/backups/hardcount
   sudo tee /usr/local/bin/hardcount-backup >/dev/null <<'EOF'
   #!/usr/bin/env bash
   set -euo pipefail
   cd /opt/hardcount/deploy/compose
   set -a; . ./.env; set +a
   docker run --rm \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/hardcount/scripts:/scripts:ro \
     -v /var/backups/hardcount:/var/backups/hardcount \
     -e DATABASE_URL="postgres://hardcount:${POSTGRES_PASSWORD}@localhost:5432/hardcount?sslmode=disable" \
     postgres:16 bash /scripts/backup.sh
   EOF
   sudo chmod 755 /usr/local/bin/hardcount-backup
   ```

   `--network "container:…"` puts the backup container next to the database, which has no port open on the VM. `chmod 700` lets only root open the backup folder, because the dumps hold every customer's data.

2. Run it once.

   ```bash
   sudo hardcount-backup
   ```

   **You see:** log lines ending with a "Backup Summary" block that says `Verified:  yes`. The dump is in `/var/backups/hardcount`, and `latest.dump` points at the newest one.

3. Run it every night at 03:00.

   ```bash
   echo '0 3 * * * root /usr/local/bin/hardcount-backup >> /var/log/hardcount-backup.log 2>&1' \
     | sudo tee /etc/cron.d/hardcount-backup
   ```

   **You see:** the line echoed back. Tomorrow, `/var/log/hardcount-backup.log` has the first run.

4. Copy `/var/backups/hardcount` off the VM, for example with your cloud's object storage tool. A backup on the same disk is lost with the VM.

### Test a restore

This test restores the newest dump into a second, empty database on the same server. Your live data is not touched.

1. Restore into a test database.

   ```bash
   cd /opt/hardcount/deploy/compose
   set -a; . ./.env; set +a
   docker compose exec -T postgres createdb -U hardcount hardcount_restore_test
   docker run --rm \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/hardcount/scripts:/scripts:ro \
     -v /var/backups/hardcount:/var/backups/hardcount:ro \
     -e DATABASE_URL="postgres://hardcount:${POSTGRES_PASSWORD}@localhost:5432/hardcount_restore_test?sslmode=disable" \
     -e HARDCOUNT_RESTORE_CONFIRM=yes \
     postgres:16 bash /scripts/restore.sh /var/backups/hardcount/latest.dump
   ```

   **You see:** `All critical tables present.` and then `Restore complete.`

2. Compare the restored data with the live data.

   ```bash
   for db in hardcount hardcount_restore_test; do
     docker compose exec -T postgres psql -U hardcount -d "$db" -tAc \
       "SELECT count(*) FROM invoices"
   done
   ```

   **You see:** the same number twice, or a slightly higher first number if invoices were made since the backup.

3. Delete the test database.

   ```bash
   docker compose exec -T postgres dropdb -U hardcount hardcount_restore_test
   ```

Repeat this test every few months, and after every upgrade. More on what to back up, and on point-in-time restore: [Backups](ops/backup-considerations.md).

## Who can sign in to the dashboard

Anyone who can open your address and knows a team member's password can sign in. Hardcount protects sign-in with no setup:

- **Passwords are stored only as bcrypt hashes,** which are slow to crack.
- **About 30,000 common leaked passwords are refused** when a password is set or reset.
- **Sign-in is rate-limited:** one address can try only so many times a minute.
- **Repeated wrong passwords slow down,** from one address or spread over many, instead of locking the account. A browser that signed in before is not slowed by other people's guesses ([how it works](ops/runbook/sign-in-throttled.md)).
- **The sign-in cookie travels only over HTTPS** outside `localhost`, because `.env` sets `APP_ENV=production` ([step 3](#3-add-https-with-caddy)).

Hardcount has no two-factor sign-in yet, so a stolen password is enough to sign in. If your team wants a second factor, you can put sign-in behind your own SSO or VPN. This is optional: Hardcount runs in production without it.

To do it, let only your team reach `/v1/auth/`. Every sign-in, password reset and invite acceptance goes through that path, and the dashboard shows no data until someone signs in. If your SSO tool needs a page to send people through, add the sign-in page, `/login`.

Keep these paths reachable from the internet. The dashboard shares its address with them:

| Path | Who uses it |
|---|---|
| `/v1/webhooks/` | Stripe, to tell Hardcount about payments. Postmark too, if you send email through it. |
| `/invoice/…`, `/update-payment`, `/payment-method-added` | Your customers: the invoice page, the page to add a card, and the page Stripe sends them back to |
| `/v1/public/` | The customer pages above, and shared cost dashboards |
| `/assets/`, `/favicon.svg` | The page files. The customer pages and the dashboard load the same ones. |
| The rest of `/v1/` | Your app, which sends an API key with each call |

## You are live when

Check each line before the first real customer.

- [ ] `docker compose exec hardcount-api /usr/local/bin/hardcount version` shows a release, not `dev`.
- [ ] `https://billing.example.com/health/ready` answers `"status":"ok"`.
- [ ] `docker compose exec postgres psql -U hardcount_app -d hardcount -tAc "SHOW idle_in_transaction_session_timeout"` shows `30s`. If it shows `0`, set it as [Postgres requirements](ops/postgres-requirements.md#timeouts-on-the-limited-user) says.
- [ ] You can sign in at `https://billing.example.com`.
- [ ] A password-reset email arrived, and its link opens your `https://` address.
- [ ] **Settings → Payments** reads **Connected** in test mode, and a test charge worked ([Stripe end-to-end test](dev/stripe-end-to-end-test.md)).
- [ ] The **Dunning policies** page shows the policy you want as the default. Dunning means retrying a failed card payment and emailing the customer. A new account starts with "Default retries": three retries over two weeks, then the subscription keeps billing and the invoice stays open. Edit it, or create your own, if you want collection paused or the invoice written off instead.
- [ ] `/var/log/hardcount-backup.log` shows a nightly backup, a copy is off the VM, and a restore test passed.
- [ ] `.env` is stored somewhere safe, apart from the backups.
- [ ] If you collect metrics: `METRICS_TOKEN` is set, and Prometheus sends it. See [What to watch](self-host.md#what-to-watch).
- [ ] For real money: switch to live mode with the Test/Live toggle in the sidebar, then connect your live Stripe keys and live webhook under **Settings → Payments** the same way. Your app uses the live API key, `hc_secret_live_…`.

## Upgrade to a new release

Hardcount was called Velox before v0.9.0. v0.9.0 installs fresh only: it refuses a database made by Velox, a 0.8.x release or a build of `main` ([why](ops/runbook/migration-failed-midway.md#the-database-was-made-by-velox)). A Velox install does not get that far with these steps: its `.env` names `VELOX_*` settings, so `docker compose pull` in step 4 stops with `required variable HARDCOUNT_APP_DB_PASSWORD is missing a value`. Install Hardcount fresh, from [step 1](#1-install-a-pinned-release) of this page, with an empty database: a new VM, or a new Compose data volume.

Database changes (migrations) run by themselves as the new version starts.

1. Read the upgrade notes in the [changelog](../CHANGELOG.md) for every release between yours and the new one.
2. Take a backup. If the upgrade goes wrong, it is how you go back ([Going back](#going-back)).

   ```bash
   sudo hardcount-backup
   ```

   **You see:** `Verified:  yes`.

3. Get the new release's files and point `.env` at its images. This example moves to `v0.9.1`.

   ```bash
   HARDCOUNT_VERSION=v0.9.1
   cd /opt/hardcount
   git fetch --tags
   git checkout "$HARDCOUNT_VERSION"
   cd deploy/compose
   sed -i "s|^HARDCOUNT_IMAGE=.*|HARDCOUNT_IMAGE=ghcr.io/gethardcount/hardcount:${HARDCOUNT_VERSION}|; s|^HARDCOUNT_DASHBOARD_IMAGE=.*|HARDCOUNT_DASHBOARD_IMAGE=ghcr.io/gethardcount/hardcount-dashboard:${HARDCOUNT_VERSION}|" .env
   ```

   **You see:** `HEAD is now at …` from git.

4. Pull the new images and see what the upgrade will change. `migrate status` only reads.

   ```bash
   docker compose pull
   docker compose run --rm hardcount-api migrate status
   ```

   **You see:** a few `Container … Running` lines, then the plan. It lists the migrations to apply. If it says `no migration record`, stop: the upgrade will not run ([why](ops/runbook/migration-failed-midway.md#tables-but-no-migration-record)).

5. Start the new images.

   ```bash
   docker compose up -d
   docker compose exec hardcount-api /usr/local/bin/hardcount version
   ```

   **You see:** `hardcount v0.9.1`, and `curl -fsS http://localhost:8080/health/ready` answers `"status":"ok"` again.

If the upgrade stops with an error, Hardcount does not start, and nothing needs repair: a failed migration is undone. `docker compose logs --tail 20 hardcount-api` shows the error. Compose keeps starting the API again, and each start tries the upgrade again, so stop it while you find the cause:

```bash
docker compose stop hardcount-api
```

Then follow [A database upgrade stopped](ops/runbook/migration-failed-midway.md). If the error says `waited 5s for a table lock`, something held a lock on a table the upgrade changes, often a long-running query: see [An upgrade waited for a table lock](ops/runbook/migration-failed-midway.md#it-waited-5s-for-a-table-lock). When the cause is fixed, start the API again:

```bash
docker compose up -d hardcount-api
```

### Going back

Never roll the database back with `hardcount migrate rollback`: it runs only with `APP_ENV=local`, because it loses data the newer release wrote. To go back, restore the backup from step 2. That is why every upgrade starts with a backup: without one, a database upgrade cannot be undone.

**A restore loses everything written after the backup.** The old release kept running after step 2, and the new one ran after step 5. The restore removes what both wrote: usage events your app sent, invoices Hardcount built and charged, payments and refunds, and changes made in the dashboard or through the API. The longer the new release runs, the more a restore loses.

To go back with a restore:

1. Stop the API, so nothing more is written.

   ```bash
   docker compose stop hardcount-api
   ```

   **You see:** a `Container … Stopped` line.

2. Find the backup from step 2. Its name holds the time it started, in UTC. Use that file, not a newer nightly backup: a backup taken after step 5 already holds the new release's changes.

   ```bash
   ls -l /var/backups/hardcount/
   ```

   **You see:** files such as `hardcount_20261019_143000.dump`, a backup started at 14:30:00 UTC on 19 October 2026.

3. List the invoices created or changed since that time, and keep the list. Run this in the database shell (`docker compose exec postgres psql -U hardcount -d hardcount`), with your backup's time:

   ```sql
   SELECT id, livemode, status, payment_status, total_amount_cents, updated_at
   FROM invoices
   WHERE updated_at >= '2026-10-19 14:30:00+00'
   ORDER BY updated_at;
   ```

   **You see:** one row for each invoice that the restore removes, or takes back to how it was at the backup.

4. Restore the backup into an empty database, then start the old release. See [Restore a backup](ops/backup-considerations.md#4-restore-a-backup).
5. Put back what the restore removed.
   - **Usage events.** Have your app send again every event it sent after the backup time, with the same `idempotency_key`. Hardcount counts each key once, so an event the backup already holds is not counted twice. An event that is not sent again is not billed.
   - **Payments.** The restored database does not know about a payment Stripe took after the backup. A subscription's invoice built after the backup is built again by the next billing run, and the customer's card can be charged again. Check each invoice on your list in Stripe, and refund a second charge.
   - **Everything else.** Run the [checks after a restore](ops/backup-considerations.md#checks-after-a-restore).

**Next:** send your first usage event and bill it: [First invoice](first-invoice.md).
