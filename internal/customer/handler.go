package customer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

type Handler struct {
	svc         *Service
	auditLogger *audit.Logger
	costSvc     CostDashboardService
	sentEmails  SentEmailsLister
	// apiBaseURL is the public-facing origin used to compose the
	// `public_url` field on the rotate-cost-dashboard-token response.
	// Empty → the operator gets back just the relative path; production
	// wires the real origin via SetAPIBaseURL.
	apiBaseURL string
}

// SentEmailRow is the wire-shape of a single email_outbox row returned
// from GET /v1/customers/{id}/sent-emails. Mirrors Stripe's customer-
// page "Sent emails" section. Fields stay flat (no nested payload) so
// the SPA can render directly.
type SentEmailRow struct {
	ID        string `json:"id"`
	EmailType string `json:"email_type"`
	Recipient string `json:"recipient"`
	Status    string `json:"status"`
	// DeliveryState is the provider-confirmed outcome (ADR-098):
	// unknown / delivered / bounced / complained. Layered over Status
	// in the UI — 'dispatched' alone only means the relay took it.
	DeliveryState string  `json:"delivery_state"`
	InvoiceNumber string  `json:"invoice_number,omitempty"`
	LastError     string  `json:"last_error,omitempty"`
	CreatedAt     string  `json:"created_at"`
	DispatchedAt  *string `json:"dispatched_at,omitempty"`
	// Line is the row as the activity line standard words it (ADR-128):
	// the same email words the invoice's Activity card uses, dated on the
	// customer's calendar with the real send time as its Recorded time on
	// a test clock.
	Line *activity.Line `json:"line,omitempty"`
}

// SentEmailsLister returns email_outbox rows for a customer (30-day
// window, newest first). Implemented at the router layer via an
// adapter over *email.OutboxStore so the customer package doesn't
// import email (peer-domain rule per CLAUDE.md).
type SentEmailsLister interface {
	ListByCustomer(ctx context.Context, tenantID, customerID string) ([]SentEmailOutboxRow, error)
}

// SentEmailOutboxRow is the cross-package shape this handler reads.
// Mirrors the customer-relevant fields of email.OutboxRow; the router
// adapter translates one to the other.
type SentEmailOutboxRow struct {
	ID        string
	EmailType string
	Recipient string // resolved from payload->>'to'
	Status    string
	// DeliveryState is the provider-confirmed outcome layered over
	// Status (ADR-098): unknown / delivered / bounced / complained.
	DeliveryState string
	LastError     string
	CreatedAt     time.Time
	DispatchedAt  *time.Time
	// InvoiceNumber is resolved from payload — non-empty for all
	// invoice-scoped email types (which is everything the lister
	// returns today, since ListByCustomer joins on invoice_number).
	InvoiceNumber string
	// InvoiceID is the invoice InvoiceNumber names, for the line's link.
	InvoiceID string
	// CreditNoteNumber is the credit note a credit-note email sent (payload
	// credit_note_number), and CreditNoteID its id while it exists.
	CreditNoteNumber string
	CreditNoteID     string
	// Trigger is why a payment setup email was sent (payload trigger).
	Trigger string
	// SendFailure is a failed row's error classified into one the line
	// names in plain words (activity.SendFailure*), or empty.
	SendFailure string
	// SkipReason is the state the invoice reached that made Velox skip a
	// queued email, or empty.
	SkipReason string
	// SimEffectiveAt is the instant on the customer's test clock that
	// caused the email (ADR-104); nil when not caused on a test clock.
	SimEffectiveAt *time.Time
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger wires the audit logger. Currently used by the
// cost-dashboard token rotate endpoint (record that a rotation happened
// without leaking the plaintext token into the audit trail). Nil-safe —
// callers that don't audit just skip the entry.
func (h *Handler) SetAuditLogger(l *audit.Logger) {
	h.auditLogger = l
}

// SetAPIBaseURL wires the public-facing API origin used to compose
// the `public_url` field on the rotate-cost-dashboard-token response.
// Empty is OK — the response then carries just the relative API path
// and the operator composes the full URL themselves.
func (h *Handler) SetAPIBaseURL(u string) {
	h.apiBaseURL = u
}

// SetSentEmailsLister wires the email-outbox lister used by
// GET /v1/customers/{id}/sent-emails. Nil-safe — when unwired, the
// endpoint returns 503 to make the misconfiguration loud rather than
// silently returning an empty list.
func (h *Handler) SetSentEmailsLister(l SentEmailsLister) {
	h.sentEmails = l
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Patch("/{id}", h.update)
	r.Route("/{id}/billing-profile", func(r chi.Router) {
		r.Put("/", h.upsertBillingProfile)
		r.Get("/", h.getBillingProfile)
	})
	r.Post("/{id}/rotate-cost-dashboard-token", h.rotateCostDashboardToken)
	r.Get("/{id}/sent-emails", h.listSentEmails)
	// GDPR endpoints (data export + right to erasure) were removed
	// 2026-05-29 pre-launch — the prior half-implementation didn't
	// propagate erasure to Stripe (so PII survived in the Stripe
	// Customer object) and lacked the audit + acknowledgement-window
	// machinery a real regulator-facing GDPR flow needs. Rebuild
	// when the first EU-targeting DP defines actual regulatory scope.
	return r
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	customer, err := h.svc.Create(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "customer")
		return
	}

	respond.JSON(w, r, http.StatusCreated, customer)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// `ids` filter (comma-separated) lets other list pages
	// (Invoices, Subscriptions, etc.) fetch exactly the customers
	// referenced by their primary rows. Avoids the "list-then-
	// client-side-join" pagination bug that surfaces "Unknown" on
	// rows whose customer fell off the default 50-row page. Bounded
	// by the same 100-row limit cap as any other list.
	var ids []string
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}

	filter := ListFilter{
		TenantID:   tenantID,
		Status:     r.URL.Query().Get("status"),
		ExternalID: r.URL.Query().Get("external_id"),
		Search:     strings.TrimSpace(r.URL.Query().Get("search")),
		IDs:        ids,
		Limit:      limit,
		Offset:     offset,
		Sort:       r.URL.Query().Get("sort"),
		SortDir:    r.URL.Query().Get("dir"),
	}

	customers, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list customers", "error", err)
		return
	}

	if customers == nil {
		customers = []domain.Customer{}
	}

	respond.List(w, r, customers, total)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	customer, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "customer")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get customer", "error", err)
		return
	}

	respond.JSON(w, r, http.StatusOK, customer)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input UpdateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	customer, err := h.svc.Update(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "customer")
		return
	}

	// Customer updates (display_name, email, dunning policy, status)
	// are operator-visible state changes. An email change can break
	// invoice delivery; a dunning-policy reassignment changes when
	// auto-collection fires. Without the audit row the AuditLog page
	// shows nothing when ops asks "who changed this customer's email?".
	if h.auditLogger != nil {
		meta := map[string]any{}
		if input.DisplayName != "" {
			meta["display_name"] = input.DisplayName
		}
		// Record THAT the email changed, never the address itself: audit_log
		// is append-only (DB-trigger enforced), so customer PII written here
		// can never be erased — incompatible with a GDPR deletion request.
		// The current address lives on the (mutable, erasable) customer row
		// this audit row links to.
		if input.Email != "" {
			meta["email_changed"] = true
		}
		if input.Status != "" {
			meta["status"] = input.Status
		}
		if input.DunningPolicyID != nil {
			meta["dunning_policy_id"] = *input.DunningPolicyID
		}
		// Same PII rule as email_changed: record that the CC list
		// changed and its new size, never the addresses.
		if input.AdditionalEmails != nil {
			meta["additional_emails_changed"] = true
			meta["additional_emails_count"] = len(customer.AdditionalEmails)
		}
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), tenantID, customer.ID), tenantID, domain.AuditActionUpdate, "customer", customer.ID, customer.DisplayName, meta)
	}

	respond.JSON(w, r, http.StatusOK, customer)
}

func (h *Handler) upsertBillingProfile(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "id")

	// Strict: a dropped `tax_exempt` would leave tax_status at its default
	// and tax an exempt customer. Read-only fields from a GET are declared on
	// the type, so a GET body sent back is still accepted.
	var bp domain.CustomerBillingProfile
	if !respond.DecodeStrict(w, r, &bp) {
		return
	}
	bp.CustomerID = customerID

	profile, err := h.svc.UpsertBillingProfile(r.Context(), tenantID, bp)
	if err != nil {
		respond.FromError(w, r, err, "billing profile")
		return
	}

	// Billing-profile changes flow directly into every future invoice
	// computed for the customer (tax_status, tax_id, address used by
	// tax engines). Auditable for both compliance and incident review
	// — a regressed invoice tax line is often traced back to a
	// billing-profile edit.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), tenantID, customerID), tenantID, domain.AuditActionUpdate, "customer", customerID, profile.LegalName, map[string]any{
			"action":     "billing_profile_upserted",
			"tax_status": string(profile.TaxStatus),
			// tax_id is a personal identifier for sole proprietors — record
			// presence only; the value stays on the erasable profile row.
			"tax_id_set": profile.TaxID != "",
			"country":    profile.Country,
		})
	}

	respond.JSON(w, r, http.StatusOK, profile)
}

func (h *Handler) getBillingProfile(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "id")

	profile, err := h.svc.GetBillingProfile(r.Context(), tenantID, customerID)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "billing profile")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get billing profile", "error", err)
		return
	}

	respond.JSON(w, r, http.StatusOK, profile)
}

// rotateCostDashboardToken mints a fresh public cost-dashboard token
// and writes it to the customer row. The old token is invalidated
// immediately (no grace window — read-only surface, rotation intent
// is "stop the previous URL right now").
//
// Response body: { "token": "vlx_pcd_<64 hex>", "public_url": "<url>" }.
// Audit log records the rotation with no plaintext token.
func (h *Handler) rotateCostDashboardToken(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		respond.BadRequest(w, r, "missing customer id")
		return
	}

	token, err := h.svc.RotateCostDashboardToken(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "customer")
		return
	}

	publicURL := "/v1/public/cost-dashboard/" + token
	if h.apiBaseURL != "" {
		publicURL = strings.TrimRight(h.apiBaseURL, "/") + publicURL
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), tenantID, id), tenantID, domain.AuditActionRotate, "customer", id, "", map[string]any{
			"surface": "cost_dashboard_token",
		})
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{
		"token":      token,
		"public_url": publicURL,
	})
}

// listSentEmails returns the email_outbox rows for the customer, last
// 30 days, newest first. Powers the "Sent emails" section on the
// customer detail page (Stripe shape — docs.stripe.com/invoicing/
// send-email lists the email log on the customer page).
//
// Returns 503 when the lister isn't wired (production wires via
// router; narrow tests can leave it nil — make the misconfig loud
// rather than silently returning an empty list and hiding a wiring
// bug from the operator).
func (h *Handler) listSentEmails(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		respond.BadRequest(w, r, "missing customer id")
		return
	}
	if h.sentEmails == nil {
		// Same shape as other "expected-wired-in-production" backstops
		// (cost-dashboard token mint, GDPR routes). 500 keeps the
		// wiring bug loud — silent empty list would mask a misconfig.
		slog.ErrorContext(r.Context(), "list sent emails: lister not wired")
		respond.InternalError(w, r)
		return
	}

	rows, err := h.sentEmails.ListByCustomer(r.Context(), tenantID, id)
	if err != nil {
		slog.ErrorContext(r.Context(), "list sent emails", "customer_id", id, "error", err)
		respond.InternalError(w, r)
		return
	}

	out := make([]SentEmailRow, 0, len(rows))
	for _, row := range rows {
		view := SentEmailRow{
			ID:            row.ID,
			EmailType:     row.EmailType,
			Recipient:     row.Recipient,
			Status:        row.Status,
			DeliveryState: row.DeliveryState,
			InvoiceNumber: row.InvoiceNumber,
			LastError:     row.LastError,
			CreatedAt:     row.CreatedAt.Format(time.RFC3339),
		}
		if row.DispatchedAt != nil {
			d := row.DispatchedAt.Format(time.RFC3339)
			view.DispatchedAt = &d
		}
		if l, ok := sentEmailLine(row); ok {
			view.Line = &l
		} else {
			// The lister's allowlist and the email names are one list
			// (TestSentEmailLines_NoFallback), so this needs a new email
			// type listed without a name.
			slog.WarnContext(r.Context(), "list sent emails: email type has no activity words", "email_type", row.EmailType, "outbox_id", row.ID)
		}
		out = append(out, view)
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"sent_emails": out})
}

// sentEmailLine is a Sent emails row's activity line (ADR-128): the email
// words every surface uses (activity.EmailLine), naming the invoice the
// email was about and, for a credit-note email, its credit note. ok is
// false for an email type no surface shows.
func sentEmailLine(row SentEmailOutboxRow) (activity.Line, bool) {
	e := activity.Email{
		Type: row.EmailType, Status: row.Status, DeliveryState: row.DeliveryState,
		SendFailure: row.SendFailure, SkipReason: row.SkipReason, Trigger: row.Trigger, To: row.Recipient,
		CreatedAt: row.CreatedAt, DispatchedAt: row.DispatchedAt, SimEffectiveAt: row.SimEffectiveAt,
	}
	var about []activity.Segment
	if row.InvoiceNumber != "" {
		about = append(about, activity.Segment{reference(activity.LinkInvoice, row.InvoiceID, row.InvoiceNumber)})
	}
	if row.EmailType == "credit_note" && row.CreditNoteNumber != "" {
		about = append(about, activity.Segment{reference(activity.LinkCreditNote, row.CreditNoteID, row.CreditNoteNumber)})
	}
	l, ok := activity.EmailLine(e, about...)
	if !ok {
		return activity.Line{}, false
	}
	l.ID = "email:" + row.ID
	occurred, recorded, pinned := activity.EmailInstant(e)
	return activity.Stamp(l, occurred, recorded, pinned), true
}

// reference names an object: a link while its id is known, else its number
// as plain text (rule 8: no link drawn from a guess).
func reference(kind activity.LinkKind, id, label string) activity.Part {
	if id == "" {
		return activity.T(label)
	}
	return activity.L(kind, id, label)
}

// publicCostDashboard composes a sanitized cost-dashboard projection
// for the customer behind the token in the URL path. Unauthenticated;
// the 256-bit token IS the credential. Returns 401 when the token
// doesn't resolve (anti-enumeration — same 401 for invalid /
// never-existed).
//
// Sanitization: customer PII (email, display_name, external_id,
// metadata, billing_profile) is NEVER on this response. Only
// billing-relevant fields: customer_id, tenant_id, billing_period,
// subscriptions (id + plan name + period only), usage (meter + rules
// + totals), totals, projected_total_cents.
func (h *Handler) publicCostDashboard(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(chi.URLParam(r, "token"))
	if token == "" || !strings.HasPrefix(token, costDashboardTokenPrefix) {
		respond.Unauthorized(w, r, "invalid cost-dashboard token")
		return
	}
	if h.costSvc == nil {
		respond.FromError(w, r, fmt.Errorf("cost dashboard service not wired"), "cost_dashboard")
		return
	}
	proj, err := h.costSvc.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.Unauthorized(w, r, "invalid cost-dashboard token")
			return
		}
		respond.FromError(w, r, err, "cost_dashboard")
		return
	}
	respond.JSON(w, r, http.StatusOK, proj)
}

// SetCostDashboardService wires the public-projection assembler used
// by GET /v1/public/cost-dashboard/{token}. Optional in narrow tests.
func (h *Handler) SetCostDashboardService(s CostDashboardService) {
	h.costSvc = s
}

// PublicCostDashboardRoutes returns the chi router for the
// unauthenticated /v1/public/cost-dashboard/{token} surface. Mounted
// separately in router.go with its own rate-limit bucket.
func (h *Handler) PublicCostDashboardRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{token}", h.publicCostDashboard)
	return r
}

// CostDashboardService is the narrow shape used by the public route
// to compose the sanitized projection. Implemented by the cost-
// dashboard assembler wired in router.go from the existing
// customer-usage service.
type CostDashboardService interface {
	GetByToken(ctx context.Context, token string) (any, error)
}
