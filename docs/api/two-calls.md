# Send usage and read a balance: curl, Node, Python and Go

Most integrations make two calls: they send usage as it happens, and they
read how much credit a customer has left. This page makes both calls in four
languages, with standard libraries only, so you can copy the one you use.
Hardcount has no official client library (SDK) yet.

Each example retries the way the API expects:

- **It makes the usage event's `idempotency_key` once, before the first
  try,** and sends the same event on every retry. Hardcount never counts one
  key twice, so a retry cannot bill the usage again.
- **On `429`, it waits the seconds in `Retry-After`.** See
  [Rate limits](rate-limits.md).
- **On no answer or a `5xx`, such as `503` `ingest_unavailable`, it waits 2,
  4, 8, then 16 seconds.** It gives up after 5 tries.
- **On any other error, it stops and prints the code.** Fix the request; see
  [Errors](errors.md).

Retrying a `5xx` is safe here because each usage event carries its own key.
For other writes, read [Idempotency](idempotency.md#a-retry-loop) first.

## Before you start

You need a running Hardcount, a secret test key and a customer on a plan
that prices the `tokens` meter. Steps 1 to 4 of
[Send usage and get your first invoice](../first-invoice.md) make all of
them: customer `acme-1`, priced per model and token type.

Both calls need a secret key. A usage-only key can send the event but cannot
read the balance.

Set four variables. The balance route takes Hardcount's ID for the customer
(`hc_cus_…`), so look it up from your own ID once:

```bash
export HARDCOUNT=http://localhost:8080
export HARDCOUNT_KEY=hc_secret_test_…   # your test key
export CUSTOMER_REF=acme-1               # your ID for the customer
CUSTOMER_ID=$(curl -s "$HARDCOUNT/v1/customers?external_id=$CUSTOMER_REF" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -r '.data[0].id')
export CUSTOMER_ID
echo "$CUSTOMER_ID"
```

`echo` prints the customer's ID, such as `hc_cus_…`. Every example reads
these four variables.

## What each example prints

A line for the event, then one line for each currency the customer has
credit or unbilled usage in. In that line:

- `estimated_remaining_cents` is the credit left: the balance minus the
  usage not invoiced yet, and never below 0;
- `balance_cents` is the credit balance;
- `unbilled_usage_cents` is the usage not invoiced yet.

Amounts are in the currency's smallest unit, such as cents. A retry also
prints a line, such as `429 rate_limited: retrying in 1 s`, to standard
error.

## curl

Paste it into bash or zsh. It needs `curl`, `jq` and `uuidgen`.

```bash
# hc METHOD PATH [BODY]: send one request. Retry it on no answer, 429 or a
# 5xx, with the same body each time. Print the answer's JSON.
hc() {
  local tmp code delay args=()
  tmp=$(mktemp -d)
  [ -n "$3" ] && args=(-H "Content-Type: application/json" -d "$3")
  for attempt in 1 2 3 4 5; do
    : > "$tmp/headers"
    code=$(curl -s -o "$tmp/body" -D "$tmp/headers" -w '%{http_code}' --max-time 35 \
      -X "$1" "$HARDCOUNT$2" -H "Authorization: Bearer $HARDCOUNT_KEY" "${args[@]}")
    case "$code" in
      2??) cat "$tmp/body"; echo; return 0 ;;
      000|429|5??)
        # Retry-After is the number of seconds to wait. Without it, back off.
        delay=$(awk 'tolower($1) == "retry-after:" {print $2 + 0}' "$tmp/headers")
        delay=${delay:-$((2 ** attempt))}
        echo "$code: retrying in $delay s" >&2
        sleep "$delay" ;;
      *) cat "$tmp/body" >&2; echo >&2; return 1 ;;   # fix the request
    esac
  done
  return 1
}

# 1. Send one usage event. Make its key once, before the first attempt.
EVENT=$(jq -n --arg ref "$CUSTOMER_REF" --arg key "$CUSTOMER_REF-$(uuidgen):input" \
  '{event_name: "tokens", external_customer_id: $ref, quantity: 1000,
    dimensions: {provider: "anthropic", model: "claude-sonnet-4.5", token_type: "input"},
    idempotency_key: $key}')
hc POST /v1/usage-events "$EVENT" | jq -c '{id, quantity}'

# 2. Read the credit the customer can spend now.
hc GET "/v1/credits/balance/$CUSTOMER_ID/live" \
  | jq -c '.balances[] | {currency, balance_cents, unbilled_usage_cents, estimated_remaining_cents}'
```

It prints the event's `id` and `quantity`, then the balance fields as JSON.

## Node.js

Save it as `two-calls.mjs` and run `node two-calls.mjs`. It needs Node.js 20
or later, for `fetch`. To use it from TypeScript, add types to `hc`'s
arguments.

```js
// two-calls.mjs — Node.js 20 or later, no packages.
import { randomUUID } from "node:crypto";

const BASE = process.env.HARDCOUNT ?? "http://localhost:8080";
const KEY = process.env.HARDCOUNT_KEY; // hc_secret_test_…

const sleep = (seconds) => new Promise((done) => setTimeout(done, seconds * 1000));

// Send one request. Retry it on no answer, 429 or a 5xx, with the same body
// each time. Return the status and the answer's JSON.
async function hc(method, path, body) {
  for (let attempt = 1; ; attempt++) {
    let res;
    try {
      res = await fetch(BASE + path, {
        method,
        headers: {
          Authorization: `Bearer ${KEY}`,
          ...(body && { "Content-Type": "application/json" }),
        },
        body: body && JSON.stringify(body),
        signal: AbortSignal.timeout(35_000),
      });
    } catch (err) {
      if (attempt === 5) throw err; // no answer: network error or timeout
      await sleep(2 ** attempt);
      continue;
    }
    const json = await res.json().catch(() => ({}));
    if (res.ok) return { status: res.status, json };
    const { code, message } = json.error ?? {};
    if ((res.status !== 429 && res.status < 500) || attempt === 5) {
      throw new Error(`${method} ${path}: ${res.status} ${code}: ${message}`); // fix the request
    }
    // Retry-After is the number of seconds to wait. Without it, back off.
    const delay = Number(res.headers.get("Retry-After")) || 2 ** attempt;
    console.error(`${res.status} ${code}: retrying in ${delay} s`);
    await sleep(delay);
  }
}

// 1. Send one usage event. Make its key once, before the first attempt.
const ref = process.env.CUSTOMER_REF;
const event = {
  event_name: "tokens",
  external_customer_id: ref,
  quantity: 1000,
  dimensions: { provider: "anthropic", model: "claude-sonnet-4.5", token_type: "input" },
  idempotency_key: `${ref}-${randomUUID()}:input`,
};
const sent = await hc("POST", "/v1/usage-events", event);
console.log(sent.status === 201 ? "recorded" : "already recorded", sent.json.id);

// 2. Read the credit the customer can spend now.
const live = await hc("GET", `/v1/credits/balance/${process.env.CUSTOMER_ID}/live`);
for (const b of live.json.balances) {
  console.log(`${b.currency}: ${b.estimated_remaining_cents} left of ${b.balance_cents}, ` +
    `${b.unbilled_usage_cents} used and not yet invoiced (cents)`);
}
```

It prints `recorded` (or `already recorded` if a retry found the first try
had arrived) and the event's ID, then the balance.

## Python

Save it as `two_calls.py` and run `python3 two_calls.py`. It needs Python
3.8 or later, and no packages.

```python
# two_calls.py — Python 3.8 or later, standard library only.
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid

BASE = os.environ.get("HARDCOUNT", "http://localhost:8080")
KEY = os.environ["HARDCOUNT_KEY"]  # hc_secret_test_…


def hc(method, path, body=None):
    """Send one request. Retry it on no answer, 429 or a 5xx, with the same
    body each time. Return the status and the answer's JSON."""
    data = None if body is None else json.dumps(body).encode()
    for attempt in range(1, 6):
        req = urllib.request.Request(BASE + path, data=data, method=method)
        req.add_header("Authorization", "Bearer " + KEY)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=35) as res:
                return res.status, json.load(res)
        except urllib.error.HTTPError as res:
            try:
                error = json.load(res).get("error", {})
            except ValueError:
                error = {}
            code, message = error.get("code"), error.get("message")
            if (res.code != 429 and res.code < 500) or attempt == 5:
                raise RuntimeError(f"{method} {path}: {res.code} {code}: {message}")  # fix the request
            # Retry-After is the number of seconds to wait. Without it, back off.
            delay = int(res.headers.get("Retry-After") or 2 ** attempt)
            print(f"{res.code} {code}: retrying in {delay} s", file=sys.stderr)
        except OSError:  # no answer: network error or timeout
            if attempt == 5:
                raise
            delay = 2 ** attempt
        time.sleep(delay)


# 1. Send one usage event. Make its key once, before the first attempt.
ref = os.environ["CUSTOMER_REF"]
event = {
    "event_name": "tokens",
    "external_customer_id": ref,
    "quantity": 1000,
    "dimensions": {"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "input"},
    "idempotency_key": f"{ref}-{uuid.uuid4()}:input",
}
status, sent = hc("POST", "/v1/usage-events", event)
print("recorded" if status == 201 else "already recorded", sent["id"])

# 2. Read the credit the customer can spend now.
status, live = hc("GET", f"/v1/credits/balance/{os.environ['CUSTOMER_ID']}/live")
for b in live["balances"]:
    print(f"{b['currency']}: {b['estimated_remaining_cents']} left of {b['balance_cents']}, "
          f"{b['unbilled_usage_cents']} used and not yet invoiced (cents)")
```

## Go

Save it as `two_calls.go` and run `go run two_calls.go`.

```go
// two_calls.go — Go 1.21 or later, standard library only.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

var (
	base   = envOr("HARDCOUNT", "http://localhost:8080")
	key    = os.Getenv("HARDCOUNT_KEY") // hc_secret_test_…
	client = &http.Client{Timeout: 35 * time.Second}
)

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// hc sends one request. It retries it on no answer, 429 or a 5xx, with the
// same body each time. It decodes a 2xx answer into out and returns the
// status.
func hc(method, path string, body, out any) (int, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		delay := time.Duration(1<<attempt) * time.Second
		res, err := client.Do(req)
		var raw []byte
		if err == nil {
			raw, err = io.ReadAll(res.Body)
			res.Body.Close()
		}
		if err != nil { // no answer: network error or timeout
			if attempt == 5 {
				return 0, err
			}
			time.Sleep(delay)
			continue
		}
		if res.StatusCode < 300 {
			return res.StatusCode, json.Unmarshal(raw, out)
		}
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if (res.StatusCode != 429 && res.StatusCode < 500) || attempt == 5 {
			return res.StatusCode, fmt.Errorf("%s %s: %d %s: %s", // fix the request
				method, path, res.StatusCode, e.Error.Code, e.Error.Message)
		}
		// Retry-After is the number of seconds to wait. Without it, back off.
		if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil {
			delay = time.Duration(s) * time.Second
		}
		fmt.Fprintf(os.Stderr, "%d %s: retrying in %s\n", res.StatusCode, e.Error.Code, delay)
		time.Sleep(delay)
	}
}

func main() {
	// 1. Send one usage event. Make its key once, before the first attempt.
	ref := os.Getenv("CUSTOMER_REF")
	id := make([]byte, 16)
	rand.Read(id)
	event := map[string]any{
		"event_name":           "tokens",
		"external_customer_id": ref,
		"quantity":             1000,
		"dimensions":           map[string]string{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "input"},
		"idempotency_key":      ref + "-" + hex.EncodeToString(id) + ":input",
	}
	var sent struct {
		ID string `json:"id"`
	}
	status, err := hc("POST", "/v1/usage-events", event, &sent)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if status == http.StatusCreated {
		fmt.Println("recorded", sent.ID)
	} else {
		fmt.Println("already recorded", sent.ID)
	}

	// 2. Read the credit the customer can spend now.
	var live struct {
		Balances []struct {
			Currency                string `json:"currency"`
			BalanceCents            int64  `json:"balance_cents"`
			UnbilledUsageCents      int64  `json:"unbilled_usage_cents"`
			EstimatedRemainingCents int64  `json:"estimated_remaining_cents"`
		} `json:"balances"`
	}
	if _, err := hc("GET", "/v1/credits/balance/"+os.Getenv("CUSTOMER_ID")+"/live", nil, &live); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, b := range live.Balances {
		fmt.Printf("%s: %d left of %d, %d used and not yet invoiced (cents)\n",
			b.Currency, b.EstimatedRemainingCents, b.BalanceCents, b.UnbilledUsageCents)
	}
}
```

## Before you use it in production

- **Keep the event and its key in your own queue until a `2xx` comes back.**
  Then a crash and a restart send the same key again, not a new one.
- **Send many events in one call.** `POST /v1/usage-events/batch` takes up to
  1,000 events. A batch is stored all or nothing, so resend the whole batch
  with the same keys.
- **Do not read the balance on every request your app serves.** It is worked
  out on each read. Poll it on an interval and cache it:
  [Stop usage when credit runs out](../stop-usage-when-credit-runs-out.md).
- **The request time limit is 30 seconds.** The examples wait 35 seconds for
  an answer, so a slow request ends on Hardcount's side first.
