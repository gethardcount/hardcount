package webhook

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// AuditWriter is the narrow audit surface webhook handler uses.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

type Handler struct {
	svc         *Service
	auditLogger AuditWriter
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger wires audit on webhook endpoint lifecycle + manual
// event replays. Endpoint URL changes + secret rotations are
// integration-security events; manual replays change downstream
// state and need forensic trail.
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/endpoints", h.createEndpoint)
	r.Get("/endpoints", h.listEndpoints)
	r.Get("/endpoints/stats", h.getEndpointStats)
	r.Get("/endpoints/{id}", h.getEndpoint)
	r.Patch("/endpoints/{id}", h.updateEndpoint)
	r.Delete("/endpoints/{id}", h.deleteEndpoint)
	r.Post("/endpoints/{id}/rotate-secret", h.rotateSecret)
	r.Get("/endpoints/{id}/deliveries", h.listEndpointDeliveries)
	r.Post("/endpoints/{id}/deliveries/{deliveryID}/replay", h.replayDelivery)
	r.Get("/events", h.listEvents)
	return r
}

// EventRoutes is the Week 6 real-time event surface, mounted at
// /v1/webhook_events. Lives here (rather than alongside Routes) so the
// route table stays tightly scoped to "the live-tail dashboard" and
// router.go can mount it under its own auth scope without dragging in
// the endpoint-management permissions.
//
// Critical: chi/v5 dispatches by registration order. We register
// /stream BEFORE /{id} so a literal "stream" path doesn't get captured
// as an ID and route to the deliveries handler. See
// docs/dev/design/create-preview.md for the canonical write-up.
//
// Note: the SSE handler (streamEvents) IS exposed here for tests and
// dev-time mounting, but the production router mounts /stream
// SEPARATELY outside the /v1 block so it can skip the global 30s
// middleware.Timeout (which would kill any long-lived stream). See
// internal/api/router.go for the production mount.
func (h *Handler) EventRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/stream", h.streamEvents)
	r.Get("/{id}/deliveries", h.listDeliveriesEnriched)
	r.Post("/{id}/replay", h.replayEventV2)
	return r
}

// StreamHandler is the chi-compatible handler for the live-tail SSE
// stream. Exported so router.go can mount it on a route block that
// skips the 30s timeout middleware applied to /v1/* routes.
func (h *Handler) StreamHandler() http.HandlerFunc {
	return h.streamEvents
}

func (h *Handler) getEndpointStats(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	// One row per endpoint, read whole and paged here like the endpoint
	// list it sits beside.
	stats, err := h.svc.GetEndpointStats(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get webhook endpoint stats", "error", err)
		return
	}
	respond.List(w, r, respond.Window(stats, page), len(stats))
}

// getEndpoint serves GET /endpoints/{id}: one endpoint, for its detail page.
func (h *Handler) getEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, err := h.svc.GetEndpoint(r.Context(), auth.TenantID(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "webhook endpoint")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get webhook endpoint", "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, ep)
}

func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateEndpointInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	result, err := h.svc.CreateEndpoint(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "webhook_endpoint")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "webhook_endpoint", result.Endpoint.ID, result.Endpoint.URL, map[string]any{
			"url":    result.Endpoint.URL,
			"events": result.Endpoint.Events,
		})
	}

	respond.JSON(w, r, http.StatusCreated, result)
}

func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	// A tenant's endpoints are read whole (every dispatch reads the same
	// list) and paged here.
	endpoints, err := h.svc.ListEndpoints(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list webhook endpoints", "error", err)
		return
	}
	respond.List(w, r, respond.Window(endpoints, page), len(endpoints))
}

func (h *Handler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	err := h.svc.DeleteEndpoint(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "endpoint")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionDelete, "webhook_endpoint", id, "", nil)
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

// updateEndpoint is the Stripe-shaped PATCH: url / description / events /
// active mutate WITHOUT rotating the signing secret.
func (h *Handler) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input UpdateEndpointInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	ep, err := h.svc.UpdateEndpoint(r.Context(), tenantID, id, input)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "webhook endpoint")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "webhook_endpoint")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "webhook_endpoint", id, "", map[string]any{
			"url": ep.URL, "active": ep.Active, "events": ep.Events,
		})
	}

	respond.JSON(w, r, http.StatusOK, ep)
}

func (h *Handler) rotateSecret(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	result, err := h.svc.RotateSecret(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "webhook endpoint")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "rotate webhook secret", "error", err)
		return
	}

	// Record rotation without the secret value (security-critical).
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionRotate, "webhook_endpoint", id, "", nil)
	}

	respond.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	events, total, err := h.svc.ListEvents(r.Context(), tenantID, page.Limit, page.Offset)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list webhook events", "error", err)
		return
	}
	respond.List(w, r, events, total)
}

// replayEventV2 is the Week 6 dashboard-facing replay handler. Returns
// the full ReplayResult ({event_id, replay_of, status: "queued"}) so
// the dashboard can highlight the new clone in its live tail and toast
// the audit pivot.
func (h *Handler) replayEventV2(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	eventID := chi.URLParam(r, "id")

	res, err := h.svc.Replay(r.Context(), tenantID, eventID)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "webhook event")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "replay webhook event v2", "error", err)
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "webhook_event", res.EventID, "", map[string]any{
			"action":    "replayed",
			"replay_of": res.ReplayOf,
		})
	}
	respond.JSON(w, r, http.StatusOK, res)
}

// EndpointDeliveryView is the endpoint drill-down row: one receiver's
// history across events, so each row names the business fact it carried
// (event_type) rather than the receiver (which is the page's own
// subject). Snake_case pinned by TestWireShape_EndpointDeliveries.
type EndpointDeliveryView struct {
	ID        string `json:"id"`
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	// IsReplay marks rows born from an operator replay (event-wide or
	// single-receiver) — the drill-down badges them the same way the
	// event timeline does.
	IsReplay        bool       `json:"is_replay"`
	ReplayOfEventID string     `json:"replay_of_event_id,omitempty"`
	Status          string     `json:"status"`
	StatusCode      int        `json:"status_code"`
	AttemptCount    int        `json:"attempt_count"`
	ResponseBody    string     `json:"response_body"`
	Error           string     `json:"error"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	NextRetryAt     *time.Time `json:"next_retry_at"`
}

// listEndpointDeliveries serves GET /endpoints/{id}/deliveries — the
// endpoint drill-down, one page of its deliveries. A deleted endpoint 404s
// instead of rendering an empty-but-healthy-looking history; the endpoint
// itself is GET /endpoints/{id}.
func (h *Handler) listEndpointDeliveries(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	endpointID := chi.URLParam(r, "id")
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	deliveries, total, err := h.svc.ListEndpointDeliveries(r.Context(), tenantID, endpointID, page.Limit, page.Offset)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "webhook endpoint")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list endpoint deliveries", "error", err)
		return
	}

	out := make([]EndpointDeliveryView, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, EndpointDeliveryView{
			ID:              d.ID,
			EventID:         d.WebhookEventID,
			EventType:       d.EventType,
			IsReplay:        d.EventReplayOfID != "",
			ReplayOfEventID: d.EventReplayOfID,
			Status:          string(d.Status),
			StatusCode:      d.HTTPStatusCode,
			AttemptCount:    d.AttemptCount,
			ResponseBody:    truncateBody(d.ResponseBody),
			Error:           d.ErrorMessage,
			CreatedAt:       d.CreatedAt,
			CompletedAt:     d.CompletedAt,
			NextRetryAt:     d.NextRetryAt,
		})
	}

	respond.List(w, r, out, total)
}

// replayDelivery serves POST /endpoints/{id}/deliveries/{deliveryID}/replay
// — the single-receiver replay. Unlike the event-wide replay it never
// touches other endpoints, so fixing one broken receiver doesn't spray
// duplicates at the healthy ones.
func (h *Handler) replayDelivery(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	endpointID := chi.URLParam(r, "id")
	deliveryID := chi.URLParam(r, "deliveryID")

	res, err := h.svc.ReplayDelivery(r.Context(), tenantID, endpointID, deliveryID)
	if err != nil {
		respond.FromError(w, r, err, "webhook_delivery")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "webhook_delivery", deliveryID, "", map[string]any{
			"action":          "replayed_to_endpoint",
			"endpoint_id":     endpointID,
			"replay_event_id": res.EventID,
			"new_delivery_id": res.DeliveryID,
			"replay_of":       res.ReplayOf,
		})
	}

	respond.JSON(w, r, http.StatusOK, res)
}

// DeliveryView is the dashboard-facing delivery row: it carries the
// per-attempt facts the timeline needs (attempt number, status,
// response body, timestamps) plus the request_payload_sha256 the diff
// viewer uses to flag "payload identical between attempts" (the common
// case for Stripe-style replays).
//
// Snake_case throughout — pinned by TestWireShape_WebhookEventDeliveries.
type DeliveryView struct {
	ID      string `json:"id"`
	EventID string `json:"event_id"`
	// EndpointID stays for machine use, but the timeline is an operator
	// surface: EndpointURL/EndpointDescription are what let a human
	// answer "which of my receivers is this?" without cross-referencing
	// an opaque id against a page that never displays ids. Resolved by
	// the store's historical join, so they survive endpoint deletion.
	EndpointID           string     `json:"endpoint_id"`
	EndpointURL          string     `json:"endpoint_url"`
	EndpointDescription  string     `json:"endpoint_description"`
	AttemptNo            int        `json:"attempt_no"`
	Status               string     `json:"status"`
	StatusCode           int        `json:"status_code"`
	ResponseBody         string     `json:"response_body"`
	Error                string     `json:"error"`
	RequestPayloadSHA256 string     `json:"request_payload_sha256"`
	AttemptedAt          time.Time  `json:"attempted_at"`
	CompletedAt          *time.Time `json:"completed_at"`
	NextRetryAt          *time.Time `json:"next_retry_at"`
	IsReplay             bool       `json:"is_replay"`
	ReplayEventID        string     `json:"replay_event_id"`
}

// DeliveriesResponse wraps the timeline. We surface root_event_id so
// the dashboard can confirm it received the original-pivot's chain
// (matters when the operator clicked Replay from a clone — the chain
// is still rooted at the original).
type DeliveriesResponse struct {
	RootEventID string         `json:"root_event_id"`
	Deliveries  []DeliveryView `json:"deliveries"`
}

// listDeliveriesEnriched is the Week 6 deliveries endpoint at GET
// /v1/webhook_events/{id}/deliveries. It returns the enriched view shape
// (attempt_no, request_payload_sha256, etc.) and stitches the original
// event + every replay clone into one ordered timeline.
func (h *Handler) listDeliveriesEnriched(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	eventID := chi.URLParam(r, "id")

	// Resolve the root: an operator opening the dashboard after
	// clicking Replay on a clone might paste the clone's ID; the
	// timeline should still root at the original so the audit chain
	// shows the full history.
	root, err := h.svc.GetEvent(r.Context(), tenantID, eventID)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "webhook event")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list deliveries: get event", "error", err)
		return
	}
	rootID := root.ID
	if root.ReplayOfEventID != nil && *root.ReplayOfEventID != "" {
		rootID = *root.ReplayOfEventID
	}

	deliveries, err := h.svc.ListDeliveries(r.Context(), tenantID, rootID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list deliveries", "error", err)
		return
	}

	// Pre-fetch every event in the tree once so we can hash their
	// payloads without re-querying per-delivery. The payload-SHA256
	// goes onto each delivery row for the diff viewer.
	tree := map[string]domain.WebhookEvent{rootID: {}}
	rootEv, _ := h.svc.GetEvent(r.Context(), tenantID, rootID)
	tree[rootID] = rootEv
	// Fetch each delivery's referencing event in one pass — uniqueified
	// by event_id since deliveries to multiple endpoints share an event.
	for _, d := range deliveries {
		if _, ok := tree[d.WebhookEventID]; !ok {
			ev, err := h.svc.GetEvent(r.Context(), tenantID, d.WebhookEventID)
			if err == nil {
				tree[d.WebhookEventID] = ev
			}
		}
	}

	out := make([]DeliveryView, 0, len(deliveries))
	for i, d := range deliveries {
		ev := tree[d.WebhookEventID]
		view := DeliveryView{
			ID:                   d.ID,
			EventID:              d.WebhookEventID,
			EndpointID:           d.WebhookEndpointID,
			EndpointURL:          d.EndpointURL,
			EndpointDescription:  d.EndpointDescription,
			AttemptNo:            i + 1,
			Status:               string(d.Status),
			StatusCode:           d.HTTPStatusCode,
			ResponseBody:         truncateBody(d.ResponseBody),
			Error:                d.ErrorMessage,
			RequestPayloadSHA256: hashPayload(ev.Payload),
			AttemptedAt:          d.CreatedAt,
			CompletedAt:          d.CompletedAt,
			NextRetryAt:          d.NextRetryAt,
		}
		if ev.ReplayOfEventID != nil && *ev.ReplayOfEventID != "" {
			view.IsReplay = true
			view.ReplayEventID = ev.ID
		}
		out = append(out, view)
	}

	respond.JSON(w, r, http.StatusOK, DeliveriesResponse{
		RootEventID: rootID,
		Deliveries:  out,
	})
}
