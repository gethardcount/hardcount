# fix(demo): rerunning the demo on a later day no longer collides on the customer id

## What & why

`scripts/demo.sh` set `RUN=$(date +%H%M%S)` and used it in:

- the customer `external_id` (`acme_$RUN`), unique on `(tenant_id, livemode, external_id)` (migration 0020);
- the subscription `code` (`acme-tokens-$RUN`), unique on `(tenant_id, livemode, code)`;
- the LiteLLM call ids (`chatcmpl-$RUN-$i`) and `user` (`acme_$RUN`, which must equal the external id).

A rerun at the same HH:MM:SS on a later day stopped at step 3 with `✗ POST /v1/customers → HTTP 409`. README says "Each run creates a fresh demo customer on its own test clock, so you can rerun it."

Fix: `RUN="$(date -u +%Y%m%d%H%M%S)-$RANDOM"`.

- Full UTC date and time: a time of day repeats every day.
- `$RANDOM`, not `$$`: `./quickstart.sh demo` runs the script in a fresh alpine container, where the PID tends to be the same every run.
- Limits: `external_id` max 255 chars (`acme_` + 21 = 26); subscription `code` must match the slug pattern (letters, digits, `-`, `_`), which it does. LiteLLM call ids have no length check.
- `deploy/compose/quickstart.sh` has no run id of its own; it calls `demo.sh`. No other script uses `%H%M%S` for ids.

### Test

No automated test. The repo has no shell-script test harness. CI's `golden-path` job runs `demo.sh` once against a fresh database, so it cannot see a rerun collision, and making it fail before the fix would need a faked `date` across two runs against a live server.

Evidence instead (scratch script, a `date` shim fixed at 14:30:05Z on two consecutive days):

```
RUN=$(date +%H%M%S) -> day1=143005 day2=143005 COLLIDES
RUN="$(date -u +%Y%m%d%H%M%S)-$RANDOM" -> day1=20261016143005-31841 day2=20261017143005-31357 distinct
```

- `bash -n scripts/demo.sh`: OK. `shellcheck` is not installed here; not run.

### Checks

- `make lint-manual-test`: OK
- No Go or web-v2 touched.

## Checklist

- [ ] Tests cover the new behavior — no automated test (see above)
- [ ] `go test ./... -race -short -count=1` passes — no Go changed; not run
- [ ] `make lint` passes — no Go changed; not run
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (FLOW X14, new `[ ]` box)
- [ ] `make gen` re-run — n/a
- [ ] Frontend touched? — n/a

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
