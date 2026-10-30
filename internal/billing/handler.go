package billing

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// The entries POST /v1/billing/run puts in `errors`. api/openapi.yaml names
// each one in the 206 description.
const (
	// RunErrSubscription is a subscription that could not be billed; %s is
	// its id.
	RunErrSubscription = "subscription %s: billing failed"
	// RunErrStopped is a run that stopped before it reached every due
	// subscription: the due list could not be read, or the request ended.
	RunErrStopped = "billing run failed"
	// RunErrAudit is a run whose audit record could not be written.
	RunErrAudit = "billing run completed but its audit record failed"
)

type Handler struct {
	engine *Engine
}

func NewHandler(engine *Engine) *Handler {
	return &Handler{engine: engine}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/run", h.triggerCycle)
	return r
}

func (h *Handler) triggerCycle(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		// Every key and session carries a tenant (platform keys are gone,
		// ADR-133), but a request without one must still fail closed — NEVER
		// fall through to the unscoped, cross-tenant RunCycle (the pre-fix
		// leak). RunCycle stays scheduler-only.
		respond.Forbidden(w, r, "billing run requires a key scoped to one account")
		return
	}

	generated, failures := h.engine.RunCycleForTenant(r.Context(), tenantID, 50)

	// Full detail (pq constraint names, Stripe internals) goes to the server log
	// only; the API caller gets its own subscription ids + a generic class. Even
	// a tenant's OWN raw errors leak DB/provider internals, so they are stripped.
	errStrings := make([]string, 0, len(failures))
	for _, f := range failures {
		slog.ErrorContext(r.Context(), "billing run: subscription failed",
			"tenant_id", tenantID,
			"subscription_id", f.SubscriptionID,
			"error", f.Err,
		)
		if f.SubscriptionID != "" {
			errStrings = append(errStrings, fmt.Sprintf(RunErrSubscription, f.SubscriptionID))
		} else {
			errStrings = append(errStrings, RunErrStopped)
		}
	}

	// ADR-090: the operator's TRIGGER gets its own row. The per-invoice
	// finalize rows this run writes cannot answer "who started this cycle" —
	// they are byte-identical whether the scheduler or an operator drove it.
	//
	// Residual OWN-TX emission, deliberately post-effect: the route owns no
	// transaction (each invoice already committed inside the engine, with its
	// own in-tx evidence), so there is nothing left to share fate with. The row
	// therefore records what the run DID — the invoice count it actually
	// produced — which is only knowable after it returns. A successful Log
	// self-marks the request as accounted-for.
	if aw := h.engine.auditWriter(); aw != nil {
		if err := aw.Log(r.Context(), tenantID, domain.AuditActionRun, "billing", "", "", map[string]any{
			"action":           "cycle_run_triggered",
			"invoices_created": generated,
			"failures":         len(failures),
		}); err != nil {
			// Never swallowed (`_ =` is the pattern ADR-090 kills). The run is
			// already committed, so the response keeps its invoice count rather
			// than lying with a 500 — but the caller is TOLD the trigger could
			// not be recorded, and the request stays UNMARKED so the
			// AuditCoverage detector reports the uncovered mutation.
			slog.ErrorContext(r.Context(), "billing run: audit trigger row failed",
				"tenant_id", tenantID, "error", err)
			errStrings = append(errStrings, RunErrAudit)
		}
	}

	// Status keys off the surfaced error list, not the failure slice: an
	// unrecorded trigger is a partial outcome too, and a 200 alongside a
	// non-empty `errors` array would be a lie.
	status := http.StatusOK
	if len(errStrings) > 0 {
		status = http.StatusPartialContent
	}
	respond.JSON(w, r, status, map[string]any{
		"invoices_generated": generated,
		"errors":             errStrings,
	})
}
