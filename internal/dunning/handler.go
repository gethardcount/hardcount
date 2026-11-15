package dunning

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/activity"
)

// EventLiner words a payment-recovery run's events as activity lines
// (ADR-128): the lines the invoice's Activity card shows for the same
// events, without that page's folds. lines is keyed by event id; degraded
// names each side lane that could not be read. Satisfied by the invoice
// handler, wired at the composition root: the words and the sources they
// read (the invoice, its emails and charge attempts) live there.
type EventLiner interface {
	RecoveryRunLines(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, events []domain.InvoiceDunningEvent) (lines map[string]activity.Line, degraded []string, err error)
}

type Handler struct {
	svc         *Service
	auditLogger AuditWriter
	eventLiner  EventLiner
}

// SetEventLiner wires the words of the recovery timeline (lazy: the invoice
// handler is built after this one). Unwired, events carry no line.
func (h *Handler) SetEventLiner(l EventLiner) { h.eventLiner = l }

// AuditWriter is the narrow audit surface dunning handler uses.
// Decoupled from internal/audit so the handler can be tested with
// a fake; wired in router.go via SetAuditLogger.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// SetAuditLogger wires the audit logger so dunning policy changes land in
// audit_log.
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	// Dunning policies (ADR-036 campaigns model — multi-policy-per-
	// tenant). Replaces the prior singleton /policy + per-customer
	// /customers/{id}/override surface. Customers are reassigned via
	// PATCH /v1/customers/{id} { "dunning_policy_id": ... } on the
	// customer handler.
	r.Route("/policies", func(r chi.Router) {
		r.Get("/", h.listPolicies)
		r.Post("/", h.createPolicy)
		r.Get("/{id}", h.getPolicy)
		r.Patch("/{id}", h.updatePolicy)
		r.Delete("/{id}", h.deletePolicy)
		r.Post("/{id}/set-default", h.setDefaultPolicy)
	})

	r.Route("/runs", func(r chi.Router) {
		r.Get("/", h.listRuns)
		r.Get("/{id}", h.getRun)
	})

	// /stats backs the dashboard's stat cards. Aggregate query — no
	// pagination, no client-side derivation from a sliced /runs list.
	r.Get("/stats", h.getStats)

	return r
}

func (h *Handler) getStats(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	stats, err := h.svc.GetStats(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get dunning stats", "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, stats)
}

func (h *Handler) listPolicies(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	// A tenant's policies are read whole and paged here.
	policies, err := h.svc.ListPolicies(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list dunning policies", "error", err)
		return
	}
	if policies == nil {
		policies = []domain.DunningPolicy{}
	}
	window := respond.Window(policies, page)
	out := make([]PolicyListItem, 0, len(window))
	for _, p := range window {
		count, _ := h.svc.CountCustomersOnPolicy(r.Context(), tenantID, p.ID)
		out = append(out, PolicyListItem{DunningPolicy: p, AssignedCustomers: count})
	}
	respond.List(w, r, out, len(policies))
}

func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	policy, err := h.svc.GetPolicyByID(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "dunning_policy")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get dunning policy", "id", id, "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, policy)
}

// policyBody is a policy payload: the policy's fields, plus the keys it
// replaced, declared only so they can be refused by name with what to send
// instead: the pre-ADR-112 `final_action`, and the three schedule fields
// that retry_days replaced (2026-10-23). A caller still sending
// `final_action: "cancel_subscription"` would otherwise store a policy that
// pauses and never writes off.
type policyBody struct {
	domain.DunningPolicy
	FinalAction      *json.RawMessage `json:"final_action"`
	RetrySchedule    *json.RawMessage `json:"retry_schedule"`
	MaxRetryAttempts *json.RawMessage `json:"max_retry_attempts"`
	GracePeriodDays  *json.RawMessage `json:"grace_period_days"`
}

// policyReadOnly are the policy fields the server sets: set-default moves
// is_default, and an edit clears seeded (ADR-123). A body that sends one is
// refused, naming it.
var policyReadOnly = respond.ReadOnly("id", "tenant_id", "is_default", "seeded", "created_at", "updated_at")

// decodePolicyBody decodes a policy payload strictly (respond.DecodeBody)
// and refuses the replaced keys by name. It writes the refusal and returns
// false; the handler returns on false.
func decodePolicyBody(w http.ResponseWriter, r *http.Request) (domain.DunningPolicy, bool) {
	var body policyBody
	if err := respond.DecodeBody(r, &body, policyReadOnly); err != nil {
		var typeErr *json.UnmarshalTypeError
		// Field is the key's path in the body: retry_days, or retry_days.1
		// for an entry.
		if errors.As(err, &typeErr) && strings.HasPrefix(typeErr.Field, "retry_days") {
			respond.ValidationField(w, r, "retry_days", "retry_days is a list of whole days, for example [3, 4, 7]")
			return domain.DunningPolicy{}, false
		}
		respond.WriteBodyError(w, r, err)
		return domain.DunningPolicy{}, false
	}
	if body.FinalAction != nil {
		respond.ValidationField(w, r, "final_action",
			"final_action is no longer accepted — it decided two separate things. Send final_subscription_action (none|pause|cancel) and final_invoice_action (none|mark_uncollectible) instead")
		return domain.DunningPolicy{}, false
	}
	for _, old := range []struct {
		field string
		sent  bool
	}{
		{"grace_period_days", body.GracePeriodDays != nil},
		{"retry_schedule", body.RetrySchedule != nil},
		{"max_retry_attempts", body.MaxRetryAttempts != nil},
	} {
		if old.sent {
			respond.ValidationField(w, r, old.field,
				old.field+" is no longer accepted. Send retry_days instead: the whole days to wait before each retry, the first counted from the failed payment (or the collection that found no card), for example [3, 4, 7] for retries on day 3, 7 and 14")
			return domain.DunningPolicy{}, false
		}
	}
	return body.DunningPolicy, true
}

func (h *Handler) createPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	policy, ok := decodePolicyBody(w, r)
	if !ok {
		return
	}
	result, err := h.svc.UpsertPolicy(r.Context(), tenantID, policy)
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		// A create can take the default (the first policy, or the first
		// enabled one after the account's seeded policy, ADR-123). The old
		// default's demotion writes no row of its own, so this row is the
		// only record of why the default moved.
		var metadata map[string]any
		if result.IsDefault {
			metadata = map[string]any{"became_default": true}
		}
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "dunning_policy", result.ID, result.Name, metadata)
	}
	respond.JSON(w, r, http.StatusCreated, result)
}

func (h *Handler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	policy, ok := decodePolicyBody(w, r)
	if !ok {
		return
	}
	policy.ID = id
	result, err := h.svc.UpsertPolicy(r.Context(), tenantID, policy)
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "dunning_policy", result.ID, result.Name, nil)
	}
	respond.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	err := h.svc.DeletePolicy(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "dunning_policy")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionDelete, "dunning_policy", id, "", nil)
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) setDefaultPolicy(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.svc.SetDefaultPolicy(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "dunning_policy")
			return
		}
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "dunning_policy", id, "", map[string]any{
			"action": "set_default",
		})
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "default_updated"})
}

func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	// ?state= is a closed set: an unknown value matched no row and read as
	// "none of those".
	state := r.URL.Query().Get("state")
	switch domain.DunningRunState(state) {
	case "", domain.DunningActive, domain.DunningEscalated, domain.DunningResolved:
	default:
		respond.ValidationField(w, r, "state", "state must be one of active, escalated, resolved")
		return
	}

	runs, total, err := h.svc.ListRuns(r.Context(), RunListFilter{
		TenantID:  tenantID,
		InvoiceID: r.URL.Query().Get("invoice_id"),
		State:     state,
		Limit:     page.Limit,
		Offset:    page.Offset,
	})
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list dunning runs", "error", err)
		return
	}
	if runs == nil {
		runs = []domain.InvoiceDunningRun{}
	}

	respond.List(w, r, runs, total)
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	run, err := h.svc.store.GetRun(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "dunning run")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get dunning run", "error", err)
		return
	}

	// A lane that cannot be read is named in `degraded`, never silently
	// dropped: an empty recovery timeline must not read as "nothing
	// happened".
	degraded := []string{}
	events, err := h.svc.store.ListEvents(r.Context(), tenantID, id)
	if err != nil {
		slog.ErrorContext(r.Context(), "get dunning run: list events", "run_id", id, "error", err)
		degraded = append(degraded, "events")
		events = nil
	}

	var lines map[string]activity.Line
	if h.eventLiner != nil && len(events) > 0 {
		l, d, lerr := h.eventLiner.RecoveryRunLines(r.Context(), tenantID, run, events)
		if lerr != nil {
			slog.ErrorContext(r.Context(), "get dunning run: activity lines", "run_id", id, "error", lerr)
			degraded = append(degraded, "invoice")
		}
		lines = l
		degraded = append(degraded, d...)
	}
	out := make([]RunEvent, 0, len(events))
	for _, evt := range events {
		e := RunEvent{InvoiceDunningEvent: evt}
		if l, ok := lines[evt.ID]; ok {
			e.Line = &l
		}
		out = append(out, e)
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"run":      run,
		"events":   out,
		"degraded": degraded,
	})
}

// PolicyListItem is one policy as GET /v1/dunning/policies lists it. It
// carries the number of customers assigned to the policy, so the admin page
// can show "N customers assigned" without a request per row.
type PolicyListItem struct {
	domain.DunningPolicy
	AssignedCustomers int `json:"assigned_customers"`
}

// RunEvent is one event of a run on the wire: the event, and its activity
// line (ADR-128), in the words the invoice's Activity card uses for it.
type RunEvent struct {
	domain.InvoiceDunningEvent
	Line *activity.Line `json:"line,omitempty"`
}

// Customer override handlers (GetCustomerOverride / UpsertCustomerOverride
// / DeleteCustomerOverride) were removed in ADR-036. Per-customer
// differentiation is now expressed as `customers.dunning_policy_id`
// assignment; mutation goes through PATCH /v1/customers/{id} on the
// customer handler.
