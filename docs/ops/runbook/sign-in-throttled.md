# Sign-in says "Too many sign-in attempts"

A team member cannot sign in because of the sign-in throttle. Back to the
[runbook index](index.md).

## Symptom

- The sign-in page says `Too many sign-in attempts. Try again in N minutes.`
- `POST /v1/auth/login` answers 429 with code `login_throttled`.
- The log has `auth: sign-in throttled` lines, and
  `hardcount_auth_login_total{outcome="throttled"}` rises.

## Why

Hardcount counts sign-in tries in the `login_throttle` table before it checks
the password. A wait of 1 to 15 minutes starts at the 5th try from one
address for one email. It also starts at the 10th try for one email from
any address, counting only browsers that never signed in to it. A count
with no try for 24 hours starts over.

The wait does not lock the account:

- A browser that signed in to the email before counts on its own, so other
  people's guesses do not slow it.
- A completed password reset or an accepted invite marks the browser that
  completed it, and that browser can then sign in during the wait.

A browser that never signed in to the email waits for as long as someone
keeps guessing.

Two causes are common:

- **Someone is guessing that person's password.** The wait is doing its job.
- **`TRUST_PROXY` is wrong**, so every request looks like it comes from your
  load balancer. Then 5 wrong passwords for an email, from anyone, make every
  new browser wait.

## Check

1. Is the client address right? Sign in, switch the dashboard to test mode
   and open the audit log. The newest `login` entry shows the address Hardcount
   saw. If it is your load balancer's or proxy's address, fix `TRUST_PROXY`
   ([Settings](../../self-host.md#settings)).
2. Who is trying? Each wrong password logs `auth: failed login attempt` with
   `account` (the same 12 characters for every try on one email, from any
   address), `user_id` when the email has an account, and `ip`:

   ```bash
   docker compose logs hardcount-api | grep -E 'failed login attempt|sign-in throttled'
   ```

## Fix

- **Sign in from a browser used before.** Guesses from elsewhere do not slow
  it.
- **Or reset the password from the browser you want to use.** This needs
  email set up (`SMTP_HOST` and `DASHBOARD_BASE_URL`). The reset marks that
  browser, and it can then sign in.
- **Without email, or if you know the tries were not an attack, clear every
  count.** The table holds only counters, no account data. Run it in `psql`:

  ```sql
  DELETE FROM login_throttle;
  ```

  Browsers that signed in before are then treated as new until their next
  sign-in.

## Verify

The person signs in, and `hardcount_auth_login_total{outcome="ok"}` rises.
