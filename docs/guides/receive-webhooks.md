# Receive webhooks

A webhook is a message Hardcount sends to a URL in your app when something
happens, such as an invoice paid or a payment failed. This guide takes you
from a test receiver on your computer to one you can run in production. You
check the signature, handle each event once, keep test and live apart, and
resend what you missed.

The reference for messages, signatures, endpoints and retries is
[Webhooks](../webhooks.md). Every event and its data:
[Webhook events](../api/webhook-events.md).

## Receive your first event

You run a small test receiver, register it with Hardcount, make something
happen, and check the message.

**Before you start:** Hardcount is running, and you have a test API key (see
[API keys](../api-keys.md)). Open a terminal in the `hardcount` folder and set:

```bash
export HARDCOUNT=http://localhost:8080
export HARDCOUNT_KEY=hc_secret_test_…   # your test key
```

1. **Start the test receiver.** It answers every message with `200` and
   writes each one to a file.

   ```bash
   python3 scripts/dev/webhook-sink.py /tmp/hardcount-webhooks.jsonl
   ```

   **You see:** `webhook sink on 127.0.0.1:9099 -> /tmp/hardcount-webhooks.jsonl (paths under /fail return 500)`

2. **Pick the address Hardcount will call.** It depends on how you run Hardcount.

   - **From source** (`make dev`): use `http://localhost:9099/ok`.
   - **In Docker** (the [quick start](../quick-start.md) or a server): inside
     its container, `localhost` is the container itself, and Hardcount refuses
     private network addresses, so it cannot reach the receiver directly.
     In a second terminal, start a tunnel, which gives your computer a
     public `https://` address. For example:
     `cloudflared tunnel --url http://localhost:9099`. Use the `https://`
     address it prints, plus `/ok`.

   ```bash
   export HOOK_URL=http://localhost:9099/ok   # or https://<tunnel address>/ok
   ```

   **You see:** nothing yet. The address is saved in `HOOK_URL`.

3. **Register the endpoint.** `"*"` asks for every event.

   ```bash
   jq -n --arg url "$HOOK_URL" '{url: $url, events: ["*"]}' |
     curl -s -X POST "$HARDCOUNT/v1/webhook-endpoints/endpoints" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @-
   ```

   **You see:** JSON with `endpoint.id` (`hc_whe_…`) and `secret`
   (`whsec_…`). The secret is shown only this once. Save it:
   `export WHSEC=whsec_…`.

4. **Make something happen.** Run the demo again. It creates a
   subscription and a month of usage, then bills it.
   - Docker quick start: `cd deploy/compose && ./quickstart.sh demo`
   - From source: `./scripts/demo.sh $HARDCOUNT_KEY`

   Then look at the file:

   ```bash
   tail -n 3 /tmp/hardcount-webhooks.jsonl
   ```

   **You see:** one JSON line per message, each with `"path": "/ok"`, a
   `"sig"` such as `t=…,v1=…`, and a `"body"` whose `event_type` is, for
   example, `subscription.created` or `invoice.finalized`.

5. **Check the signature.** This proves the message came from Hardcount.

   ```bash
   head -n 1 /tmp/hardcount-webhooks.jsonl | python3 -c '
   import hashlib, hmac, json, os, sys
   r = json.loads(sys.stdin.readline())
   p = dict(x.split("=", 1) for x in r["sig"].split(","))
   mac = hmac.new(os.environ["WHSEC"].encode(), (p["t"] + "." + r["body"]).encode(), hashlib.sha256)
   print(mac.hexdigest() == p["v1"])'
   ```

   **You see:** `True`

The dashboard shows the same messages: open **Webhooks**, then your
endpoint, to see each delivery and its result.

To test failures, register an address whose path starts with `/fail`, such
as `http://localhost:9099/fail`. The receiver answers `500`, so Hardcount
retries on its [schedule](../webhooks.md#retries).

## Check the signature in your code

Every message has a `Hardcount-Signature` header such as
`t=1793432193,v1=0d7f…`. Check it before you trust the body:

- Use the **raw request bytes**. Do not parse and re-serialize the JSON
  first.
- Use the **full secret** as the key, `whsec_` included.
- Accept the message if **any** `v1=` matches. There are two while a secret
  is being rotated.
- Refuse a `t=` far from your clock. These examples allow 5 minutes either
  way.

How the signature is made: [Verifying signatures](../webhooks.md#verifying-signatures).

Node:

```js
const crypto = require("crypto");

function verifyHardcountSignature(rawBody, header, secret, toleranceSec = 300) {
  const t = header.match(/(?:^|,)t=(\d+)/)?.[1];
  // Collect every v1= entry: there are two during a secret rotation.
  const sigs = [...header.matchAll(/v1=([0-9a-f]+)/g)].map(m => m[1]);
  if (!t || sigs.length === 0) return false;
  if (Math.abs(Date.now() / 1000 - Number(t)) > toleranceSec) return false;
  const expected = crypto
    .createHmac("sha256", secret)
    .update(`${t}.${rawBody}`)
    .digest("hex");
  return sigs.some(s =>
    s.length === expected.length &&
    crypto.timingSafeEqual(Buffer.from(s), Buffer.from(expected)),
  );
}
```

Python:

```python
import hashlib, hmac, re, time

def verify_hardcount_signature(raw_body: bytes, header: str, secret: str, tolerance: int = 300) -> bool:
    m = re.search(r"(?:^|,)t=(\d+)", header)
    sigs = re.findall(r"v1=([0-9a-f]+)", header)
    if not m or not sigs:
        return False
    t = int(m.group(1))
    if abs(time.time() - t) > tolerance:
        return False
    expected = hmac.new(secret.encode(), f"{t}.".encode() + raw_body, hashlib.sha256).hexdigest()
    return any(hmac.compare_digest(s, expected) for s in sigs)
```

Go:

```go
import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// verifyHardcountSignature reports whether header is a valid
// Hardcount-Signature for rawBody, made within tolerance of now.
func verifyHardcountSignature(rawBody []byte, header, secret string, tolerance time.Duration) bool {
	var t string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		key, value, _ := strings.Cut(part, "=")
		switch key {
		case "t":
			t = value
		case "v1": // two of them while a secret is being rotated
			sigs = append(sigs, value)
		}
	}
	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil || len(sigs) == 0 {
		return false
	}
	if age := time.Since(time.Unix(unix, 0)); age > tolerance || age < -tolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range sigs {
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}
	return false
}
```

In a Go handler, read the body with `io.ReadAll(r.Body)` first, pass those
bytes to `verifyHardcountSignature(body, r.Header.Get("Hardcount-Signature"),
secret, 5*time.Minute)`, and decode the JSON only if it returns `true`.

Answer a message that fails the check with `400`. Hardcount treats any
answer that is not `2xx` as a failure and retries it, so a real message
signed with a secret you have not deployed yet arrives again later.

## Handle each event once

Hardcount sends each event at least once, so your receiver must be safe to
run twice. Follow the
[three rules for your receiver](../webhooks.md#three-rules-for-your-receiver).
In code that means:

1. **Answer fast.** Hardcount waits 10 seconds for a `2xx`. Save the message
   and answer, then do slow work, such as sending an email, afterwards.
2. **Skip an event you have handled.** After you handle an event, store its
   `id`. When a message arrives with an `id` you stored, answer `200` and do
   nothing.
3. **Do not trust the order.** To know an invoice's or a subscription's
   state now, read it from the API, such as `GET /v1/invoices/{id}`.
4. **Accept event types you do not handle.** Answer them with `200`. New
   types can appear in any release.

## Test mode and live mode

**An endpoint belongs to one mode.** An endpoint registered with a test key
gets only test events; one registered with a live key gets only live
events. The message itself does not say which mode it came from.

To receive both, register two endpoints:

1. Register your test URL with a test key (`hc_secret_test_…`), as in step 3
   above.
2. Switch to a live key (`hc_secret_live_…`) and register your live URL the
   same way.
   **You see:** a second `endpoint.id` and a second `secret`.

Each endpoint has its own secret. Give each mode its own URL, such as
`/hardcount/webhooks/test` and `/hardcount/webhooks/live`, and check each
with its own secret. In the dashboard, the **Webhooks** page shows the
endpoints of the mode the dashboard is in.

## Resend what your endpoint missed

A delivery that fails 6 times is marked failed, about 26 hours 36 minutes
after the first try. Once your endpoint works again, send the missed events
again. A resend carries the event's original `id`, so a receiver that skips
handled ids handles each event once.

- **One endpoint:** list its deliveries, and resend each failed one to that
  endpoint only:

  ```bash
  ENDPOINT_ID=hc_whe_…   # your endpoint
  curl -s "$HARDCOUNT/v1/webhook-endpoints/endpoints/$ENDPOINT_ID/deliveries?limit=100" \
    -H "Authorization: Bearer $HARDCOUNT_KEY" \
    | jq -r '.data[] | select(.status == "failed") | .id' \
    | while read -r DELIVERY_ID; do
        curl -s -X POST \
          "$HARDCOUNT/v1/webhook-endpoints/endpoints/$ENDPOINT_ID/deliveries/$DELIVERY_ID/replay" \
          -H "Authorization: Bearer $HARDCOUNT_KEY"
        echo
      done
  ```

  **You see:** one line per resend, with the new `delivery_id` and the
  `event_id` of the resend. A paused endpoint (`active: false`) is refused
  with `409`: resume it first.

- **Every endpoint that wants the event:**
  `POST /v1/webhook_events/{id}/replay`. It also reaches endpoints that
  already got it, so prefer the one-endpoint resend.
- **In the dashboard:** open **Webhooks**, then the endpoint, then
  **Resend** on a delivery.

## Rotate the secret

Rotate the secret when it may have leaked, or on a schedule.

1. Call `POST /v1/webhook-endpoints/endpoints/{id}/rotate-secret`.
   **You see:** the new secret. For the next 72 hours, Hardcount signs each
   message with both the old and the new secret.
2. Put the new secret in your receiver within those 72 hours.
   **You see:** messages keep passing your check, since it accepts any
   matching `v1=`.

If the old secret leaked, put the new one in your receiver at once and stop
accepting the old one. Messages keep passing, because each one also carries
a signature made with the new secret. Rotating again within the 72 hours
drops the older of the two secrets at once.

Changing an endpoint with `PATCH` (its URL, events, or whether it is active)
never changes the secret.
