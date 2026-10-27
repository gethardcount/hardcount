package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// CustomerResolver looks up a customer by external ID.
type CustomerResolver interface {
	GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error)
}

// MeterResolver looks up a meter by the event_name its events send.
type MeterResolver interface {
	GetMeterByEventName(ctx context.Context, tenantID, eventName string) (domain.Meter, error)
}

// MeterLister lists the tenant's meters. The aggregate endpoint reads each
// meter's aggregation from them (see BillingModes) so a Maximum meter reports
// its peak, not a sum of readings. Wired in router.go (pricing.Service).
type MeterLister interface {
	ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error)
}

type Handler struct {
	svc        *Service
	customers  CustomerResolver
	meters     MeterResolver
	meterModes MeterLister
	// Held-usage routes only (#1044); see SetHeldDeps.
	heldCustomers CustomerLookup
}

// SetMeterLister wires the meter list the aggregate endpoint reads modes
// from. Unwired, every by_meter value is the plain sum with an empty
// aggregation.
func (h *Handler) SetMeterLister(l MeterLister) { h.meterModes = l }

func NewHandler(svc *Service, customers CustomerResolver, meters MeterResolver) *Handler {
	return &Handler{svc: svc, customers: customers, meters: meters}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.ingest)
	r.Post("/batch", h.batchIngest)
	r.Get("/", h.list)
	// Server-side aggregate that powers the /usage page's stat cards +
	// "Usage by Meter" breakdown. Mounted as a sibling of GET / so chi
	// picks the more-specific pattern; reuses parseListFilter so filter
	// semantics match the list query exactly. See internal/usage/store.go
	// (Aggregate type) for the response shape.
	r.Get("/aggregate", h.aggregate)
	// Held usage (#1044, ADR-122): rows the LiteLLM callback could not
	// ingest because their customer or meter did not exist. GET needs
	// usage read, the POSTs usage write (RequireMethod in router.go).
	r.Get("/held", h.listHeld)
	r.Post("/held/release", h.releaseHeld)
	r.Post("/held/discard", h.discardHeld)
	return r
}

// Backfill is exported so the router can mount it behind PermUsageWrite —
// the /usage-events subtree uses PermUsageRead for historical reasons, but
// backfill is a sensitive ledger operation and should not be reachable by
// read-only keys.
func (h *Handler) Backfill(w http.ResponseWriter, r *http.Request) {
	h.backfill(w, r)
}

// APIEvent is the public API input — developers send external identifiers.
//
// Quantity is kept raw so parseQuantity can answer every bad value with a 422
// naming the field (a decode error would be a 400 for the whole body, and a
// whole batch). The response side serializes it as a decimal string.
//
// Properties is not a field. It was the old name of Dimensions, removed
// before 0.9.0, and is decoded only so an event that still sends it with a
// key is refused: stored without its dimensions it would be priced by the
// wrong rule, silently. A null or empty properties is taken as absent.
type APIEvent struct {
	ExternalCustomerID string           `json:"external_customer_id"`
	EventName          string           `json:"event_name"`
	Quantity           json.RawMessage  `json:"quantity,omitempty"`
	Dimensions         map[string]any   `json:"dimensions,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key,omitempty"`
	Timestamp          *json.RawMessage `json:"timestamp,omitempty"`
	Properties         json.RawMessage  `json:"properties,omitempty"`
}

// hasProperties reports whether an event sent a properties value that could
// carry dimensions. null and {} carry none (null decodes to an empty map), so
// they are taken as if absent: clients generated from the old spec may send
// either for an unset field. Any other value is refused.
func hasProperties(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) != nil || len(obj) > 0
}

// parseQuantity reads the event's quantity the way the spec states it: a JSON
// number, or a string holding one, and present (domain.ParseDecimal). The
// scale and size limits are the service's (checkQuantity), so every ingest
// path shares them.
func parseQuantity(raw json.RawMessage) (decimal.Decimal, error) {
	q, ok, err := domain.ParseDecimal("quantity", raw)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !ok {
		return decimal.Decimal{}, errs.Required("quantity")
	}
	return q, nil
}

// resolveCache memoizes the customer and meter lookups for the lifetime of
// ONE request. Each lookup opens its own RLS transaction (BEGIN + three
// set_config calls + the SELECT), so a 1000-event batch for one customer and
// one meter paid 2000 of them — the dominant cost of batch ingest, and pure
// waste, since a batch is overwhelmingly one customer's traffic.
//
// The scope is deliberately per-request and NOT a process-lifetime cache: a
// longer-lived one would keep serving a customer deleted mid-flight or a meter
// re-keyed by another operator, with no invalidation path. Within a single
// request that staleness window does not exist.
type resolveCache struct {
	customers map[string]string // external customer id -> internal id
	meters    map[string]string // event name -> meter id
}

func newResolveCache() *resolveCache {
	return &resolveCache{
		customers: make(map[string]string),
		meters:    make(map[string]string),
	}
}

// The four accessors are nil-safe so the single-event doors can pass nil and
// take the uncached path without a branch at every call site.
func (c *resolveCache) customer(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	id, ok := c.customers[key]
	return id, ok
}

func (c *resolveCache) putCustomer(key, id string) {
	if c != nil {
		c.customers[key] = id
	}
}

func (c *resolveCache) meter(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	id, ok := c.meters[key]
	return id, ok
}

func (c *resolveCache) putMeter(key, id string) {
	if c != nil {
		c.meters[key] = id
	}
}

// missingKeyMessage is the 422 text for an event with no idempotency_key. It
// names the fix, since a client that answers the 422 with a fresh key on every
// attempt still double-counts.
// MaxIdempotencyKeyLen is the longest idempotency_key accepted, in characters.
const MaxIdempotencyKeyLen = 255

const longKeyMessage = "idempotency_key is longer than 255 characters"

const missingKeyMessage = "idempotency_key is required — give each event its own key and send the same key when you retry that event"

// resolve converts public API identifiers to internal IDs. cache may be nil
// (single-event doors), in which case every lookup goes to the store.
func (h *Handler) resolve(ctx context.Context, tenantID string, evt APIEvent, cache *resolveCache) (IngestInput, error) {
	extCust := strings.TrimSpace(evt.ExternalCustomerID)
	eventName := strings.TrimSpace(evt.EventName)

	if extCust == "" {
		return IngestInput{}, errs.Required("external_customer_id")
	}
	if eventName == "" {
		return IngestInput{}, errs.Required("event_name")
	}
	// Without a key Velox cannot tell a retry from new usage, so a client
	// that missed the response and sent the event again was billed twice
	// (#980). Checked before the lookups so a keyless event never costs a
	// store round trip or turns into a 503. The key is stored as sent, not
	// trimmed, so existing keys keep their meaning. LiteLLM does not come
	// through here: it builds its own keys (litellm/mapper.go).
	if strings.TrimSpace(evt.IdempotencyKey) == "" {
		return IngestInput{}, errs.Invalid("idempotency_key", missingKeyMessage)
	}
	// A longer key would pass validation and then fail the UNIQUE index
	// (Postgres caps a btree row near 2.7 KB) with a 5xx, which tells an
	// ingest client to retry: forever. 255 characters is at most 1,020 bytes.
	if utf8.RuneCountInString(evt.IdempotencyKey) > MaxIdempotencyKeyLen {
		return IngestInput{}, errs.Invalid("idempotency_key", longKeyMessage)
	}

	if hasProperties(evt.Properties) {
		return IngestInput{}, errs.Invalid("properties", "properties was renamed dimensions; send the event's dimensions as dimensions")
	}
	quantity, err := parseQuantity(evt.Quantity)
	if err != nil {
		return IngestInput{}, err
	}

	// A miss is cached only on success: caching the "not found" verdict would
	// turn one bad index in a batch into a repeated failure for every later
	// event naming the same customer, which is the same answer but a slower
	// and less obvious one to debug.
	custID, ok := cache.customer(extCust)
	if !ok {
		cust, err := h.customers.GetByExternalID(ctx, tenantID, extCust)
		if err != nil {
			// Both lookup stores return errs.ErrNotFound only on
			// sql.ErrNoRows. Any other error means the store gave no answer,
			// and calling that "not found" tells the client to stop retrying
			// usage that was never recorded.
			if !errors.Is(err, errs.ErrNotFound) {
				return IngestInput{}, fmt.Errorf("%w: customer lookup: %w", errIngestUnavailable, err)
			}
			return IngestInput{}, errs.Invalid("external_customer_id", fmt.Sprintf("customer %q not found", extCust))
		}
		custID = cust.ID
		cache.putCustomer(extCust, custID)
	}

	meterID, ok := cache.meter(eventName)
	if !ok {
		meter, err := h.meters.GetMeterByEventName(ctx, tenantID, eventName)
		if err != nil {
			if !errors.Is(err, errs.ErrNotFound) {
				return IngestInput{}, fmt.Errorf("%w: meter lookup: %w", errIngestUnavailable, err)
			}
			return IngestInput{}, errs.Invalid("event_name", fmt.Sprintf("meter %q not found", eventName))
		}
		meterID = meter.ID
		cache.putMeter(eventName, meterID)
	}

	input := IngestInput{
		CustomerID:     custID,
		MeterID:        meterID,
		Quantity:       quantity,
		Dimensions:     evt.Dimensions,
		IdempotencyKey: evt.IdempotencyKey,
	}

	if evt.Timestamp != nil {
		// A caller that sends a timestamp means it. Silently discarding a
		// malformed/non-string value and falling back to wall-clock now
		// back-dates or future-dates usage into the wrong billing period —
		// reject it instead, matching the Backfill and getSummary paths.
		var ts any
		if err := json.Unmarshal(*evt.Timestamp, &ts); err != nil {
			return IngestInput{}, errs.Invalid("timestamp", "must be an RFC3339 string, e.g. 2026-05-31T12:00:00Z")
		}
		tsStr, ok := ts.(string)
		if !ok {
			return IngestInput{}, errs.Invalid("timestamp", "must be an RFC3339 string, e.g. 2026-05-31T12:00:00Z")
		}
		parsed, err := parseTimestamp(tsStr)
		if err != nil {
			return IngestInput{}, errs.Invalid("timestamp", "must be an RFC3339 string, e.g. 2026-05-31T12:00:00Z")
		}
		input.Timestamp = &parsed
	}

	return input, nil
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var evt APIEvent
	if !respond.DecodeStrict(w, r, &evt) {
		return
	}

	input, err := h.resolve(r.Context(), tenantID, evt, nil)
	if err != nil {
		if errors.Is(err, errIngestUnavailable) {
			respondIngestUnavailable(w, r, tenantID, err)
			return
		}
		respond.FromError(w, r, err, "usage_event")
		return
	}

	event, err := h.svc.Ingest(r.Context(), tenantID, input)
	if err != nil {
		h.respondIngestError(w, r, tenantID, input.IdempotencyKey, err)
		return
	}

	respond.JSON(w, r, http.StatusCreated, event)
}

// errIngestUnavailable marks a NON-verdict: a customer or meter lookup from
// resolve, or the service's open-period or aggregation read (open_period.go),
// that the store could not answer (failover, a lost pool, a cancelled request). The
// three ingest doors answer it with 503 so the client retries, the same
// contract as the LiteLLM door. Never returned to the caller as text.
var errIngestUnavailable = errors.New("usage: lookup store unavailable")

// respondIngestUnavailable writes the 503 for errIngestUnavailable. The raw
// error is logged, never sent to the caller (ADR-026). "Nothing was recorded"
// holds at every door because resolve and the open-period read run before
// any write, and the aggregation read runs on the write's transaction, so a
// failed one rolls the write back.
func respondIngestUnavailable(w http.ResponseWriter, r *http.Request, tenantID string, err error) {
	slog.ErrorContext(r.Context(), "usage ingest: lookup store unavailable — request refused for client retry",
		"tenant_id", tenantID,
		"error", err,
	)
	respond.Error(w, r, http.StatusServiceUnavailable, "api_error", "ingest_unavailable",
		"usage store temporarily unavailable — nothing was recorded; retry the request")
}

// respondIngestError translates ingest failures, treating an idempotency
// replay as SUCCESS: 200 + the ORIGINAL event + Idempotent-Replayed
// header (Stripe idempotency shape, and the same header the API-wide
// Idempotency-Key middleware sets). Pre-fix a replay got a bare 409 with
// no fetch-original — retry middleware doing at-least-once delivery read
// healthy dedup as failure, inconsistent with the LiteLLM door's
// silent-success contract. Falls back to the plain error mapping if the
// original row can't be read.
func (h *Handler) respondIngestError(w http.ResponseWriter, r *http.Request, tenantID, idempotencyKey string, err error) {
	if errors.Is(err, errIngestUnavailable) {
		// An open-period or aggregation read failed: nothing recorded.
		respondIngestUnavailable(w, r, tenantID, err)
		return
	}
	if errors.Is(err, errs.ErrDuplicateKey) && idempotencyKey != "" {
		if original, gerr := h.svc.GetByIdempotencyKey(r.Context(), tenantID, idempotencyKey); gerr == nil {
			// A replay MUTATES NOTHING — it hands back the row the original
			// request already wrote (and, for backfill, already audited).
			// Declare that to the audit observer: without it every client
			// retry of a backfill would report as an uncovered mutation, and
			// a second audit row would record an insert that never happened.
			audit.MarkSkip(r.Context())
			w.Header().Set("Idempotent-Replayed", "true")
			respond.JSON(w, r, http.StatusOK, original)
			return
		}
	}
	respond.FromError(w, r, err, "usage_event")
}

// backfill ingests a historical usage event — same payload shape as POST /
// but the service enforces that timestamp is non-nil and strictly in the
// past, and the row is tagged origin='backfill' in the audit trail.
//
// Use cases: CSV imports from a legacy system, reconciliation after a
// dropped-event incident, migrating from another billing vendor. For
// real-time ingest, use POST /usage-events instead.
func (h *Handler) backfill(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var evt APIEvent
	if !respond.DecodeStrict(w, r, &evt) {
		return
	}

	input, err := h.resolve(r.Context(), tenantID, evt, nil)
	if err != nil {
		if errors.Is(err, errIngestUnavailable) {
			respondIngestUnavailable(w, r, tenantID, err)
			return
		}
		respond.FromError(w, r, err, "usage_event")
		return
	}

	event, err := h.svc.Backfill(r.Context(), tenantID, input)
	if err != nil {
		// Same replay-as-success contract as live ingest — backfill
		// shares the idempotency-key space.
		h.respondIngestError(w, r, tenantID, input.IdempotencyKey, err)
		return
	}

	respond.JSON(w, r, http.StatusCreated, event)
}

// parseListFilter pulls the shared list/aggregate query params off the
// request URL. Both handlers honour customer_id / meter_id / from / to;
// the list handler additionally reads limit + offset (respond.ParsePage),
// which Aggregate does not take (the whole point of the aggregate endpoint
// is the unbounded total). Empty values are treated as absent so the frontend can build
// URLs unconditionally. from / to go through timefilter.ParseRange, the
// parser the CSV export uses: RFC 3339 or YYYY-MM-DD, anything else is an
// error the caller answers with 422. Dropping a value it could not read
// returned all-time totals as if they were filtered.
func parseListFilter(r *http.Request) (ListFilter, error) {
	q := r.URL.Query()
	filter := ListFilter{
		TenantID:   auth.TenantID(r.Context()),
		CustomerID: q.Get("customer_id"),
		MeterID:    q.Get("meter_id"),
	}
	from, to, err := timefilter.ParseRange(r, "from", "to")
	if err != nil {
		return ListFilter{}, err
	}
	if !from.IsZero() {
		filter.From = &from
	}
	if !to.IsZero() {
		filter.To = &to
	}
	return filter, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	filter, err := parseListFilter(r)
	if err != nil {
		respond.FromError(w, r, err, "usage_event")
		return
	}
	filter.Limit, filter.Offset = page.Limit, page.Offset
	dims, err := parseDimensionsParam(r.URL.Query().Get("dimensions"))
	if err != nil {
		// Unlike the other list params, a malformed dimension filter is a
		// hard 422, not treated-as-absent — silently dropping it would
		// render UNfiltered data as filtered (the exact bug this param's
		// server side shipped with).
		respond.Validation(w, r, err.Error())
		return
	}
	filter.Dimensions = dims
	events, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list usage events", "error", err)
		return
	}
	if events == nil {
		events = []domain.UsageEvent{}
	}

	respond.List(w, r, events, total)
}

// aggregate returns server-side totals + per-meter breakdown for the
// current filter. Mirrors the list handler's filter parsing exactly so
// the dashboard's stat cards reflect the same scope as the events table.
// Unlike list, limit/offset are ignored — Aggregate.TotalEvents is the
// authoritative count, and ByMeter is the full filtered breakdown.
func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request) {
	filter, err := parseListFilter(r)
	if err != nil {
		respond.FromError(w, r, err, "usage_event")
		return
	}
	dims, err := parseDimensionsParam(r.URL.Query().Get("dimensions"))
	if err != nil {
		// Hard 422 — same rationale as list: a dropped filter shows
		// unfiltered totals as filtered.
		respond.Validation(w, r, err.Error())
		return
	}
	filter.Dimensions = dims
	// tz buckets the per-meter daily series by calendar day (the tenant's
	// timezone on the dashboard). Unknown names are refused, not ignored,
	// so a typo never silently re-buckets the chart to UTC.
	if tz := r.URL.Query().Get("tz"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil || tz == "Local" {
			respond.Validation(w, r, fmt.Sprintf("tz: unknown time zone %q (use an IANA name such as Europe/Berlin)", tz))
			return
		}
		filter.DayLoc = loc
	}

	modes := map[string]string{}
	if h.meterModes != nil {
		meters, err := h.meterModes.ListMeters(r.Context(), filter.TenantID)
		if err != nil {
			respond.InternalError(w, r)
			slog.ErrorContext(r.Context(), "aggregate usage events: list meters", "error", err)
			return
		}
		modes = BillingModes(meters)
	}

	agg, err := h.svc.Aggregate(r.Context(), filter, modes)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "aggregate usage events", "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, agg)
}

// batchIngest is ALL-OR-NOTHING: every event validates, then the whole
// batch commits in one transaction. Pre-fix each event committed in its
// own tx and a mid-batch abort (client timeout, dropped connection) left
// a committed prefix. Atomicity rules out that prefix; it does not make a
// retry safe on its own, because a batch that committed and lost its
// response is written again unless its events carry keys. That is why
// resolve requires a key on every event (#980). Replays of
// already-ingested keys are SUCCESS (counted in "deduplicated"), not
// error rows — an at-least-once delivery pipeline retrying a committed
// batch must not trip its alerting on HTTP 206 + 1000 'duplicate key'
// strings (and the LiteLLM door already treats dupes as success; two
// front doors, one contract).
func (h *Handler) batchIngest(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var raws []json.RawMessage
	if err := respond.DecodeBody(r, &raws); err != nil {
		// The router caps request bodies via MaxBytesReader; a too-big
		// batch must read as "split your batch" (413), not as malformed
		// JSON (400) — the byte cap fires mid-array on valid JSON.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respond.Error(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "batch_too_large",
				fmt.Sprintf("request body exceeds %d bytes — send smaller batches", maxErr.Limit))
			return
		}
		respond.BadRequest(w, r, "expected JSON array of events")
		return
	}

	if len(raws) == 0 {
		respond.BadRequest(w, r, "at least one event is required")
		return
	}
	if len(raws) > 1000 {
		respond.BadRequest(w, r, "maximum 1000 events per batch")
		return
	}
	// Each event follows the one body rule. An unknown key is that event's
	// failure, named in errors[] with its index like every other; a value
	// of the wrong JSON type is a 400 for the body, as before.
	events := make([]APIEvent, len(raws))
	keyErrs := make([]error, len(raws))
	for i, raw := range raws {
		if err := respond.DecodeValue(raw, &events[i]); err != nil {
			var bodyErr *respond.BodyError
			if errors.As(err, &bodyErr) && bodyErr.Field != "" {
				keyErrs[i] = err
				continue
			}
			respond.BadRequest(w, r, "expected JSON array of events")
			return
		}
	}

	// Resolve-stage failures (missing fields, unknown customer/meter,
	// malformed timestamp — the common 422 class) are collected for EVERY
	// failing index, same as the ingest stage below: a bare first-error
	// made 500-event batches undebuggable, and the spec promises one
	// errors[] naming every failing index for the batch 422.
	var inputs []IngestInput
	var resolveErrs []string
	// One cache for the whole batch — see resolveCache. A batch naming one
	// customer and one meter now costs two lookups instead of 2N.
	cache := newResolveCache()
	for i, evt := range events {
		if keyErrs[i] != nil {
			resolveErrs = append(resolveErrs, fmt.Sprintf("event[%d]: %s", i, keyErrs[i].Error()))
			continue
		}
		input, err := h.resolve(r.Context(), tenantID, evt, cache)
		if err != nil {
			// A store that cannot answer is not a verdict on this event, so
			// it must not join errors[] as one. Refuse the whole batch now:
			// the remaining events would each wait on the same failing store.
			if errors.Is(err, errIngestUnavailable) {
				respondIngestUnavailable(w, r, tenantID, err)
				return
			}
			resolveErrs = append(resolveErrs, fmt.Sprintf("event[%d]: %s", i, err.Error()))
			continue
		}
		inputs = append(inputs, input)
	}
	if len(resolveErrs) > 0 {
		respondBatchRejected(w, r, resolveErrs, len(events))
		return
	}

	res, ingestErrs := h.svc.BatchIngest(r.Context(), tenantID, inputs)

	if len(ingestErrs) > 0 {
		// A store that gave no answer refuses the batch for retry; nothing
		// was written (the open-period read runs before the write, and a
		// failed aggregation read rolls the batch back).
		for _, e := range ingestErrs {
			if errors.Is(e, errIngestUnavailable) {
				respondIngestUnavailable(w, r, tenantID, e)
				return
			}
		}
		errStrings := make([]string, len(ingestErrs))
		for i, e := range ingestErrs {
			errStrings[i] = e.Error()
		}
		respondBatchRejected(w, r, errStrings, len(events))
		return
	}

	respond.JSON(w, r, http.StatusCreated, BatchResponse{
		Ingested:       res.Inserted,
		Deduplicated:   res.Deduplicated,
		Total:          len(events),
		UnbilledCounts: res.Unbilled,
		Results:        res.Results,
	})
}

// BatchResponse is the batch 201. The counts by billing_status count written
// rows answered with each status (open_period.go); results holds each event's
// result at its index in the request.
type BatchResponse struct {
	Ingested     int `json:"ingested"`
	Deduplicated int `json:"deduplicated"`
	Total        int `json:"total"`
	UnbilledCounts
	Results []BatchEventResult `json:"results"`
}

// respondBatchRejected writes the batch 422 the spec documents: every
// failing index in one errors[] array ("event[i]: reason") plus an
// explicit ingested:0 marker that the all-or-nothing batch wrote nothing.
// Shared by the resolve and ingest validation stages so both produce the
// same shape — pre-fix the resolve stage returned a bare first-error 422
// with no errors[], the exact undebuggable-batch failure the ingest
// stage's accumulation exists to prevent.
func respondBatchRejected(w http.ResponseWriter, r *http.Request, errStrings []string, total int) {
	respond.ErrorWith(w, r, http.StatusUnprocessableEntity, "invalid_request_error", "batch_rejected",
		"batch rejected — nothing was ingested; fix the listed events and retry the whole batch",
		map[string]any{"errors": errStrings, "ingested": 0, "total": total})
}

// ParseDimensionsParam parses the `dimensions` query value ("k=v,k2=v2")
// the same way the list and aggregate endpoints do. Exported for the CSV
// export, which takes the same filter.
func ParseDimensionsParam(raw string) (map[string]any, error) { return parseDimensionsParam(raw) }

// parseDimensionsParam parses the `dimensions` query param: comma-
// separated key=value pairs ("model=gpt-4o,token_type=input"). Values
// that parse as JSON booleans or numbers are typed accordingly
// ("cached=true" matches the JSONB boolean true, "attempt=2" the number
// 2) — matching how ingest stored them; everything else matches as a
// string. Malformed pairs are an ERROR, never treated-as-absent: the
// callers 422 so a broken filter can't render unfiltered data as
// filtered.
func parseDimensionsParam(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	dims := make(map[string]any)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("dimension filter %q must be key=value (e.g. model=gpt-4o)", pair)
		}
		var typed any = v
		var lit any
		if err := json.Unmarshal([]byte(v), &lit); err == nil {
			switch lit.(type) {
			case bool, float64:
				typed = lit
			}
		}
		dims[k] = typed
	}
	if len(dims) == 0 {
		return nil, nil
	}
	return dims, nil
}

func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp format: %s", s)
}
