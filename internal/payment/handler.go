package payment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

const maxWebhookBodySize = 65536 // 64KB

// EndpointResolver is the narrow surface the webhook handler needs from
// tenantstripe.Service. Exists so the handler can be tested without a DB.
type EndpointResolver interface {
	LookupEndpoint(ctx context.Context, endpointID string) (tenantstripe.EndpointLookup, error)
}

type Handler struct {
	stripe    *Stripe
	endpoints EndpointResolver
}

// NewHandler wires the Stripe webhook receiver. Credentials (signing secret,
// tenant id, livemode) are resolved per request via the endpoint id embedded
// in the URL path — there is no operator-level secret: each tenant registers
// their own webhook endpoint in their own Stripe dashboard pointing at
// /v1/webhooks/stripe/{endpoint_id}, where endpoint_id is the
// stripe_provider_credentials.id (hc_spc_XXX) Hardcount handed them when they
// connected. Moving secrets off env vars is the whole point of the per-
// tenant model (see migration 0032).
func NewHandler(stripe *Stripe, endpoints EndpointResolver) *Handler {
	return &Handler{
		stripe:    stripe,
		endpoints: endpoints,
	}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/stripe/{endpoint_id}", h.handleStripeWebhook)
	return r
}

func (h *Handler) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	endpointID := chi.URLParam(r, "endpoint_id")
	if endpointID == "" {
		respond.NotFound(w, r, "webhook_endpoint")
		return
	}

	// Look up the tenant-owned endpoint BEFORE reading the body — a bogus URL
	// shouldn't even buffer the payload.
	lookup, err := h.endpoints.LookupEndpoint(r.Context(), endpointID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			// Tenant disconnected / never registered the webhook, or someone
			// is probing random endpoint ids. 404 either way — don't leak
			// the distinction.
			respond.NotFound(w, r, "webhook_endpoint")
			return
		}
		slog.ErrorContext(r.Context(), "webhook endpoint lookup failed",
			"endpoint_id", endpointID, "error", err)
		respond.InternalError(w, r)
		return
	}

	// Read ONE byte beyond the cap so an oversized body is DETECTED, not
	// silently truncated. Pre-fix a >64KB event was truncated at the limit,
	// the HMAC then failed over the truncated bytes, and the request died as a
	// misleading "invalid signature" 400 — after ~3 days of Stripe retries the
	// event was permanently lost with nothing in the logs pointing at size.
	// A 413 + ERROR names the real problem (an event shape larger than the
	// cap) so the operator raises maxWebhookBodySize instead of chasing a
	// phantom signing-secret issue.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodySize+1))
	if err != nil {
		respond.BadRequest(w, r, "failed to read body")
		return
	}
	if len(body) > maxWebhookBodySize {
		slog.ErrorContext(r.Context(), "stripe webhook body exceeds the size cap — Stripe will retry ~3 days then drop the event permanently; raise maxWebhookBodySize if this event type is legitimate",
			"endpoint_id", endpointID,
			"tenant_id", lookup.TenantID,
			"cap_bytes", maxWebhookBodySize,
		)
		respond.Error(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "payload_too_large",
			"webhook body exceeds the size cap")
		return
	}

	// Signature check is stripe-go's own (webhook.ValidatePayload, 5-minute
	// DefaultTolerance). Its tolerance is ONE-SIDED: only signatures OLDER than
	// the window are rejected. The hand-rolled check this replaced was
	// two-sided, so a host clock running >5 min behind Stripe rejected every
	// delivery — a silent total inbound blackout (Stripe retries ≤3 days, then
	// drops). Stripe's reference semantics are the contract.
	sigHeader := r.Header.Get("Stripe-Signature")
	if err := webhook.ValidatePayload(body, sigHeader, lookup.WebhookSecret); err != nil {
		slog.WarnContext(r.Context(), "stripe webhook signature verification failed",
			"endpoint_id", endpointID,
			"tenant_id", lookup.TenantID,
			"reason", err.Error(),
		)
		respond.BadRequest(w, r, "invalid signature")
		return
	}

	event, metaTenant, err := parseStripeEvent(body, lookup.TenantID, lookup.Livemode)
	switch {
	case errors.Is(err, errEventUnreadable):
		respond.BadRequest(w, r, "invalid JSON")
		return
	case errors.Is(err, errEventModeMismatch):
		slog.WarnContext(r.Context(), "stripe webhook livemode mismatch",
			"endpoint_id", endpointID,
			"tenant_id", lookup.TenantID,
			"endpoint_mode", lookup.Livemode,
			"payload_mode", !lookup.Livemode,
			"stripe_event_id", event.StripeEventID,
		)
		respond.BadRequest(w, r, "livemode mismatch")
		return
	case errors.Is(err, errEventOtherTenant):
		slog.WarnContext(r.Context(), "stripe webhook tenant mismatch",
			"endpoint_id", endpointID,
			"endpoint_tenant", lookup.TenantID,
			"metadata_tenant", metaTenant,
			"stripe_event_id", event.StripeEventID,
		)
		respond.BadRequest(w, r, "tenant mismatch")
		return
	case errors.Is(err, errEventNotHardcount):
		// The tenant's own Stripe object at the same endpoint: acknowledge.
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "skipped", "reason": "no hardcount metadata"})
		return
	}

	// Stage ctx with the endpoint's tenant + livemode before handing off —
	// adapter DB writes read ctx tenant/livemode to satisfy RLS policies.
	ctx := auth.WithTenantID(r.Context(), lookup.TenantID)
	ctx = postgres.WithLivemode(ctx, lookup.Livemode)

	if err := h.stripe.HandleWebhook(ctx, lookup.TenantID, event); err != nil {
		slog.ErrorContext(ctx, "webhook processing failed",
			"stripe_event_id", event.StripeEventID,
			"event_type", event.EventType,
			"tenant_id", lookup.TenantID,
			"error", err,
		)
		// A returned error is transient by construction: HandleWebhook
		// already ack's (returns nil) any event it can't ever process —
		// e.g. one referencing an invoice not in our DB — after recording
		// it for audit. So the only errors that reach here are retryable
		// (DB write failure, contention, downstream timeout), and crucially
		// NO dedup row was persisted for them. Respond 5xx so Stripe
		// redelivers and the handler re-runs; returning 200 here would let
		// a transient blip silently drop the event forever. Don't echo the
		// raw error to Stripe (may leak internal details).
		respond.InternalError(w, r)
		return
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "processed"})
}

// The reasons parseStripeEvent refuses an event body. The webhook handler
// answers each one with its own status. The restore catch-up skips the event.
var (
	errEventUnreadable   = errors.New("invalid JSON")
	errEventModeMismatch = errors.New("livemode mismatch")
	errEventOtherTenant  = errors.New("tenant mismatch")
	errEventNotHardcount = errors.New("no hardcount metadata")
)

// parseStripeEvent reads a Stripe event body for the account that received
// it: the tenant and mode of the webhook endpoint, or of the credentials the
// restore catch-up listed it with. Both paths run these guards, so an event
// from a Stripe account that several tenants share goes to the same tenant
// either way. The errors:
//   - errEventUnreadable: the body is not JSON.
//   - errEventModeMismatch: the body's livemode is not the account's. The
//     account's mode is authoritative: its key or signing secret is
//     mode-specific, so a body for the other mode was not meant for it.
//   - errEventOtherTenant: hardcount_tenant_id names another tenant, returned
//     as metaTenant. Someone pointed a Stripe account at the wrong endpoint,
//     and writing the event into this tenant's rows would cross tenants.
//   - errEventNotHardcount: no hardcount_tenant_id, and the type is not one
//     the account is authoritative for (endpointAuthoritativeEvent). It is
//     the tenant's own Stripe object, not Hardcount's.
//
// The returned event carries its StripeEventID even with an error, for the
// log line.
func parseStripeEvent(body []byte, tenantID string, livemode bool) (event domain.StripeWebhookEvent, metaTenant string, err error) {
	var raw struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Created  int64  `json:"created"`
		Livemode bool   `json:"livemode"`
		Data     struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.StripeWebhookEvent{}, "", errEventUnreadable
	}
	event.StripeEventID = raw.ID
	if raw.Livemode != livemode {
		return event, "", errEventModeMismatch
	}

	var obj struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		// PaymentIntent is read only from a Dispute, whose own id is not a
		// payment's (see below).
		PaymentIntent json.RawMessage `json:"payment_intent"`
		Status        string          `json:"status"`
		Amount        int64           `json:"amount"`
		Currency      string          `json:"currency"`
		Customer      string          `json:"customer"`
		LastError     *struct {
			Message string `json:"message"`
		} `json:"last_payment_error"`
		Metadata map[string]string `json:"metadata"`
	}
	_ = json.Unmarshal(raw.Data.Object, &obj) // an unreadable object has no fields: the guard below skips it, and an authoritative type's handler refuses its empty id

	metaTenant = obj.Metadata["hardcount_tenant_id"]
	if metaTenant != "" && metaTenant != tenantID {
		return event, metaTenant, errEventOtherTenant
	}
	if metaTenant == "" && !endpointAuthoritativeEvent(raw.Type) {
		// A few event types are Hardcount's by the object itself, not by a
		// metadata marker. Stripe Checkout does not copy a session's
		// metadata onto the SetupIntent it creates, so Hardcount's
		// setup_intent.succeeded arrives with empty metadata, and the
		// handler maps the SetupIntent's customer back to the Hardcount
		// customer instead.
		return event, "", errEventNotHardcount
	}

	// Scrub Stripe's free-text failure message before it enters Hardcount
	// data. It is saved to stripe_webhook_events.failure_message and copied
	// into invoices.last_payment_error by handlePaymentFailed, so scrubbing
	// here keeps PII (card last4, emails) out of both.
	failureMsg := ""
	if obj.LastError != nil {
		failureMsg = errs.Scrub(obj.LastError.Message)
	}

	amount := obj.Amount
	// The audit row's payment_intent_id names the payment. For a Dispute the
	// object's own id is the dispute's (du_…), so the disputed PaymentIntent
	// goes there instead.
	paymentIntentID := obj.ID
	if obj.Object == "dispute" {
		paymentIntentID = stripeObjectID(obj.PaymentIntent)
	}
	return domain.StripeWebhookEvent{
		StripeEventID:      raw.ID,
		EventType:          raw.Type,
		ObjectType:         obj.Object,
		PaymentIntentID:    paymentIntentID,
		PaymentStatus:      obj.Status,
		AmountCents:        &amount,
		Currency:           obj.Currency,
		CustomerExternalID: obj.Customer,
		FailureMessage:     failureMsg,
		InvoiceID:          obj.Metadata["hardcount_invoice_id"],
		Payload:            map[string]any{"raw": string(body)},
		OccurredAt:         time.Unix(raw.Created, 0).UTC(),
		Livemode:           livemode,
	}, metaTenant, nil
}

// endpointAuthoritativeEvent reports whether an event type may be processed
// using only the endpoint's authoritative tenant + the object's own
// references, without a hardcount_tenant_id metadata marker. Stripe Checkout
// doesn't copy a session's metadata onto the SetupIntent it creates, so a
// Hardcount-originated setup_intent.* arrives with empty metadata; these are
// recognized downstream by mapping the SetupIntent's `customer` back to the
// Hardcount customer (setup_failed is a no-op log). Kept deliberately narrow —
// payment_intent.* still requires the metadata marker so a tenant's own
// unrelated charges at the same endpoint aren't ingested.
func endpointAuthoritativeEvent(eventType string) bool {
	switch eventType {
	case "setup_intent.succeeded", "setup_intent.setup_failed":
		return true
	case "refund.created", "charge.refund.updated", "refund.updated", "refund.failed":
		// A Refund carries its own metadata, not the PaymentIntent's, so it
		// never has hardcount_tenant_id: Hardcount's own refunds carry only
		// hardcount_cn_id (since the ADR-063 amendment), and a refund made in
		// the Stripe dashboard carries none. Without admitting them here they
		// would be skipped. The endpoint tenant is authoritative (per-tenant
		// secret + HMAC); the handler matches the refund to a credit note by
		// its id, or to the invoice its PaymentIntent paid (ADR-134), and
		// acknowledges a refund of a payment no Hardcount invoice holds.
		return true
	case "charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed":
		// A Dispute carries its own metadata, not the PaymentIntent's, so a
		// dispute on a Hardcount charge arrives without hardcount_tenant_id (#979).
		// The endpoint tenant is authoritative; the handler matches the
		// dispute to a Hardcount invoice by the disputed PaymentIntent and acks
		// one that no Hardcount invoice holds.
		return true
	default:
		return false
	}
}
