package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// CustomerLookup is the narrow surface the adapter uses to resolve
// LiteLLM's `user` field (= external_customer_id) to a Velox internal
// customer_id. Implemented by *customer.PostgresStore.
type CustomerLookup interface {
	GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error)
}

// MeterLookup is the narrow surface the adapter uses to resolve the
// meter_key ("tokens_input" / "tokens_output") to a Velox internal
// meter_id. Implemented by *pricing.Service.
type MeterLookup interface {
	GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error)
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

// Handler exposes POST /v1/integrations/litellm/spend. Auth is the
// standard API key (Bearer); operator generates a Velox key and
// pastes it into LiteLLM's GENERIC_LOGGER_HEADERS as
// `Authorization: Bearer <vlx_secret_…>`.
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
// every POST, so the Velox auth middleware enforces the tenant.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/spend", h.spend)
	return r
}

// SpendResponse is the wire shape returned by POST /spend. Mirrors
// the existing /v1/usage-events/batch response so partners using
// both surfaces see one shape:
//
//	{ "accepted": N, "deduplicated": D, "held": H, "skipped": M, "cache_hits": C, "errors": [{ "id": "...", "error": "..." }] }
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
	Accepted     int             `json:"accepted"`
	Deduplicated int             `json:"deduplicated"`
	Held         int             `json:"held"`
	Skipped      int             `json:"skipped"`
	CacheHits    int             `json:"cache_hits"`
	Errors       []SpendRowError `json:"errors,omitempty"`
}

// SpendRowError carries the per-payload reason a particular row was
// rejected. The adapter is intentionally permissive — one bad row
// doesn't fail the whole batch. LiteLLM retries the whole batch on
// 5xx, so a per-row errors[] entry inside a 200 avoids retry storms
// when a single misconfigured call lacks `user`.
type SpendRowError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// spend handles the LiteLLM generic_api callback. Accepts either a
// single payload (LiteLLM's default callback shape) or a batch
// (operator-side buffered shape).
//
// Flow:
//  1. Decode body; normalize single | batch to []StandardLoggingPayload
//  2. For each payload: MapPayload → the call's events, or a skip
//  3. Resolve external_customer_id + meter_key once for the call
//  4. Write the call in one transaction (usage.Service.IngestCall, or
//     HoldCall when the customer or meter does not exist yet), idempotent
//     via the usage_events UNIQUE (tenant_id, livemode, idempotency_key) —
//     tenant-wide, NOT per customer/meter, which is why the mapper
//     suffixes the key per token type
//  5. Tally accepted / deduplicated / held / skipped / errors; return 200
//
// The status code answers ONE question: did Velox reach a verdict on
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
		respond.BadRequest(w, r, err.Error())
		return
	}

	resp := SpendResponse{Errors: []SpendRowError{}}

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
		res, err := h.persist(r.Context(), tenantID, m)
		switch {
		case err == nil:
			if res.held {
				resp.Held += res.Inserted
			} else {
				resp.Accepted += res.Inserted
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
	meter, err := h.meters.GetMeterByKey(ctx, tenantID, first.MeterKey)
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
		// misfiled as per-row).
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
		MeterKey:           first.MeterKey,
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

// decodeBody normalizes the request body into a slice of payloads.
// Accepts:
//   - LiteLLM's default callback shape: single payload as the top-level object
//   - Batched shape: { "events": [...] } (used by some operator-side buffers)
//   - Bare array: [payload, payload, ...] (defensive — some HTTP clients send this)
func decodeBody(r *http.Request) ([]StandardLoggingPayload, error) {
	defer func() { _ = r.Body.Close() }()

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body")
	}

	// Bare array first — try [..] before the object shape.
	if raw[0] == '[' {
		var batch []StandardLoggingPayload
		if err := json.Unmarshal(raw, &batch); err == nil {
			return batch, nil
		}
	}

	// { events: [...] } shape.
	var withEvents struct {
		Events []StandardLoggingPayload `json:"events"`
	}
	if err := json.Unmarshal(raw, &withEvents); err == nil && len(withEvents.Events) > 0 {
		return withEvents.Events, nil
	}

	// Single payload.
	var one StandardLoggingPayload
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("invalid JSON body")
	}
	return []StandardLoggingPayload{one}, nil
}
