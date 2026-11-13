package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/usage"
)

// CustomerLookup is the narrow surface the adapter uses to resolve
// LiteLLM's `user` field (= external_customer_id) to a Hardcount internal
// customer_id. Implemented by *customer.PostgresStore.
type CustomerLookup interface {
	GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error)
}

// MeterLookup is the narrow surface the adapter uses to resolve the
// event_name ("tokens_input" / "tokens_output") to a Hardcount internal
// meter_id. Implemented by *pricing.Service.
type MeterLookup interface {
	GetMeterByEventName(ctx context.Context, tenantID, key string) (domain.Meter, error)
}

// Ingester is the narrow surface the adapter calls to persist one call's
// resolved events. Implemented by *usage.Service. It writes the call in ONE
// transaction: a key held and then discarded is not written, a key still
// held is written and marked released (#1044), and a call first recorded
// before the cache-write split is written the old way (see
// usage.PostgresStore.IngestCallResolvingHeld).
type Ingester interface {
	IngestCall(ctx context.Context, tenantID string, in usage.CallInput) (usage.CallResult, error)
}

// Holder keeps a call whose customer or meter does not exist yet, so it is
// not lost (#1044, ADR-122), in one transaction. Implemented by
// *usage.Service. A key already held is counted as deduplicated.
type Holder interface {
	HoldCall(ctx context.Context, tenantID string, in usage.HoldCallInput) (usage.CallResult, error)
}

// Handler exposes POST /v1/integrations/litellm/spend. Auth is a Bearer
// API key holding usage:ingest: the operator creates a usage-only key
// (ADR-133) and puts it in the proxy's callback headers as
// `Authorization: Bearer <hc_ingest_…>`. A secret key works too.
type Handler struct {
	customers CustomerLookup
	meters    MeterLookup
	ingester  Ingester
	holder    Holder
}

// New constructs the LiteLLM adapter handler. All four deps are
// required — adapter does no useful work without persistence.
func New(customers CustomerLookup, meters MeterLookup, ingester Ingester, holder Holder) *Handler {
	return &Handler{
		customers: customers,
		meters:    meters,
		ingester:  ingester,
		holder:    holder,
	}
}

// Routes returns the chi router for /v1/integrations/litellm/*.
// Mounted by api/router.go under the standard /v1 auth-required
// stack — LiteLLM's generic_api callback sets the Bearer header on
// every POST, so the Hardcount auth middleware enforces the tenant.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/spend", h.spend)
	return r
}

// SpendResponse is the wire shape returned by POST /spend. Mirrors
// the existing /v1/usage-events/batch response so partners using
// both surfaces see one shape:
//
//	{ "accepted": N, "deduplicated": D, "held": H, "skipped": M, "cache_hits": C,
//	  "after_close": A, "no_price": P, "not_on_plan": O, "outside_subscription": S,
//	  "errors": [{ "id": "...", "error": "..." }] }
//
// after_close, no_price, not_on_plan and outside_subscription count accepted
// rows by the billing_status a single ingest would
// answer them with (usage open_period.go). They are stored all the same. A
// replay never counts.
//
// held counts rows whose customer or meter does not exist yet. They are
// kept in usage_held_events, not billed, and shown to the operator, who
// releases them once the customer exists (#1044, ADR-122). Before this they
// were listed in errors[] and lost: LiteLLM reads a 200 as delivered.
//
// accepted counts rows the store NEWLY recorded. Deduplicated counts rows
// it already held — a success, since LiteLLM redelivering a batch is the
// happy path, but not new usage. They were previously summed into
// accepted, so a pure replay reported the same number as the original
// delivery and an operator reconciling spend could not tell a retry from
// fresh consumption. /v1/usage-events/batch already separated the two.
//
// skipped counts CALLS that bill nothing: a call type on the skip list
// (image generation, moderation, ...), a call with zero tokens, a failed
// call, and a LiteLLM response-cache hit.
// cache_hits counts the cache hits among them, so an operator can
// reconcile against LiteLLM's own spend, which counts a hit as $0.
type SpendResponse struct {
	Accepted     int `json:"accepted"`
	Deduplicated int `json:"deduplicated"`
	Held         int `json:"held"`
	Skipped      int `json:"skipped"`
	CacheHits    int `json:"cache_hits"`
	usage.UnbilledCounts
	Errors []SpendRowError `json:"errors,omitempty"`
}

// SpendRowError carries the per-payload reason a particular row was
// rejected. The adapter is intentionally permissive — one bad row
// doesn't fail the whole batch. A per-row errors[] entry inside a 200
// keeps the rest of the batch: LiteLLM reads any 2xx as delivered.
// LiteLLM 1.83.9 never sends a batch again once Hardcount has answered;
// 1.84.0 and later send it again, when max_retries is set, only after a
// timeout, a connection error or a 5xx — never after a 4xx. A row-level verdict answered as a 4xx or
// 5xx would therefore lose the whole batch or resend it to no end.
type SpendRowError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// spend handles the LiteLLM generic_api callback. Accepts each of
// LiteLLM's log formats — a JSON array (json_array, the default), records
// one a line (ndjson), one record (single) — and an operator buffer's
// {"events": [...]} (decodeBody).
//
// Flow:
//  1. Decode body; normalize every shape to []StandardLoggingPayload
//  2. For each payload: MapPayload → the call's events, or a skip
//  3. Resolve external_customer_id + event_name once for the call
//  4. Write the call in one transaction (usage.Service.IngestCall, or
//     HoldCall when the customer or meter does not exist yet), idempotent
//     via the usage_events UNIQUE (tenant_id, livemode, idempotency_key) —
//     tenant-wide, NOT per customer/meter, which is why the mapper
//     suffixes the key per token type
//  5. Tally accepted / deduplicated / held / skipped / errors; return 200
//
// The status code answers ONE question: did Hardcount reach a verdict on
// this batch? Per-row verdicts — a customer or meter that does not exist
// yet (the row is HELD, counted in held, #1044), a payload that fails
// validation, a row the store already has — stay 200 with held / errors[]
// / deduplicated, so one misconfigured call never fails the batch and
// never makes LiteLLM retry it. A
// NON-verdict — the usage store could not be reached or could not
// commit (managed-Postgres failover, a replica losing its pool during
// a rolling restart, a stalled connection) — aborts the batch fail-fast
// with 503 api_error/ingest_unavailable so LiteLLM's retry (configure
// max_retries > 0; docs/integrations/litellm.md) re-sends it. Before
// 2026-08-30 this path returned 200 with the failure filed as a
// customer-not-found row; LiteLLM's generic logger treats any 2xx as
// delivered and clears its queue, so every completion in the outage
// window was silently lost (ADR-033 amendment 2026-08-30). Whole-batch
// retry is safe by construction: every row is idempotency-keyed and the
// store dedups on (tenant_id, livemode, idempotency_key), so rows
// committed before the abort come back as deduplicated.
func (h *Handler) spend(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	body, err := decodeBody(r)
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			// The cap fires mid-array on valid JSON, so this is "split
			// the batch", not "malformed JSON" (the /v1/usage-events/batch
			// answer for the same case). Nothing was saved.
			respond.Error(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "batch_too_large",
				fmt.Sprintf("the request body is over the %d MB limit, so the batch was not saved — send fewer records per batch (LiteLLM: lower DEFAULT_BATCH_SIZE)", tooBig.Limit>>20))
		case errors.Is(err, errEmptyBody):
			respond.BadRequest(w, r, "empty body")
		default:
			// A bad record names its line, so the operator can find it in
			// what the proxy sent. Nothing of the batch was saved.
			var bad *recordError
			if errors.As(err, &bad) {
				respond.BadRequest(w, r, fmt.Sprintf("invalid JSON body at line %d (record %d); the batch was not saved", bad.line, bad.record))
				return
			}
			respond.BadRequest(w, r, "invalid JSON body")
		}
		return
	}

	resp := SpendResponse{Errors: []SpendRowError{}}
	// One open-period read per customer for the whole request.
	ctx := usage.WithPeriodCache(r.Context())

	for _, payload := range body {
		m, err := MapPayload(payload)
		if err != nil {
			resp.Errors = append(resp.Errors, SpendRowError{
				ID:    payload.ID,
				Error: err.Error(),
			})
			continue
		}
		if m.Skip == SkipCacheHit {
			resp.CacheHits++
		}
		if len(m.Events) == 0 {
			// A call that bills nothing: a call type on the skip list (image
			// generation, moderation, ...), zero tokens, a failed call, or a
			// LiteLLM response-cache hit. Counted as skipped — not rejected — so
			// the operator's spend dashboard reconciles.
			resp.Skipped++
			continue
		}
		res, err := h.persist(ctx, tenantID, m)
		switch {
		case err == nil:
			if res.held {
				resp.Held += res.Inserted
			} else {
				resp.Accepted += res.Inserted
				resp.UnbilledCounts = resp.Plus(res.Unbilled)
			}
			// Rows the store already had, or already held. Still a
			// success — LiteLLM retrying a delivered batch is the happy
			// path — but counting them as accepted told an operator
			// reconciling spend that a replay had recorded fresh usage.
			resp.Deduplicated += res.Deduplicated
		case errors.Is(err, errIngestUnavailable):
			// Non-verdict: abort the whole batch for client retry.
			// The raw error is logged, never sent to the caller (ADR-026).
			slog.ErrorContext(r.Context(), "litellm spend: usage store unavailable — batch aborted for client retry",
				"tenant_id", tenantID,
				"accepted", resp.Accepted,
				"deduplicated", resp.Deduplicated,
				"error", err,
			)
			respond.Error(w, r, http.StatusServiceUnavailable, "api_error", "ingest_unavailable",
				"usage store temporarily unavailable — retry the whole batch; rows already recorded replay as deduplicated")
			return
		default:
			resp.Errors = append(resp.Errors, SpendRowError{
				ID:    payload.ID,
				Error: err.Error(),
			})
		}
	}

	if len(resp.Errors) > 0 || resp.Held > 0 {
		slog.WarnContext(r.Context(), "litellm spend: rows held or refused",
			"tenant_id", tenantID,
			"accepted", resp.Accepted,
			"deduplicated", resp.Deduplicated,
			"held", resp.Held,
			"skipped", resp.Skipped,
			"errors", len(resp.Errors),
		)
	}
	respond.JSON(w, r, http.StatusOK, resp)
}

// callOutcome is what persist did with one call.
type callOutcome struct {
	usage.CallResult
	// held is true when the call's rows were held, not ingested.
	held bool
}

// persist resolves the call's customer and meter once and writes all its
// events in one transaction. Every event of a call has the same customer
// and the same `tokens` meter, so a call is ingested whole or held whole.
// It classifies every error into a VERDICT (returned as a per-row error:
// a validation failure) or a NON-VERDICT (wrapped in errIngestUnavailable:
// the store could not be reached or could not commit). Both lookup stores
// return errs.ErrNotFound only on sql.ErrNoRows and the raw driver error
// otherwise, which is what makes this split honest — a BeginTx failure
// used to be reported as "customer not found".
func (h *Handler) persist(ctx context.Context, tenantID string, m Mapping) (callOutcome, error) {
	first := m.Events[0]
	cust, err := h.customers.GetByExternalID(ctx, tenantID, first.ExternalCustomerID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return h.hold(ctx, tenantID, m, domain.HeldReasonCustomerNotFound)
		}
		return callOutcome{}, fmt.Errorf("%w: customer lookup: %w", errIngestUnavailable, err)
	}
	meter, err := h.meters.GetMeterByEventName(ctx, tenantID, first.EventName)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return h.hold(ctx, tenantID, m, domain.HeldReasonMeterNotFound)
		}
		return callOutcome{}, fmt.Errorf("%w: meter lookup: %w", errIngestUnavailable, err)
	}

	res, err := h.ingester.IngestCall(ctx, tenantID, usage.CallInput{
		CustomerID: cust.ID,
		MeterID:    meter.ID,
		Events:     callEvents(m.Events),
		Legacy:     callEvents(m.Legacy),
		LegacyKey:  m.LegacyKey,
	})
	if err != nil {
		// Verdicts on the call itself stay per-row: a bad payload must
		// never 503, or LiteLLM retries it until max_retries is spent.
		// Classified on Kind sentinels only — a DomainError's Code is not
		// a verdict signal (a future infra error carrying a code would be
		// misfiled as per-row). A failed open-period or aggregation read is
		// neither, so it is a non-verdict: 503, nothing written.
		if errors.Is(err, errs.ErrValidation) || errors.Is(err, errs.ErrNotFound) {
			return callOutcome{}, err
		}
		return callOutcome{}, fmt.Errorf("%w: ingest: %w", errIngestUnavailable, err)
	}
	return callOutcome{CallResult: res}, nil
}

// hold keeps a call whose customer or meter does not exist yet (#1044,
// ADR-122). The rows keep their idempotency keys, so a release later writes
// each at most once, and a LiteLLM resend of a call already held is counted
// as deduplicated. A hold that cannot be written is a NON-verdict: 503, so
// LiteLLM resends the batch rather than reading a 200 as delivered.
func (h *Handler) hold(ctx context.Context, tenantID string, m Mapping, reason domain.HeldUsageReason) (callOutcome, error) {
	first := m.Events[0]
	res, err := h.holder.HoldCall(ctx, tenantID, usage.HoldCallInput{
		Source:             domain.HeldUsageSourceLiteLLM,
		ExternalCustomerID: first.ExternalCustomerID,
		EventName:          first.EventName,
		Reason:             reason,
		Events:             callEvents(m.Events),
		Legacy:             callEvents(m.Legacy),
		LegacyKey:          m.LegacyKey,
	})
	switch {
	case err == nil:
		return callOutcome{CallResult: res, held: true}, nil
	case errors.Is(err, errs.ErrValidation):
		// The call itself is bad; holding it would only fail later.
		return callOutcome{}, err
	default:
		return callOutcome{}, fmt.Errorf("%w: hold: %w", errIngestUnavailable, err)
	}
}

// callEvents drops the per-event customer and meter, which persist resolved
// once for the call.
func callEvents(in []ExternalIngest) []usage.CallEvent {
	if in == nil {
		return nil
	}
	out := make([]usage.CallEvent, 0, len(in))
	for _, e := range in {
		out = append(out, usage.CallEvent{
			Quantity:       e.Quantity,
			Dimensions:     e.Dimensions,
			IdempotencyKey: e.IdempotencyKey,
			Timestamp:      e.Timestamp,
			// ADR-079 D4: the provider's own per-role cost, already
			// filtered by observedCostMicros to the roles D4 allows. nil
			// leaves rate-table inference in charge.
			ObservedCostMicros: e.ObservedCostMicros,
		})
	}
	return out
}

// errIngestUnavailable marks a NON-verdict: the usage store could not be
// reached or could not commit. spend aborts the batch with 503 so the
// client retries it; never returned to the caller as text.
var errIngestUnavailable = errors.New("litellm: usage store unavailable")

// MaxBodyBytes is the request-body cap for POST /v1/integrations/litellm/spend:
// 32 MiB, where every other route gets 1 MB (api/router.go limitRequestBody).
//
// Why this route needs its own: LiteLLM's generic_api logger sends a JSON
// array of up to DEFAULT_BATCH_SIZE records (512 in 1.83.9 and 1.84.0), or
// what it queued in DEFAULT_FLUSH_INTERVAL_SECONDS (5). A 1.83.9 record is 7
// to 9 KB even for a one-word prompt, and it carries the call's messages and
// reply, so it grows with them. A full default batch is about 4 MB. LiteLLM
// never resends a batch Hardcount answers with a 4xx, so the 1 MB cap lost
// every whole batch from a proxy busier than about 25 to 30 calls a second.
//
// Why 32 MiB: it holds a full default batch whose records average 64 KB —
// seven times the short-prompt record, room for long prompts — and it reads in
// about 3 s at 100 Mbit/s, inside the server's 30 s ReadTimeout. Memory does
// not grow with the cap: decodeBody streams the body one record at a time,
// and a record keeps only the fields the adapter reads, so its messages and
// response — the part that grows with the prompt — are never held. What a
// request holds grows with its record count (about 5 KB a record, mostly
// metadata), not with its bytes. Measured on a 4.3 MB batch of 512 records:
// 2.7 MB held once decoded and 4.2 MB allocated, where reading the body
// whole first allocated 14.2 MB.
const MaxBodyBytes = 32 << 20

// errEmptyBody is decodeBody's answer to a request with no body.
var errEmptyBody = errors.New("empty body")

// decodeBody normalizes the request body into a slice of payloads.
// Accepts:
//   - Bare array: [payload, payload, ...] — what LiteLLM's generic_api
//     logger sends (log_format json_array, its default)
//   - NDJSON: payloads one after another, one a line — log_format ndjson,
//     any number of records. A record split over lines reads too.
//   - LiteLLM's single-record shape: one payload as the top-level object
//     (log_format single), the one-record case of the above
//   - Batched shape: { "events": [...] } (used by some operator-side buffers);
//     a top-level object whose "events" array is not empty gives its events
//
// The body is decoded one record at a time from the stream, so its bytes are
// never held whole (see MaxBodyBytes). Any error leaves nothing decoded: the
// handler saves no row from a body it could not read to the end, and a body
// over the router's cap fails here with its *http.MaxBytesError. A record
// that is not valid JSON, or not a record, fails with a *recordError naming
// it and its line.
func decodeBody(r *http.Request) ([]StandardLoggingPayload, error) {
	defer func() { _ = r.Body.Close() }()

	lines := &lineCounter{r: r.Body}
	first, err := firstNonSpace(lines)
	if errors.Is(err, io.EOF) {
		return nil, errEmptyBody
	}
	if err != nil {
		return nil, err
	}
	// The decoder reads the first byte back, then the rest of the body
	// through the counter; nothing else buffers it.
	d := &bodyDecoder{lines: lines, start: lines.read - 1, dec: json.NewDecoder(io.MultiReader(bytes.NewReader([]byte{first}), lines))}
	if first == '[' {
		return d.array()
	}
	return d.objects()
}

// bodyDecoder reads a spend body record by record, and can tell which line a
// record that fails to decode is on.
type bodyDecoder struct {
	lines *lineCounter
	start int64 // the body offset of the decoder's first byte
	dec   *json.Decoder
}

// array reads a JSON array of payloads one element at a time. The decoder's
// buffer holds about one record, not the whole array.
func (d *bodyDecoder) array() ([]StandardLoggingPayload, error) {
	if _, err := d.dec.Token(); err != nil { // the opening [
		return nil, err
	}
	batch := []StandardLoggingPayload{}
	for record := 1; d.dec.More(); record++ {
		var p StandardLoggingPayload
		if err := d.dec.Decode(&p); err != nil {
			return nil, d.recordFailed(record, err)
		}
		batch = append(batch, p)
	}
	if _, err := d.dec.Token(); err != nil { // the closing ]
		return nil, err
	}
	if err := expectEnd(d.dec); err != nil {
		return nil, err
	}
	return batch, nil
}

// objects reads top-level JSON objects one after another until the body
// ends: NDJSON, or a single record, or an events wrapper.
func (d *bodyDecoder) objects() ([]StandardLoggingPayload, error) {
	batch := []StandardLoggingPayload{}
	for record := 1; ; record++ {
		// A single payload or { "events": [...] } share one decode.
		var obj struct {
			StandardLoggingPayload
			Events []StandardLoggingPayload `json:"events"`
		}
		if err := d.dec.Decode(&obj); err != nil {
			return nil, d.recordFailed(record, err)
		}
		if len(obj.Events) > 0 {
			batch = append(batch, obj.Events...)
		} else {
			batch = append(batch, obj.StandardLoggingPayload)
		}
		if !d.dec.More() {
			break
		}
	}
	// More is false at the end of the body, and also before a stray ] or }:
	// expectEnd tells the two apart.
	if err := expectEnd(d.dec); err != nil {
		return nil, err
	}
	return batch, nil
}

// recordError is a record decodeBody could not read: its place in the body
// (the first record is 1) and the line the fault is on.
type recordError struct {
	record, line int
	err          error
}

func (e *recordError) Error() string {
	return fmt.Sprintf("record %d, line %d: %v", e.record, e.line, e.err)
}

func (e *recordError) Unwrap() error { return e.err }

// recordFailed wraps a record's decode error with the line it is on. The
// line is counted from the newlines read so far, less those in the
// decoder's buffer not yet decoded. A syntax error is placed at its
// offending byte: the error's Offset counts the bytes the decoder read, up
// to and including that byte. A truncated record is placed at the body's
// end, and a record whose JSON parsed but did not fit (a string id given as
// a number) on the line it ends on. A body over the cap keeps its
// *http.MaxBytesError, which the handler answers with 413.
func (d *bodyDecoder) recordFailed(record int, err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return err
	}
	rest, _ := io.ReadAll(d.dec.Buffered())
	decoded := d.lines.newlines - bytes.Count(rest, newline)
	upto := 0
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &syntax):
		// The offending byte, counted from where rest begins in the body.
		at := d.start + syntax.Offset - 1 - (d.lines.read - int64(len(rest)))
		if at >= 0 && at <= int64(len(rest)) {
			upto = int(at) // the bytes before the offending one
		}
	case errors.Is(err, io.ErrUnexpectedEOF):
		upto = len(rest)
	}
	return &recordError{record: record, line: 1 + decoded + bytes.Count(rest[:upto], newline), err: err}
}

var newline = []byte{'\n'}

// lineCounter counts the bytes and newlines read through it.
type lineCounter struct {
	r        io.Reader
	read     int64
	newlines int
}

func (c *lineCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	c.newlines += bytes.Count(p[:n], newline)
	return n, err
}

// expectEnd refuses anything after the body's one JSON value, as
// json.Unmarshal does.
func expectEnd(dec *json.Decoder) error {
	_, err := dec.Token()
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return errors.New("trailing data after the JSON value")
	default:
		return err
	}
}

// firstNonSpace reads the body up to its first byte that is not JSON
// whitespace and returns that byte. It reads one byte at a time, so nothing
// past it is taken from the body.
func firstNonSpace(r io.Reader) (byte, error) {
	var b [1]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return b[0], nil
	}
}
