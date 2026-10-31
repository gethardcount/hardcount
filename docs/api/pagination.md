# Pagination: reading long lists

A list route answers one page of rows at a time. You pick the page with
`limit` and `offset`, and the answer says how many rows there are in all.
Every list route works this way, except the few in
[Lists that answer in one piece](#lists-that-answer-in-one-piece).

## Ask for a page

- `limit` is how many rows to return: 1 to 100. Without it you get 50.
- `offset` is how many rows to skip first: 0 or more. Without it, 0.

```bash
curl -s "$HARDCOUNT/v1/plans?limit=100&offset=100" \
  -H "Authorization: Bearer $HARDCOUNT_KEY"
```

The answer holds plans 101 to 200, in this shape:

```json
{
  "data": [{ "id": "hc_pln_…", "…": "…" }],
  "total": 230
}
```

- `data` is the page's rows. An empty page is `[]`, never `null`.
- `total` counts every row that matches your filters, not only this page.

## Read everything

Add 100 to `offset` until `offset` reaches `total`:

```bash
offset=0
while :; do
  page=$(curl -s "$HARDCOUNT/v1/plans?limit=100&offset=$offset" \
    -H "Authorization: Bearer $HARDCOUNT_KEY")
  echo "$page" | jq -c '.data[]'
  offset=$((offset + 100))
  [ "$offset" -lt "$(echo "$page" | jq .total)" ] || break
done
```

A row added or removed while you read can shift the pages, so a row can be
skipped or read twice. Match rows by `id` if that matters.

## Values Hardcount refuses

Hardcount never rounds a bad value to a valid one. Each of these is a `422`
with `param` naming the parameter:

- a `limit` below 1 or above 100;
- a negative `offset`;
- a `limit` or `offset` that is not a whole number, or that is sent twice;
- `after`: there is no cursor. Page with `offset`.

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "validation_error",
    "message": "limit must be a whole number from 1 to 100",
    "request_id": "req_…",
    "param": "limit"
  }
}
```

## Lists that stop counting

- The audit log's `total` stops at 10,001. A value above 10,000 means "more
  than 10,000".
- The customer list with `search`, or sorted by name or email, looks at
  5,000 customers at most. Its rows and its `total` stop there.

## Lists that answer in one piece

These routes answer a set of rows in one response. They have no `total`, and
`limit`, `offset` and `after` change nothing:

- `GET /v1/usage-events/held` lists the 200 external customers whose held
  usage arrived last. `total_customers` and `total_rows` count all of it.
  Release or discard held usage to see the rest.
- `GET /v1/subscriptions/{id}/timeline` and
  `GET /v1/invoices/{id}/payment-timeline` can leave rows out. They then
  answer `truncated: true`.
- `GET /v1/webhook_events/{id}/deliveries`, `GET /v1/members`,
  `GET /v1/credits/balance/{customer_id}` and
  `GET /v1/credits/grants/{customer_id}` answer every row.
