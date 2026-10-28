package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/generated"
	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
	"github.com/sagarsuperuser/velox/internal/platform/timeline"
)

// CustomerGetter resolves customer IDs to names and billing profiles for PDF rendering.
type CustomerGetter interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// SettingsGetter reads tenant settings for PDF company info.
type SettingsGetter interface {
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// CreditNoteLaneCap is how many credit notes the timeline's CN lane
// fetches — the creditnote store's hard clamp. The lister interface
// carries no total, so the handler discloses `truncated` when a fetch
// comes back AT the cap (it can't know what fell off, only that
// something may have). The adapter must request exactly this many;
// requesting fewer would trip the disclosure early, more gets clamped
// silently.
const CreditNoteLaneCap = 100

// CreditNoteLister fetches credit notes for an invoice, at most
// CreditNoteLaneCap of them, for the Activity timeline.
type CreditNoteLister interface {
	List(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error)
}

// IssuedCreditNoteLister reads every issued credit note on an invoice, with
// no page limit, for the invoice PDF. It is kept apart from the timeline's
// capped CreditNoteLister: the PDF is a customer document and must print
// every note that changed what the invoice asks for.
type IssuedCreditNoteLister interface {
	ListIssued(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error)
}

// PaymentCharger creates a Stripe PaymentIntent for a finalized invoice on
// an operator's or API caller's request (Collect payment, "Charge now").
type PaymentCharger interface {
	// ChargeInvoiceNow records its attempt as charge_now, not auto_charge.
	// hold is the charge hold the caller's claim returned (ADR-127): the
	// charge re-checks it right before sending and sends nothing without it.
	ChargeInvoiceNow(ctx context.Context, tenantID string, inv domain.Invoice, hold domain.ChargeHold, stripeCustomerID, stripePaymentMethodID string) (domain.Invoice, error)
}

// PaymentSetupGetter checks if a customer has a payment method ready.
type PaymentSetupGetter interface {
	GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error)
}

// AuditStampFetcher returns the audit entries for one resource — the
// wall-clock record of its operator/engine transitions, and who made
// each one. The timeline uses it as READ-TIME enrichment:
//
//   - the invoice's own rows (resource "invoice") give its lifecycle rows
//     their actor (ADR-128 rule 4) and, on a test clock, their Recorded
//     stamp (ADR-104 Invariant A): lifecycle rows are derived from invoice
//     state columns, whose stamps follow the entity's calendar (ADR-030),
//     so their real-world moment and actor live only in the audit log;
//   - a credit note's rows (resource "credit_note") give its issue actor
//     and a failed refund's provider reason;
//   - a payment-recovery run's rows (resource "dunning_run") give the
//     actor of an operator's resolve.
//
// Exact keys only (action + the frozen-vocabulary metadata
// discriminator), never a time-window match. Consumer-defined; backed by
// audit.Logger.Query.
type AuditStampFetcher interface {
	ListByResource(ctx context.Context, tenantID, resourceType, resourceID string) ([]domain.AuditEntry, error)
}

// SubscriptionLabeler names an invoice's subscription for the link on its
// activity rows (ADR-128 rule 8). errs.ErrNotFound when it no longer
// exists, in which case no link is drawn.
type SubscriptionLabeler interface {
	SubscriptionLabel(ctx context.Context, tenantID, subscriptionID string) (string, error)
}

// NoPaymentMethodNotifier emails the customer a payment-update link when a
// finalized invoice can't be auto-charged because no payment method is on
// file. Structurally identical to the billing engine's notifier of the same
// name (wired to the same adapter in router.go) — declared locally so the
// invoice package doesn't import the billing engine (zero cross-domain
// imports). Optional; nil means no-PM finalize just queues for retry.
type NoPaymentMethodNotifier interface {
	NotifyNoPaymentMethod(ctx context.Context, tenantID string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error)
}

// Collector collects one finalized invoice now: credits, then the saved card,
// or the setup-link email when there is none. Satisfied by the billing
// engine's CollectInvoice, which is the same collector the auto-charge sweep
// runs. Declared here so the invoice package doesn't import billing. at nil
// means "the invoice's clock now".
type Collector interface {
	CollectInvoice(ctx context.Context, tenantID, invoiceID string, at *time.Time)
}

// PaymentCanceler stops an invoice from being payable at Stripe. Satisfied by
// *payment.Stripe.
//
// StopCollection is a void's: expire any live Checkout session, then cancel
// the PaymentIntent if Stripe allows it (it refuses for Checkout-owned PIs —
// the session expiry is what stops those). Every void runs it from
// Service.AfterVoid.
//
// ExpireCheckoutSessions is a write-off's: the same session expiry, without
// the PaymentIntent cancel. Service.MarkUncollectible runs it.
type PaymentCanceler interface {
	StopCollection(ctx context.Context, tenantID string, inv domain.Invoice)
	ExpireCheckoutSessions(ctx context.Context, tenantID string, inv domain.Invoice)
}

// BillingProfileGetter reads customer billing profile for PDF.
type BillingProfileGetter interface {
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// DunningResolver resolves active dunning runs when an invoice is voided or
// paid: Service.AfterVoid on every void, and this handler on a write-off or
// an offline payment.
type DunningResolver interface {
	ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error
}

// DunningTimelineFetcher fetches dunning data for payment timeline.
type DunningTimelineFetcher interface {
	ListRunsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDunningRun, error)
	ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error)
}

// EmailSender sends invoice-related emails. ctx must carry livemode
// (set by auth middleware) so the underlying enqueue / brand lookup
// stamps the right tenant_settings + email_outbox row.
type EmailSender interface {
	SendInvoice(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, totalCents int64, currency string, pdfBytes []byte, publicToken string) error
}

// EmailEventLister surfaces customer-notification email rows
// (queued/dispatched/failed) tied to an invoice for the timeline.
// Without this, operators have no signal that the customer was
// notified — the no-PM finalize email goes out asynchronously and
// the only trace is the email_outbox row. Satisfied by
// email.OutboxStore.ListByInvoice.
type EmailEventLister interface {
	ListByInvoice(ctx context.Context, tenantID, invoiceNumber string) ([]EmailEventRow, error)
}

// EmailEventRow is the timeline-friendly view of an email_outbox row.
// Trimmed to the fields the timeline renderer needs.
type EmailEventRow struct {
	// ID is the outbox row id — the timeline's stable per-row key
	// (same-instant rows on a frozen clock made composite timestamp
	// keys collide).
	ID        string
	EmailType string
	Status    string // pending / dispatched / failed / skipped
	// DeliveryState is the provider-confirmed outcome layered over
	// Status (ADR-098): unknown / delivered / bounced / complained.
	DeliveryState string
	CreatedAt     time.Time
	DispatchedAt  *time.Time
	LastError     string
	To            string // resolved from payload
	// AttemptNumber pairs a dunning_warning email with the retry
	// attempt it warns about (payload attempt_number = run.AttemptCount
	// at send time). 0 for non-dunning types and legacy rows.
	AttemptNumber int
	// Trigger names why the row was enqueued (payload trigger), for
	// types where two visually-identical rows can share one invoice —
	// today the setup-link email: finalize_no_pm /
	// auto_charge_retry_no_pm / operator_resend. Empty for legacy rows
	// and types that carry no trigger.
	Trigger string
	// SimEffectiveAt is the billing-axis anchor stamped at enqueue
	// (ADR-104): the simulated instant that caused the send. Nil for
	// live-mode mail and for legacy rows enqueued before the anchor
	// existed — those render at their wall stamp.
	SimEffectiveAt *time.Time
	// SendFailure classifies a failed row's error into one the activity
	// line can name in plain words (activity.SendFailure*); empty when the
	// row did not fail or its error has no plain-words name. The raw error
	// stays in LastError and never reaches the screen.
	SendFailure string
	// SkipReason is the state the invoice reached that made the dispatcher
	// skip a queued email (email_outbox.skip_reason): paid, voided,
	// uncollectible or gone. Empty for rows not skipped, and for rows
	// skipped before the reason was recorded.
	SkipReason string
	// CreditNoteNumber is the credit note a credit-note email sent (payload
	// credit_note_number); empty for other types.
	CreditNoteNumber string
}

// RefundIssuer issues a direct refund on a paid invoice. Concretely this
// creates + issues a refund credit note atomically; the handler doesn't need
// to know about credit notes as a data model. Backed by creditnote.Service.
type RefundIssuer interface {
	IssueRefund(ctx context.Context, tenantID string, input RefundInput) (domain.CreditNote, error)
}

// RefundInput is the handler-facing refund request. AmountCents=0 means
// "refund the full remaining refundable amount".
type RefundInput struct {
	InvoiceID   string
	AmountCents int64
	Reason      string
	Description string
}

// validRefundReasons matches Stripe's refund reason enum plus "other" as the
// catch-all. Constrained at the edge so the UI can render a dropdown and the
// audit log has a stable vocabulary.
var validRefundReasons = map[string]bool{
	"duplicate":             true,
	"fraudulent":            true,
	"requested_by_customer": true,
	"other":                 true,
}

// getInvoiceServer is the slice of generated.ServerInterface that this
// handler currently implements — the single OpenAPI operation
// `getInvoice` (GET /v1/invoices/{id}). Two compile-time assertions
// keep the spec, the generated interface, and the handler aligned: the
// handler satisfies this slice, AND generated.ServerInterface (rebuilt
// from api/openapi.yaml by `make gen`) still carries this exact method
// signature — so renaming or re-typing the operation in the spec fails
// the build here instead of surfacing as a runtime 404. The naive form
// `var _ generated.ServerInterface = (*Handler)(nil)` cannot be used:
// the handler implements one of ~60 generated operations. As more
// operations migrate onto the typed surface, this slice broadens.
type getInvoiceServer interface {
	GetInvoice(w http.ResponseWriter, r *http.Request, id string)
}

var (
	_ getInvoiceServer = (*Handler)(nil)                // handler ⊇ slice
	_ getInvoiceServer = generated.ServerInterface(nil) // generated ⊇ slice
)

type Handler struct {
	svc             *Service
	customers       CustomerGetter
	settings        SettingsGetter
	creditNotes     CreditNoteLister
	pdfCreditNotes  IssuedCreditNoteLister
	charger         PaymentCharger
	paymentSetups   PaymentSetupGetter
	dunning         DunningResolver
	emailEvents     EmailEventLister
	dunningTimeline DunningTimelineFetcher
	emailSender     EmailSender
	refundIssuer    RefundIssuer
	auditLogger     auditWriter
	noPMNotifier    NoPaymentMethodNotifier
	auditStamps     AuditStampFetcher
	subscriptions   SubscriptionLabeler
	collector       Collector
	// emailConfigured is whether this server can send email (SMTP_HOST set).
	// Sends are queued, so a queued email on a server with no SMTP fails
	// later, out of the operator's sight. The send and resend actions refuse
	// up front instead. Zero value refuses: a missing wire-up never claims
	// "sent".
	emailConfigured bool
}

// auditWriter is the narrow audit-write interface the invoice handler uses.
// *audit.Logger satisfies it; declared as an interface (vs the concrete
// logger) so the handler's audit rows — action, label, metadata — are
// unit-testable with a capturing fake.
type auditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

type HandlerDeps struct {
	CreditNotes CreditNoteLister
	// IssuedCreditNotes feeds the invoice PDF (download and email).
	IssuedCreditNotes IssuedCreditNoteLister
	Charger           PaymentCharger
	PaymentSetups     PaymentSetupGetter
	Dunning           DunningResolver
	EmailEvents       EmailEventLister
	DunningTimeline   DunningTimelineFetcher
	RefundIssuer      RefundIssuer
}

func NewHandler(svc *Service, customers CustomerGetter, settings SettingsGetter, deps ...HandlerDeps) *Handler {
	h := &Handler{svc: svc, customers: customers, settings: settings}
	if len(deps) > 0 {
		h.creditNotes = deps[0].CreditNotes
		h.pdfCreditNotes = deps[0].IssuedCreditNotes
		h.charger = deps[0].Charger
		h.paymentSetups = deps[0].PaymentSetups
		h.dunning = deps[0].Dunning
		h.emailEvents = deps[0].EmailEvents
		h.dunningTimeline = deps[0].DunningTimeline
		h.refundIssuer = deps[0].RefundIssuer
	}
	return h
}

// SetEmailSender configures email sending for invoice notifications.
func (h *Handler) SetEmailSender(sender EmailSender) {
	h.emailSender = sender
}

// SetEmailConfigured records whether this server can send email
// (email.Sender.IsConfigured at boot). Without it, send and resend-setup-link
// answer 409 email_not_configured.
func (h *Handler) SetEmailConfigured(configured bool) { h.emailConfigured = configured }

// SetAuditStamps wires the timeline's audit enrichment (ADR-104, ADR-128).
func (h *Handler) SetAuditStamps(f AuditStampFetcher) { h.auditStamps = f }

// SetSubscriptionLabeler wires the subscription name the timeline links to
// (ADR-128). Optional: when nil, rows carry no subscription link.
func (h *Handler) SetSubscriptionLabeler(l SubscriptionLabeler) { h.subscriptions = l }

// SetNoPaymentMethodNotifier wires the dispatcher behind the operator's
// "resend payment setup link" action. Optional; nil disables the resend.
func (h *Handler) SetNoPaymentMethodNotifier(n NoPaymentMethodNotifier) {
	h.noPMNotifier = n
}

// SetCollector wires the collector that finalize calls right after the
// commit. Optional; nil leaves a finalized invoice to the auto-charge sweep.
func (h *Handler) SetCollector(c Collector) { h.collector = c }

// SetEmailEvents wires the email_outbox lister used by the timeline
// to surface customer-notification events. Optional — when nil, the
// timeline omits the email rows but the rest of it (lifecycle,
// charge attempts, credit notes, dunning) renders unchanged.
func (h *Handler) SetEmailEvents(lister EmailEventLister) {
	h.emailEvents = lister
}

// SetAuditLogger configures audit logging for financial operations.
func (h *Handler) SetAuditLogger(l auditWriter) { h.auditLogger = l }

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Get("/{id}/pdf", h.downloadPDF)
	r.Post("/{id}/finalize", h.finalize)
	r.Post("/{id}/void", h.void)
	r.Post("/{id}/line-items", h.addLineItem)
	r.Post("/{id}/send", h.sendEmail)
	r.Post("/{id}/resend-setup-link", h.resendSetupLink)
	r.Post("/{id}/collect", h.collectPayment)
	r.Post("/{id}/refund", h.refund)
	r.Post("/{id}/retry-tax", h.retryTax)
	r.Post("/{id}/rotate-public-token", h.rotatePublicToken)
	// Stripe-parity offline-payment recovery. Lets the operator mark
	// an unpaid (or uncollectible) invoice as paid without going
	// through a PaymentIntent — for cheque, wire, cash, or any
	// out-of-band collection. Stripe's equivalent is the
	// paid_out_of_band=true flag on POST /v1/invoices/{id}/pay.
	r.Post("/{id}/record-payment", h.recordOfflinePayment)
	// Stripe-parity uncollectible mark — operator-driven path. The
	// dunning automation reaches this same service method via the
	// mark_uncollectible final-action; this endpoint lets the
	// operator do it directly without waiting for retries.
	r.Post("/{id}/mark-uncollectible", h.markUncollectible)
	r.Get("/{id}/payment-timeline", h.paymentTimeline)
	r.Get("/{id}/disputes", h.listDisputes)
	return r
}

// currencyUnavailable answers a create whose currency could not be read: a
// retryable 503, never a guessed currency.
func currencyUnavailable(w http.ResponseWriter, r *http.Request, tenantID, customerID string, err error) {
	slog.ErrorContext(r.Context(), "invoice create: could not read the currency to use; refusing rather than guessing",
		"tenant_id", tenantID, "customer_id", customerID, "error", err)
	respond.Error(w, r, http.StatusServiceUnavailable, "api_error", "currency_unavailable",
		"could not load the customer's billing currency — please retry")
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	// The currency is resolved in Service.Create (ADR-100, ADR-129): an
	// explicit one must agree with the customer's billing profile and live
	// subscriptions (409 customer_currency_conflict); an empty one takes the
	// profile's, then the subscriptions' plan currency, then the workspace
	// default. A read that errors stops the request: guessing minted, during
	// a database blip, a USD invoice for a EUR customer — a wrong invoice
	// nobody notices, where a failed request is retried.
	inv, err := h.svc.Create(r.Context(), tenantID, input)
	if errors.Is(err, ErrCurrencyUnavailable) {
		currencyUnavailable(w, r, tenantID, input.CustomerID, err)
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	respond.JSON(w, r, http.StatusCreated, inv)
}

func (h *Handler) addLineItem(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input AddLineItemInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	item, err := h.svc.AddLineItem(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	respond.JSON(w, r, http.StatusCreated, item)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	// `ids` filter (comma-separated) lets CreditNotes-and-similar
	// pages fetch the exact invoices their primary rows reference,
	// avoiding the "list-then-client-side-join" pagination bug.
	var ids []string
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}

	// Shared ?from / ?to contract (api/timefilter): RFC3339 instants
	// or bare YYYY-MM-DD, inclusive both ends. Malformed input is a
	// loud 400 — silently ignoring it would return an unfiltered list
	// that lies about what the operator asked for.
	createdFrom, createdTo, err := timefilter.ParseRange(r, "from", "to")
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	filter := ListFilter{
		TenantID:       tenantID,
		CustomerID:     r.URL.Query().Get("customer_id"),
		SubscriptionID: r.URL.Query().Get("subscription_id"),
		Status:         r.URL.Query().Get("status"),
		PaymentStatus:  r.URL.Query().Get("payment_status"),
		Search:         strings.TrimSpace(r.URL.Query().Get("search")),
		CreatedFrom:    createdFrom,
		CreatedTo:      createdTo,
		Overdue:        r.URL.Query().Get("overdue") == "true",
		IDs:            ids,
		Limit:          page.Limit,
		Offset:         page.Offset,
		// Sort + direction are validated against a closed set in
		// the store (invoiceOrderBy). Unknown sort keys silently
		// default to created_at; unknown dir defaults to desc.
		Sort:    r.URL.Query().Get("sort"),
		SortDir: r.URL.Query().Get("dir"),
	}
	invoices, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list invoices", "error", err)
		return
	}
	if invoices == nil {
		invoices = []domain.Invoice{}
	}

	respond.List(w, r, invoices, total)
}

// GetInvoice is the OpenAPI-typed handler for `GET /v1/invoices/{id}`.
// Signature matches generated.ServerInterface so the spec, the handler,
// and the router stay aligned at compile time — see the two
// `getInvoiceServer` assertions above (handler ⊇ slice; generated ⊇ slice).
//
// The chi route still calls h.get (which extracts the id via chi.URLParam
// and delegates here), keeping the routing layer unchanged for now. As
// more endpoints adopt this pattern we'll switch the chi mount to use
// the generated route registration helper, which calls these typed
// methods directly.
func (h *Handler) GetInvoice(w http.ResponseWriter, r *http.Request, id string) {
	tenantID := auth.TenantID(r.Context())

	inv, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get invoice", "error", err)
		return
	}
	if items == nil {
		items = []domain.InvoiceLineItem{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"invoice":    inv,
		"line_items": items,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	h.GetInvoice(w, r, chi.URLParam(r, "id"))
}

func (h *Handler) finalize(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Finalize(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// The finalize audit row AND the invoice.finalized webhook are emitted
	// by service.Finalize — the canonical single writer, covering this
	// endpoint AND the tax-retry auto-finalize. Pre-fix the webhook fired
	// from here only, so the tax-retry path silently skipped it.

	// No automatic "here's your invoice" email on finalize. Hardcount
	// auto-charges the saved card (Stripe charge_automatically model), so
	// the customer's touchpoint is the payment receipt on success (fired
	// from the Stripe webhook) or the "set up payment method" email below
	// when there's no card on file — matching cycle invoices and Stripe's
	// auto-charge behavior, where finalizing an auto-charged invoice does
	// NOT email the invoice. Operators can still send it explicitly via
	// POST /{id}/send.

	// Collect now through the billing engine's one collector (credits, then
	// the card, or the setup-link email). The finalize write already queued
	// the invoice, so if this is unwired or the process dies mid-call the
	// auto-charge sweep collects it on its next tick. Bind first (ADR-030 /
	// ADR-104): the setup-link email it may enqueue must carry the invoice's
	// billing anchor, and this runs on the handler's raw ctx.
	if h.collector != nil {
		h.collector.CollectInvoice(h.svc.bindForInvoice(r.Context(), tenantID, inv.ID), tenantID, inv.ID, nil)
		// Respond with the post-collection state (paid, failed, or still
		// queued), as the caller saw it before collection moved here.
		if collected, err := h.svc.Get(r.Context(), tenantID, inv.ID); err == nil {
			inv = collected
		} else {
			slog.WarnContext(r.Context(), "reload after collect failed; responding with the finalized invoice",
				"invoice_id", inv.ID, "error", err)
		}
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	// Service.Void is the whole void: the status flip, the credit reversal
	// and the audit row in one transaction, then AfterVoid's legs (stop
	// collection at Stripe, tax reversal, invoice.voided, close the dunning
	// run), the same for every void path.
	inv, err := h.svc.Void(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	respond.JSON(w, r, http.StatusOK, inv)
}

// resendSetupLink re-emails the customer the payment-METHOD setup link for a
// finalized, unpaid invoice with no card on file — the "Resend setup link"
// nudge on the no_payment_method attention card. It re-sends the SAME email the
// engine auto-sent at finalize (NotifyNoPaymentMethod → Stripe Checkout in
// SETUP mode → engine auto-charges once a card is attached), which matches that
// state's auto-charge-on-attach model. This is deliberately distinct from
// POST /{id}/send, which emails the hosted-invoice "pay this invoice" link
// (Checkout in PAYMENT mode) — a different collection path for states where the
// operator wants the customer to pay directly.
func (h *Handler) resendSetupLink(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	// Finalized-unpaid, or WRITTEN OFF and unpaid. Draft/voided/paid invoices
	// have no setup link to resend.
	//
	// `uncollectible` is admitted because attaching a card is account-scoped
	// payment-method capture, not payment of this invoice — the same
	// distinction ADR-110 drew when it removed the customer's Pay button but
	// deliberately KEPT the "add a payment method" button beside it. Without
	// it, bad-debt recovery had no on-ramp: the operator's charge answers 422
	// "customer has no payment method set up", terminal invoices expose no
	// `attention` so the banner offering this action never renders, and this
	// endpoint 409'd on status — a closed loop around the exact scenario
	// recovery exists for, a customer returning with a NEW card (walked
	// 2026-08-05, FLOW D6).
	//
	// The copy MUST differ: see notifyRecoveryScoped below.
	collectible := (inv.Status == domain.InvoiceFinalized || inv.Status == domain.InvoiceUncollectible) &&
		inv.PaymentStatus != domain.PaymentSucceeded
	if !collectible {
		respond.ConflictCoded(w, r, "", "invoice_not_collectible",
			"setup link can only be resent for an unpaid invoice that is finalized or written off")
		return
	}
	// The setup-link email tells the CUSTOMER "add a payment method and we'll
	// collect it automatically". No charge path collects an invoice whose
	// payment is in flight — for a parked one (ADR-107), not until Stripe has
	// shown no payment for a day. Sending it would be an outbound promise the
	// engine cannot keep yet, triggerable by an operator button (found by the
	// honesty sweep, 2026-07-31).
	if b := domain.PaymentBlocksAction(inv, domain.ActionResendSetupLink); b.Blocked {
		respond.ConflictCoded(w, r, "", b.Code, b.Message)
		return
	}
	if h.noPMNotifier == nil {
		slog.ErrorContext(r.Context(), "resend setup link: no-PM notifier not wired", "invoice_id", inv.ID)
		respond.InternalError(w, r)
		return
	}
	// Refuse before the notifier mints a single-use payment-update token
	// for an email that cannot be delivered.
	if !h.emailConfigured {
		respond.FromError(w, r, errs.EmailNotConfigured(), "invoice")
		return
	}
	// The TRUE cause: an operator clicked Resend. The row used to say
	// "finalize_no_pm" — a finalize that never ran.
	// Operator entry point on a clock-pinned entity — bind so the resent
	// setup-link email carries the invoice's billing anchor (ADR-030).
	resendCtx := h.svc.bindForInvoice(r.Context(), tenantID, inv.ID)
	outcome, err := h.noPMNotifier.NotifyNoPaymentMethod(resendCtx, tenantID, inv, "operator_resend")
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	if outcome == domain.NotifySkippedNoEmail {
		// Pre-fix this fell through to 200 {"status":"sent"} — a success
		// toast for a send that never happened (the notifier's no-email
		// skip was a silent nil). The typed outcome makes the endpoint
		// honest: nothing was sent, tell the operator what works instead.
		respond.ConflictCoded(w, r, "", "no_email_on_file",
			"customer has no email on file — add one on the customer record, or copy the setup link from the customer page and share it directly")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionSend, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"action":         "resend_setup_link",
			"invoice_number": inv.InvoiceNumber,
			"customer_id":    inv.CustomerID,
		})
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "sent"})
}

// markUncollectible is the operator-driven Stripe-parity bad-debt
// write-off. Service-layer write + event already happen inside
// invoice.Service.MarkUncollectible (so the dunning automated path
// and ResolveRun(invoice_not_collectible) get the same contract);
// this handler adds the side-effects that only fire on the direct
// operator action: resolve any active dunning run so retry
// automation halts immediately.
//
// Industry parity: Stripe + Recurly both halt all dunning activity
// when an invoice is marked uncollectible / failed. We resolve the
// active dunning run with ResolutionRetriesExhausted-shape semantics
// (NOT invoice_not_collectible, which would loop back into the
// "also mark invoice uncollectible" branch we just executed).
func (h *Handler) markUncollectible(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.MarkUncollectible(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// The marked_uncollectible audit row is written by service.MarkUncollectible
	// (the canonical writer, with richer metadata). The handler-level write that
	// used to live here — added under the mistaken belief no row existed — made
	// every operator mark-uncollectible produce TWO identical audit rows.

	// Halt dunning automation. Best-effort — failure is logged not
	// surfaced; the invoice transition is the authoritative state
	// change, and dunning runs scan the invoice status on next tick
	// anyway.
	//
	// The resolution NAMES this outcome (0170 split). This site used to pass
	// ResolutionManuallyResolved on the written grounds that "passing the
	// matching resolution would recurse via ResolveRun's cross-flow branch" —
	// which was false, and cost the column its precision for months: the
	// cross-flow branch that calls MarkUncollectible lives on ResolveRun, and
	// this calls ResolveByInvoice, which has no such branch and cannot
	// recurse. Pinned by TestMarkUncollectible_ResolvesRunAsNotCollectible.
	if h.dunning != nil {
		if err := h.dunning.ResolveByInvoice(r.Context(), tenantID, id, domain.ResolutionInvoiceNotCollectible); err != nil {
			slog.WarnContext(r.Context(), "failed to resolve dunning on mark-uncollectible", "invoice_id", id, "error", err)
		}
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// RecordPaymentInput is the POST /v1/invoices/{id}/record-payment body.
type RecordPaymentInput struct {
	Note string `json:"note"`
}

// recordOfflinePayment flips an unpaid (or uncollectible) invoice to
// paid based on operator-recorded out-of-band collection (cheque,
// wire, cash). Stripe-parity: their paid_out_of_band=true flag on
// POST /v1/invoices/{id}/pay surfaces the same recovery path.
//
// Body shape: { "note": "Cheque #1234" } — single optional string.
// Amount is implicit (full amount_due); partial payments deferred to
// when a customer asks. Date stamps as clock.Now (sim-time on
// clock-pinned invoices).
//
// Side-effects: resolves any active dunning run with
// ResolutionPaymentRecovered so the dashboard reflects the recovery
// in the dunning history view.
func (h *Handler) recordOfflinePayment(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input RecordPaymentInput
	if !respond.DecodeStrict(w, r, &input, respond.EmptyBodyOK) {
		return
	}

	inv, err := h.svc.RecordOfflinePayment(r.Context(), tenantID, id, input.Note)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// The payment_recorded audit row is written by service.RecordOfflinePayment
	// (the canonical writer — its row also carries recovered_from_status). The
	// handler-level write that used to live here duplicated it on every call.

	if h.dunning != nil {
		if err := h.dunning.ResolveByInvoice(r.Context(), tenantID, id, domain.ResolutionPaymentRecovered); err != nil {
			slog.WarnContext(r.Context(), "failed to resolve dunning on record-payment", "invoice_id", id, "error", err)
		}
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// rotatePublicToken mints a fresh hosted-invoice-URL token for an invoice,
// invalidating the previous one. Defensive rotation for the case where the
// public URL is ever shared where it shouldn't be (accidentally cc'd on a
// wider thread, pasted into a ticketing system, scraped from an email
// archive leak). Only finalized/paid/voided invoices carry tokens — draft
// invoices return 422, matching the store-level guard in SetPublicToken.
func (h *Handler) rotatePublicToken(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	if inv.Status == domain.InvoiceDraft {
		respond.Error(w, r, http.StatusUnprocessableEntity, "invalid_request_error", "invalid_state",
			"draft invoices do not have a public token — finalize first")
		return
	}

	previousToken := inv.PublicToken
	token, err := GeneratePublicToken()
	if err != nil {
		slog.ErrorContext(r.Context(), "rotate public token: generate", "invoice_id", id, "error", err)
		respond.InternalError(w, r)
		return
	}
	if err := h.svc.SetPublicToken(r.Context(), tenantID, id, token); err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	inv.PublicToken = token

	if h.auditLogger != nil {
		// Audit the rotation but NOT the token values themselves —
		// plaintext tokens in the audit log would turn the log into an
		// attractive target for credential harvesting. Record only that
		// a rotation happened.
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionRotate, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"invoice_number":           inv.InvoiceNumber,
			"customer_id":              inv.CustomerID,
			"field":                    "public_token",
			"previous_token_was_unset": previousToken == "",
		})
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// SendEmailInput is the POST /v1/invoices/{id}/send body.
type SendEmailInput struct {
	Email string `json:"email"`
	// AdditionalEmails overrides the CC list for THIS send
	// (ADR-082): absent → the customer's stored additional_emails;
	// explicit [] → primary only; explicit list → validated exact
	// override. Legacy {email}-only bodies therefore now CC the
	// stored list — the Orb-parity default.
	AdditionalEmails *[]string `json:"additional_emails,omitempty"`
}

func (h *Handler) sendEmail(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var body SendEmailInput
	if !respond.DecodeStrict(w, r, &body) {
		return
	}
	if body.Email == "" {
		respond.ValidationField(w, r, "email", "email is required")
		return
	}
	// The To address gets the same check as a customer's stored email.
	to, err := domain.ValidateEmail("email", body.Email)
	if err != nil {
		respond.FromError(w, r, err, "invoice_email")
		return
	}

	inv, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}

	// This email carries a "View & pay invoice" button. Sending it for an
	// invoice whose payment is unresolved asks the customer to pay something we
	// may already have taken from them — and for a parked invoice (ADR-107) the
	// hosted page will not even offer a Pay button when they arrive.
	if b := domain.PaymentBlocksAction(inv, domain.ActionEmailInvoice); b.Blocked {
		respond.ConflictCoded(w, r, "", b.Code, b.Message)
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	if !h.emailConfigured {
		respond.FromError(w, r, errs.EmailNotConfigured(), "invoice_email")
		return
	}
	// Operator entry point on a clock-pinned entity (ADR-030): bind once
	// here so the enqueued email carries the invoice's billing anchor and
	// lands on the invoice's own calendar (ADR-104). Without this the row
	// enqueues unanchored and sorts by real send time — the exact gap the
	// 2026-07-29 walk caught after the anchor shipped.
	ctx := h.svc.bindForInvoice(r.Context(), tenantID, inv.ID)

	cc, err := resolveSendCC(ctx, h.customers, tenantID, inv.CustomerID, to, body.AdditionalEmails)
	if err != nil {
		respond.FromError(w, r, err, "invoice_email")
		return
	}

	// One shared context builder across emailed/downloaded/hosted PDFs —
	// this path previously hand-rolled a THINNER context (no buyer
	// address/tax id, no credit notes), so the emailed document diverged
	// from the downloaded one.
	bt, ci, cnInfos := BuildPDFContext(ctx, h.customers, h.settings, h.pdfCreditNotes, tenantID, &inv)

	pdfBytes, err := RenderPDF(ctx, inv, items, bt, cnInfos, ci)
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// AmountDueCents, not Total: the email template labels this figure
	// "Amount due", and credits/partial payments make the two differ —
	// telling a customer they owe the pre-credit total is wrong.
	if err := h.emailSender.SendInvoice(ctx, tenantID, to, cc, bt.Name, inv.InvoiceNumber, inv.AmountDueCents, inv.Currency, pdfBytes, inv.PublicToken); err != nil {
		// Sanitize at the boundary — SMTP errors / outbox-store errors
		// would otherwise leak to the operator toast. ADR-026.
		respond.FromError(w, r, err, "invoice_email")
		return
	}

	// Explicit audit row so an operator-initiated send is recorded as
	// "Emailed INV-NNN". (It used to also displace the catch-all middleware's
	// generic "create" row; that middleware is deleted — ADR-090 — so this row is
	// now the only record.)
	// No recipient address in the append-only row (GDPR erasure) — the email
	// outbox is the delivery record; the row links the invoice + customer.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionSend, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"invoice_number": inv.InvoiceNumber,
			"customer_id":    inv.CustomerID,
		})
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "sent"})
}

// resolveSendCC resolves the CC list for an operator-initiated document
// send (ADR-082 tri-state): nil override → the customer's stored
// additional_emails; explicit list (incl. empty) → validated exact
// override against the To address. Exported to the creditnote handler's
// twin via the shared domain.NormalizeAdditionalEmails; kept here as a
// helper because both send handlers in this package family need it.
func resolveSendCC(ctx context.Context, customers CustomerGetter, tenantID, customerID, to string, override *[]string) ([]string, error) {
	if override != nil {
		return domain.NormalizeAdditionalEmails(*override, to)
	}
	if customers == nil {
		// JSON-only handler wiring (tests) — no stored list to default
		// from. Production always wires the customer getter.
		return nil, nil
	}
	cust, err := customers.Get(ctx, tenantID, customerID)
	if err != nil {
		// The stored list is a default, not a hard dependency — but a
		// failed lookup means we can't honor the operator's configured
		// recipients, and silently sending primary-only would be a
		// silent drop. Fail loud; the operator retries.
		return nil, fmt.Errorf("resolve customer additional emails: %w", err)
	}
	// Stored entries equal to the To address are skipped (the operator
	// may have typed one of the CC addresses as the To override).
	kept := cust.AdditionalEmails[:0:0]
	for _, a := range cust.AdditionalEmails {
		if !strings.EqualFold(a, strings.TrimSpace(to)) {
			kept = append(kept, a)
		}
	}
	return kept, nil
}

func (h *Handler) collectPayment(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// finalized only. A written-off invoice is deliberately NOT chargeable
	// here (ADR-113): charge-the-written-off-object is a Stripe-only pattern,
	// and its three refusal gates existed only because the charged object
	// carried stale state. Recovery of a returned customer runs on NORMAL
	// rails — a fresh recovery invoice — and a written-off invoice is settled
	// by RECORDING writers only (offline payment, the stale-hold sweep's settle).
	if inv.Status != domain.InvoiceFinalized {
		respond.Validation(w, r, "can only collect payment on a finalized invoice — for a written-off debt, issue a recovery invoice or record an offline payment")
		return
	}
	if inv.PaymentStatus == domain.PaymentSucceeded {
		respond.Validation(w, r, "invoice is already paid")
		return
	}
	if inv.PaymentStatus == domain.PaymentUnknown {
		// A possibly-succeeded payment is the reconciler's to resolve —
		// blind re-charging an ambiguous outcome is how double charges
		// happen. The claim below also excludes 'unknown'; this gate
		// exists to say WHY instead of a generic conflict.
		//
		// Carries the gate's CODE, not just its message. respond.Validation
		// flattens everything to 422/validation_error, so a walk of all four
		// parked refusals found this one alone answering a different code than
		// void/send/resend — same rule, same sentence, and an API consumer
		// still could not branch on it uniformly. The single source exists to
		// stop exactly that divergence; it only works if callers pass both
		// halves through.
		b := domain.PaymentBlocksAction(inv, domain.ActionCollectPayment)
		respond.ConflictCoded(w, r, "", b.Code, b.Message)
		return
	}
	if inv.AmountDueCents <= 0 {
		respond.Validation(w, r, "invoice has no amount due")
		return
	}

	if h.charger == nil || h.paymentSetups == nil {
		respond.Validation(w, r, "payment provider not configured")
		return
	}

	// Per-invoice charge lease (2026-07-21 snapshot-race audit): manual
	// collect was the one charge initiator outside the mutual-exclusion
	// ring — a concurrent sweep/dunning charge, or a credit apply landing
	// between the read above and the charge below, meant a double charge
	// or an overcharge at the stale pre-credit amount. The claim's CAS
	// re-asserts the chargeable predicate against committed truth.
	hold, err := h.svc.ClaimChargeForManualCollect(r.Context(), tenantID, id)
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	if !hold.Held() {
		// A set hold is a charge running, or one whose result was lost and
		// waits for the stale-hold sweep to confirm it with Stripe (ADR-127).
		// Say so, with the same words void and offline payment use. The read
		// only picks the words; the claim above was the gate.
		if state, serr := h.svc.ChargeHoldState(r.Context(), tenantID, id); serr == nil && state != domain.ChargeHoldNone {
			respond.ConflictCoded(w, r, "", "payment_in_flight", domain.ChargeHoldRefusalMessage)
			return
		}
		respond.Validation(w, r, "a payment for this invoice is already in progress (a charge, or the customer paying on the payment page) or it is no longer chargeable — refresh and retry later")
		return
	}

	// Post-credit truth (ADR-088): drain the balance, then charge the RELOADED
	// remainder — never the handler snapshot. Charging pre-credit would
	// consummate exactly the overcharge the sweep exists to avoid.
	//
	// This runs BEFORE the payment-method check, which is the order the engine
	// sweep has always used (billing/engine.go applies credit, then settles a
	// fully-covered invoice with MarkPaid and no payment method involved). This
	// handler's comment claimed to "mirror the sweep" while doing the opposite:
	// it demanded a card first, so an invoice a customer's balance covered
	// ENTIRELY was refused for want of a card it never needed — and the
	// zero-due branch below, whose whole premise is "settle without a card
	// charge", was unreachable without one.
	//
	// Bad-debt recovery is where that bites hardest: goodwill credit issued
	// during dunning is exactly how a written-off invoice gets covered, and the
	// customer who was written off is the least likely to have a working card
	// on file.
	inv, err = h.svc.ApplyCreditBalance(r.Context(), tenantID, id)
	if err != nil {
		h.releaseCollectHold(r.Context(), tenantID, id, hold)
		respond.FromError(w, r, err, "credit")
		return
	}
	if inv.AmountDueCents <= 0 {
		// Credits covered it — settle without a card charge, and without ever
		// asking whether there was a card. No charge was sent, so the hold is
		// released either way; left set, it would wait for the stale-hold
		// sweep (ADR-127).
		settled, serr := h.svc.SettleZeroDue(r.Context(), tenantID, id)
		h.releaseCollectHold(r.Context(), tenantID, id, hold)
		if serr != nil {
			respond.FromError(w, r, serr, "invoice")
			return
		}
		// A credit-paid invoice is recovered exactly as a card-paid one is, so
		// its dunning run closes the same way. Without this the run stayed
		// active at $0: a no-card run's next retry is days away and
		// the dunning tick only touches due runs. SettleZeroDue can return the
		// invoice unsettled (not finalized, or no longer pending); a run on an
		// unpaid invoice stays open.
		if settled.Status == domain.InvoicePaid {
			h.resolveDunningRecovered(r.Context(), tenantID, id)
		}
		// The same collect row the card path writes below: the credit paid
		// the invoice, and that is the money movement an auditor looks for.
		if settled.Status == domain.InvoicePaid && h.auditLogger != nil {
			_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), settled), tenantID, domain.AuditActionCollect, "invoice", settled.ID, settled.InvoiceNumber, map[string]any{
				"invoice_number":        settled.InvoiceNumber,
				"credits_applied_cents": settled.CreditsAppliedCents,
				"currency":              settled.Currency,
				"paid_with":             "credit",
			})
		}
		respond.JSON(w, r, http.StatusOK, settled)
		return
	}

	ps, err := h.paymentSetups.GetPaymentSetup(r.Context(), tenantID, inv.CustomerID)
	if err != nil || ps.SetupStatus != domain.PaymentSetupReady || ps.StripeCustomerID == "" {
		// Provably pre-Stripe — free the lease so sweep/dunning aren't
		// locked out for the window. Any credit already applied STAYS applied:
		// it was legitimately owed to this invoice, and amount_due is now
		// correct for whatever settles it later.
		h.releaseCollectHold(r.Context(), tenantID, id, hold)
		respond.Validation(w, r, "customer has no payment method set up")
		return
	}

	// Bound to the invoice's clock, as finalize binds its collect: on a
	// clock-pinned invoice the charge records its simulated instant (the PI's
	// hardcount_anchor_at), which dates paid_at and a decline's first dunning
	// retry. Unbound, a Collect on a later Advance dated the retry from the
	// issue instant, in the past. A wall-clock invoice binds nothing.
	charged, err := h.charger.ChargeInvoiceNow(h.svc.bindForInvoice(r.Context(), tenantID, id), tenantID, inv, hold, ps.StripeCustomerID, ps.StripePaymentMethodID)
	if err != nil {
		// ADR-026 boundary sanitization: ChargeInvoice wraps a
		// *payment.PaymentError which respond.FromError detects and
		// surfaces via OperatorSafeMessage() — humanized decline
		// reason or "Payment provider rejected the request" instead
		// of raw Stripe SDK strings (idempotency conflicts, etc.).
		respond.FromError(w, r, err, "payment")
		return
	}

	// Resolve any active dunning run — manual collect payment bypasses dunning retry
	h.resolveDunningRecovered(r.Context(), tenantID, id)

	// Explicit audit row for the money-movement action — "Collected payment on
	// INV-NNN", the row an auditor looks for when money left the customer's card
	// outside a billing run.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), charged), tenantID, domain.AuditActionCollect, "invoice", charged.ID, charged.InvoiceNumber, map[string]any{
			"invoice_number": charged.InvoiceNumber,
			"amount_cents":   inv.AmountDueCents,
			"currency":       inv.Currency,
		})
	}

	respond.JSON(w, r, http.StatusOK, charged)
}

// releaseCollectHold frees Collect's charge hold on a path that sent no
// charge. The store runs the release even if the operator's request has gone
// away; a failure is logged, and the hold then waits for the stale-hold
// sweep (ADR-127), which refuses the invoice an hour or more.
func (h *Handler) releaseCollectHold(ctx context.Context, tenantID, invoiceID string, hold domain.ChargeHold) {
	if err := h.svc.ReleaseChargeClaim(ctx, tenantID, invoiceID, hold); err != nil {
		slog.WarnContext(ctx, "collect payment: releasing the charge hold failed — the invoice waits for the stale-hold sweep",
			"invoice_id", invoiceID, "error", err)
	}
}

// resolveDunningRecovered closes the invoice's active dunning run, if any, as
// payment_recovered after collect settled it, whether by card or from credit.
// Best-effort: the payment has already committed, so a failure logs and the
// dunning tick's paid check resolves the run when it next comes due.
func (h *Handler) resolveDunningRecovered(ctx context.Context, tenantID, invoiceID string) {
	if h.dunning == nil {
		return
	}
	if err := h.dunning.ResolveByInvoice(ctx, tenantID, invoiceID, domain.ResolutionPaymentRecovered); err != nil {
		slog.WarnContext(ctx, "failed to resolve dunning after collect payment", "invoice_id", invoiceID, "error", err)
	}
}

// RefundRequest is the POST /v1/invoices/{id}/refund body. The handler turns
// it into a RefundInput for the RefundIssuer.
type RefundRequest struct {
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
	Description string `json:"description"`
}

// refund issues a direct refund on a paid invoice. Convenience wrapper around
// creditnote.Service.CreateRefund — the caller passes a reason + optional
// amount and gets back the issued credit note (which carries the Stripe
// refund ID and status). For partial refunds, amount_cents < amount_paid;
// default (amount_cents=0) refunds the full remaining refundable balance.
func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	if h.refundIssuer == nil {
		respond.Validation(w, r, "refund provider not configured")
		return
	}

	var body RefundRequest
	// Strict: amount_cents=0 means "refund everything still refundable", so a
	// dropped `amount` key would be a full card refund.
	if !respond.DecodeStrict(w, r, &body) {
		return
	}

	if body.AmountCents < 0 {
		respond.Validation(w, r, "amount_cents must be non-negative")
		return
	}
	if body.Reason == "" {
		respond.Validation(w, r, "reason is required")
		return
	}
	if !validRefundReasons[body.Reason] {
		respond.Validation(w, r, "reason must be one of: duplicate, fraudulent, requested_by_customer, other")
		return
	}

	cn, err := h.refundIssuer.IssueRefund(r.Context(), tenantID, RefundInput{
		InvoiceID:   id,
		AmountCents: body.AmountCents,
		Reason:      body.Reason,
		Description: body.Description,
	})
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	if h.auditLogger != nil {
		// Label the row with the invoice number so it reads "Refunded
		// INV-NNN" (a money-out action), matching finalize/void rows. The same
		// read supplies the sim axis (AuditCtx) — a refund issued inside a
		// simulation belongs to that clock's timeline.
		refundLabel := ""
		auditCtx := r.Context()
		if refInv, gErr := h.svc.Get(r.Context(), tenantID, id); gErr == nil {
			refundLabel = refInv.InvoiceNumber
			auditCtx = h.svc.AuditCtx(auditCtx, refInv)
		} else {
			// The refund ALREADY happened — money moved — so the row is written
			// either way; dropping it would be worse than an unlabelled one. But
			// without the invoice we cannot resolve its clock, so a refund issued
			// inside a simulation lands with NULL sim columns and silently
			// disappears from ?test_clock_id=. Say so, loudly: a row missing from
			// a filtered timeline is indistinguishable from an action that never
			// happened, and this is the only place that knows it went missing.
			slog.WarnContext(r.Context(), "refund audit row will be unstamped and unlabelled: invoice re-read failed",
				"invoice_id", id, "credit_note_id", cn.ID, "error", gErr)
		}
		_ = h.auditLogger.Log(auditCtx, tenantID, domain.AuditActionRefund, "invoice", id, refundLabel, map[string]any{
			"invoice_id":          id,
			"credit_note_id":      cn.ID,
			"credit_note_number":  cn.CreditNoteNumber,
			"refund_amount_cents": cn.RefundAmountCents,
			"stripe_refund_id":    cn.StripeRefundID,
			"refund_status":       string(cn.RefundStatus),
			"reason":              cn.Reason,
			"currency":            cn.Currency,
		})
	}

	respond.JSON(w, r, http.StatusOK, cn)
}

// retryTax re-runs tax calculation against a draft invoice in
// tax_status pending. Backs the "Retry tax" action surfaced
// by the unified Attention shape. Idempotent — each call increments
// tax_retry_count and rewrites the per-line + invoice-level tax fields.
//
// 200 with the updated invoice (carrying the new Attention) on
// success or post-retry failure (a "still failing" retry is not an
// HTTP error — the dashboard wants the new code to render). 409 when
// the gate fails (status != draft, or tax_status not retryable). 404
// when the invoice doesn't exist. When the tax resolves but the
// auto-finalize does not happen (the pause state could not be read, or
// finalize failed), the call fails: 500 for a database error, 409 for a
// state conflict. The invoice stays a draft with its tax saved, for the
// operator to finalize, and the retry_tax audit row is still written.
func (h *Handler) retryTax(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	// Snapshot the pre-retry attention reason for the audit trail so
	// post-mortems can answer "did the retry change anything?".
	before, _ := h.svc.Get(r.Context(), tenantID, id)

	inv, err := h.svc.RetryTax(r.Context(), tenantID, id)
	// RetryTax returns an invoice whenever its tax write committed, with or
	// without an error: the click changed the tax and total, so audit it.
	if inv.ID != "" && h.auditLogger != nil {
		var beforeReason, afterReason string
		if before.Attention != nil {
			beforeReason = string(before.Attention.Reason)
		}
		if inv.Attention != nil {
			afterReason = string(inv.Attention.Reason)
		}
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionRetryTax, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"invoice_number":   inv.InvoiceNumber,
			"customer_id":      inv.CustomerID,
			"tax_status":       inv.TaxStatus,
			"tax_retry_count":  inv.TaxRetryCount,
			"before_attention": beforeReason,
			"after_attention":  afterReason,
			"tax_error_code":   inv.TaxErrorCode,
		})
	}
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// Causal tie-ranks for same-instant timeline cascades (frozen-clock
// closes stamp finalize → dunning start → retries → escalation →
// write-off → resolve at ONE instant; the rank encodes their causal
// order so ties never render effect-before-cause). Gaps left for
// future event kinds.
const (
	rankInvoiceCreated   = 10
	rankInvoiceFinalized = 20
	// A charge attempt CAUSES the dunning campaign that follows it, so a
	// same-instant decline must sort above "Payment recovery started"
	// (ADR-103 — with one payment owner the two are separate rows, and
	// effect-before-cause would be visible).
	rankChargeAttempt     = 25
	rankDunningStarted    = 30 // dunning_started, retry_scheduled
	rankRetryAttempted    = 40
	rankEscalated         = 50
	rankLifecycleTerminal = 60 // paid / voided / uncollectible
	rankCreditNote        = 70
	// A dispute opens after the payment it disputes, and closes after it
	// opened.
	rankDisputeOpened   = 74
	rankDisputeClosed   = 76
	rankDunningResolved = 80
	rankEmail           = 90 // last: an email follows the event it announces (ADR-104)
)

func dunningEventRank(eventType domain.DunningEventType) int {
	switch eventType {
	case domain.DunningEventRetryAttempted:
		return rankRetryAttempted
	case domain.DunningEventEscalated:
		return rankEscalated
	case domain.DunningEventResolved:
		return rankDunningResolved
	default: // dunning_started, retry_scheduled, future kinds
		return rankDunningStarted
	}
}

// rfc3339OrEmpty serializes a wall stamp for the Recorded subline;
// zero (unknown) renders nothing.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// lifecycleAuditRows joins an invoice's audit entries to the lifecycle
// event types they transition (ADR-104 Invariant A, corrected boundary №2;
// ADR-128 rule 4). EXACT keys only: the top-level action for
// create/finalize/void, the frozen-vocabulary metadata discriminator for
// the two update-flavored transitions (ADR-090 froze the action set, so
// mark-uncollectible and record-payment ride action=update). Earliest
// row wins — each transition happens once, and Query returns
// newest-first, so the walk runs backwards. A transition with no audit
// row is simply absent: its timeline row names no actor and, on a test
// clock, renders without a Recorded stamp, honest for pre-audit history.
func lifecycleAuditRows(entries []domain.AuditEntry) map[string]domain.AuditEntry {
	out := map[string]domain.AuditEntry{}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		key := ""
		switch e.Action {
		case domain.AuditActionCreate:
			key = "invoice.created"
		case domain.AuditActionFinalize:
			key = "invoice.finalized"
		case domain.AuditActionVoid:
			key = "invoice.voided"
		case domain.AuditActionUpdate:
			switch e.Metadata["action"] {
			case "marked_uncollectible":
				key = "invoice.marked_uncollectible"
			case "payment_recorded":
				key = "invoice.paid"
			}
		}
		if _, seen := out[key]; key != "" && !seen {
			out[key] = e
		}
	}
	return out
}

// sortInvoiceTimeline orders the invoice's rows by timeline.Order, the one
// ordering rule for activity surfaces (ADR-128): the full-precision instant
// on the invoice's calendar, then the real sequence inside a same-instant
// group whose every row has a recorded stamp (ADR-104 ordering amendment,
// 2026-07-29), then the causal rank, then insertion order (callers append
// sources oldest-first).
func sortInvoiceTimeline(events []TimelineEvent) {
	timeline.Order(events,
		func(e TimelineEvent) time.Time { return e.sortAt },
		func(e TimelineEvent) time.Time { return e.recordedSort },
		func(a, b TimelineEvent) bool { return a.tieRank < b.tieRank })
}

// TimelineEvent is one event of GET /v1/invoices/{id}/payment-timeline.
type TimelineEvent struct {
	Timestamp string `json:"timestamp"`
	// sortAt/tieRank are ordering-only, never serialized. sortAt is the
	// FULL-PRECISION source instant (the serialized Timestamp truncates
	// to seconds — sorting it inverted same-second pairs, the 2026-07-19
	// subscription-timeline class). tieRank orders true same-instant
	// cascades causally: on a frozen clock an entire close cascade
	// (finalize → dunning start → retries → escalate → write-off →
	// resolve) legitimately shares one instant, and source-major
	// insertion rendered "Marked uncollectible" above the escalation
	// that caused it. See sortInvoiceTimeline.
	// ID is a stable per-row key for the SPA. Source rows use their own
	// row id (outbox id, attempt id, dunning event id, credit-note id);
	// synthesized lifecycle rows derive a deterministic one from the
	// invoice id + kind. Composite timestamp keys collided the moment
	// rows shared a frozen-clock instant (ADR-104).
	ID              string `json:"id"`
	Source          string `json:"source"` // "stripe" / "dunning" / "lifecycle" / "email" / "credit_note"
	EventType       string `json:"event_type"`
	Status          string `json:"status"`
	Description     string `json:"description"`
	Error           string `json:"error,omitempty"`
	AmountCents     *int64 `json:"amount_cents,omitempty"`
	Currency        string `json:"currency,omitempty"`
	PaymentIntentID string `json:"payment_intent_id,omitempty"`
	AttemptCount    int    `json:"attempt_count,omitempty"`
	sortAt          time.Time
	tieRank         int
	// recordedSort is the row's real-world instant as a sortable value
	// (zero when unknown). Within one story-instant group the timeline
	// orders by it — but ONLY when every row in the group has one (the
	// group-completeness gate): a partially-stamped group is legacy data,
	// and any placement of its bare rows against stamped ones fabricates
	// a sequence, so those groups keep the causal ladder. See
	// sortInvoiceTimeline.
	recordedSort time.Time
	// Detail is a sub-line rendered beneath the row's main
	// description. Used today on invoice.paid for the payment
	// instrument ("via Visa •••• 4242"); generic enough that
	// future event types can attach their own context (e.g.
	// "after 3 retry attempts" on the same row in the dunning-
	// recovered case). Empty = no sub-line. ADR-020.
	Detail string `json:"detail,omitempty"`
	// IsSimulated marks events whose PRIMARY timestamp is on the
	// entity's simulated calendar. Post-ADR-104 every row type on a
	// clock-pinned invoice is anchored there at write time (lifecycle,
	// dunning, attempts, credit notes, and emails via the outbox
	// anchor); false only on live-mode invoices and on legacy rows
	// written before their anchor existed.
	// SPA reads this flag directly — no client-side heuristic.
	IsSimulated bool `json:"is_simulated,omitempty"`
	// RecordedAt is the REAL-WORLD instant of a row whose primary
	// timestamp is simulated — the operator contract's Invariant A
	// ("any row whose two calendars differ shows both"). Rendered as a
	// muted "Recorded <wall>" subline, mirroring the subscription
	// timeline. Empty when the calendars coincide.
	RecordedAt string `json:"recorded_at,omitempty"`
	// Line is the row as the activity line standard words it (ADR-128).
	// Description, Detail, Error, Status, AmountCents and Currency above
	// are filled from it (fillFromLine), so a client that reads only the
	// older fields shows the same words.
	Line activity.Line `json:"line"`
	// dunningEventID is the payment-recovery event a row tells; empty on
	// every other row. RecoveryRunLines keys the Dunning page's lines by it.
	dunningEventID string
}

// fillFromLine stamps the line's id and times and copies its words into the
// older fields: the title is the description, the meta line (detail parts
// and actor) is the detail, the problem is the error, the tone is the
// status, and the line's amount is the row's amount.
func (e *TimelineEvent) fillFromLine() {
	e.Line.ID = e.ID
	e.Line = activity.Stamp(e.Line, e.sortAt, e.recordedSort, e.IsSimulated)
	e.Description = e.Line.Title
	e.Detail = activity.MetaText(e.Line)
	e.Error = e.Line.Problem
	e.Status = legacyStatus(e.Line.Tone)
	e.AmountCents, e.Currency = nil, ""
	if e.Line.Amount != nil {
		c := e.Line.Amount.Cents
		e.AmountCents, e.Currency = &c, e.Line.Amount.Currency
	}
}

func piFromEventMetadata(meta map[string]any) string {
	pi, _ := meta["payment_intent_id"].(string)
	return pi
}

// renderChargeAttempts renders the charge-attempt facts — the single
// source of payment rows on the timeline (ADR-103).
//
// Exactly three suppressions, all exact-keyed, no heuristics:
//
//   - a dunning row already carrying this attempt's PaymentIntent
//     absorbs it: the campaign row ("Payment retry 2 failed" with
//     its cause subline) is the richer telling of the same charge, and
//     it lifts the attempt's provider reason + amount onto itself;
//   - a SUCCEEDED attempt on a paid invoice defers to the "Invoice
//     paid" lifecycle row. That row is the superset — an invoice can
//     also become paid with no charge at all (credits, an offline
//     payment, a $0 total) — so it owns the success story;
//   - a dunning retry's below-minimum attempt defers to the retry's own
//     row ("Payment retry N refused by Stripe"). Stripe made no
//     PaymentIntent to key on, but a run ends at its one below-minimum
//     retry, so each run has at most one such row and one such attempt:
//     as many of these attempts are absorbed as there are rows, and one
//     renders for each row that is missing.
//
// `pending` attempts never render (in flight; the attention banner owns
// that), and a succeeded attempt on a NOT-paid invoice does render —
// that shape is an anomaly worth seeing.
func renderChargeAttempts(events []TimelineEvent, attempts []domain.InvoiceChargeAttempt, inv domain.Invoice) []TimelineEvent {
	dunningIdxByPI := recoveryRowsByPI(events)
	retryRefusedRows := 0
	for _, e := range events {
		if e.Line.Kind == kindRetryRefused {
			retryRefusedRows++
		}
	}
	sawSucceeded := false
	for _, a := range attempts {
		if a.Outcome == domain.ChargeAttemptSucceeded {
			sawSucceeded = true
		}
		switch a.Outcome {
		case domain.ChargeAttemptFailed, domain.ChargeAttemptUnknown:
		case domain.ChargeAttemptBelowMinimum:
			if a.Trigger == domain.ChargeTriggerDunningRetry && retryRefusedRows > 0 {
				retryRefusedRows--
				continue
			}
		case domain.ChargeAttemptSucceeded:
			if inv.PaidAt != nil {
				// The paid lifecycle row owns this success's story — and
				// this attempt owns its WALL moment (a webhook settle
				// writes no operator audit row, so the attempt's
				// occurred_at is the only real-world stamp the paid row
				// can honestly carry; ADR-104 Invariant A). Lift it when
				// the audit join left the row bare.
				if a.SimEffectiveAt != nil {
					for i := range events {
						if events[i].EventType == "invoice.paid" && events[i].Source == "lifecycle" && events[i].RecordedAt == "" {
							events[i].RecordedAt = a.OccurredAt.Format(time.RFC3339)
							events[i].recordedSort = a.OccurredAt
							break
						}
					}
				}
				continue
			}
		default: // pending
			continue
		}
		// The dunning row tells this charge better — hand it the
		// provider facts and drop the attempt row.
		if idx, ok := dunningIdxByPI[a.StripePaymentIntentID]; ok && a.StripePaymentIntentID != "" {
			liftAttempt(&events[idx], a, inv)
			continue
		}
		events = append(events, chargeAttemptRow(a, inv))
	}
	// Tripwire (ADR-103): a card-paid invoice with no succeeded attempt
	// means the one owner of payment rows is missing its fact — the
	// timeline will simply show no charge. Settlements with no charge
	// (offline payment, credit-covered, $0) have no PaymentIntent and
	// legitimately no attempt.
	if inv.PaidAt != nil && !sawSucceeded && inv.HasCardPayment() {
		slog.Warn("invoice is paid by card but has no succeeded charge attempt — payment row will be missing (ADR-103 single-source invariant)",
			"invoice_id", inv.ID, "payment_intent_id", inv.StripePaymentIntentID)
	}
	return events
}

// chargeAttemptRow renders one attempt fact. A simulated attempt's
// PRIMARY position is its billing-axis instant, with the wall instant
// kept as RecordedAt (Invariant A); an unanchored attempt keeps its
// wall stamp — same fallback as legacy emails (ADR-104).
func chargeAttemptRow(a domain.InvoiceChargeAttempt, inv domain.Invoice) TimelineEvent {
	ts := a.OccurredAt
	sim := false
	var recorded time.Time
	if a.SimEffectiveAt != nil {
		ts = *a.SimEffectiveAt
		recorded = a.OccurredAt
		sim = true
	}
	return TimelineEvent{
		ID:              "attempt:" + a.ID,
		Timestamp:       ts.Format(time.RFC3339),
		sortAt:          ts,
		tieRank:         rankChargeAttempt,
		Source:          "payment",
		EventType:       "charge_attempt." + string(a.Outcome),
		PaymentIntentID: a.StripePaymentIntentID,
		IsSimulated:     sim,
		RecordedAt:      rfc3339OrEmpty(recorded),
		recordedSort:    recorded,
		Line:            attemptLine(a, inv),
	}
}

// formatPaymentCardDetail produces the sub-line shown under the
// "Invoice paid" row, e.g. "via Visa •••• 4242". Returns empty
// when card details aren't on the invoice — graceful: no
// sub-line. Brand titlecased per Stripe convention
// (visa→Visa, mastercard→Mastercard). ADR-020.
func formatPaymentCardDetail(brand, last4 string) string {
	if last4 == "" && brand == "" {
		return ""
	}
	display := brandDisplayName(brand)
	if display == "" {
		display = "card"
	}
	if last4 == "" {
		return "via " + display
	}
	return "via " + display + " •••• " + last4
}

// brandDisplayName converts Stripe's enum-form card brand to the
// title-cased form operators read on the dashboard. Mirrors
// Stripe's own display names so the timeline matches what
// operators see in the Stripe dashboard.
//
// Stripe's PaymentMethodCard.DisplayBrand returns one of: visa,
// mastercard, american_express, cartes_bancaires, diners_club,
// discover, eftpos_australia, interac, jcb, union_pay, other —
// "and may contain more values in the future" per the SDK
// comment. Unknown values fall through to a defensive
// title-case so a future-Stripe addition doesn't render as
// "cartes_bancaires" in the dashboard.
func brandDisplayName(brand string) string {
	switch strings.ToLower(brand) {
	case "visa":
		return "Visa"
	case "mastercard":
		return "Mastercard"
	case "amex", "american_express", "american express":
		return "American Express"
	case "discover":
		return "Discover"
	case "jcb":
		return "JCB"
	case "diners", "diners_club":
		return "Diners Club"
	case "unionpay", "union_pay":
		return "UnionPay"
	case "cartes_bancaires":
		return "Cartes Bancaires"
	case "eftpos_australia":
		return "Eftpos Australia"
	case "interac":
		return "Interac"
	case "other":
		return "Card"
	case "":
		return ""
	default:
		// Unknown brand — title-case each underscore-separated
		// segment so a future-Stripe value renders legibly without
		// requiring a Hardcount release.
		return titleCaseSnake(brand)
	}
}

// titleCaseSnake turns "cartes_bancaires" into "Cartes Bancaires"
// for unrecognised brands. Defensive default so new Stripe
// networks render passably.
func titleCaseSnake(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, " ")
}

// emailRowInstant resolves an email row's position on the timeline
// (ADR-104), by the one rule every email surface uses
// (activity.EmailInstant): the PRIMARY instant is the billing instant that
// caused the send — the outbox anchor stamped at enqueue from bound ctx;
// rankEmail then places the row after the money event it announces, since a
// notification is only ever an effect. The real-world send instant
// (dispatched_at once the relay took it, enqueue time until then) survives
// as `recorded` — the operator contract's Invariant A: a row whose two
// calendars differ shows both. Legacy rows without an anchor keep their wall
// stamp — misplaced but honest; inferring an anchor at read time is the
// heuristic the no-heuristic-proxies rule bans.
func emailRowInstant(evt EmailEventRow) (ts time.Time, recorded time.Time, sim bool) {
	return activity.EmailInstant(evt.email())
}

// metaNumber reads a number an audit row's metadata carries (JSON decodes
// it as float64; a row built in Go may hold an int).
func metaNumber(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

// dunningRunEvents is one payment-recovery run and its events.
type dunningRunEvents struct {
	run    domain.InvoiceDunningRun
	events []domain.InvoiceDunningEvent
}

func (h *Handler) paymentTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(ctx, tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// Draft invoices have no payment activity
	if inv.Status == domain.InvoiceDraft {
		respond.JSON(w, r, http.StatusOK, map[string]any{"events": []TimelineEvent{}, "degraded": []string{}, "truncated": false})
		return
	}

	// is_simulated is the invoice's persisted, authoritative flag — stamped at
	// write time when the creating context was bound to a frozen test clock
	// (engine: the subscription is pinned; manual composer: the customer is
	// pinned). Reading it here, instead of re-deriving from the subscription's
	// CURRENT test_clock_id, fixes two defects: (1) manual one-off invoices
	// have no subscription to look through, so the old lookup always returned
	// false and dropped their badge despite simulated timestamps; (2) the badge
	// now survives a later clock unpin, since the old sub.Get was a mutable
	// read-time snapshot (the heuristic feedback_no_heuristic_proxies bans).
	// It also decides, for every row the invoice owns, whether the line
	// carries a Recorded time (ADR-128 rule 6).
	isSimulated := inv.IsSimulated

	// Lane-degradation disclosure (2026-07-19 audit, finding 4): a
	// side-lane fetch failure used to be swallowed (`if err == nil`) — the
	// lane silently vanished, and a timeline missing its dunning rows is
	// indistinguishable from an invoice that never saw dunning. The core
	// lifecycle rows come from the invoice row itself (a failure there
	// 500s above); side lanes degrade LOUDLY instead: named in `degraded`,
	// logged, and bannered by the UI. `truncated` discloses the one capped
	// lane (credit notes, CreditNoteLaneCap).
	degraded := []string{}
	truncated := false
	degradedLane := map[string]bool{}
	degrade := func(lane string, err error) {
		slog.ErrorContext(ctx, "payment timeline: lane degraded", "lane", lane, "invoice_id", inv.ID, "error", err)
		if !degradedLane[lane] {
			degradedLane[lane] = true
			degraded = append(degraded, lane)
		}
	}
	// auditRows reads one resource's audit trail; a failure degrades the
	// audit lane (once) and the rows it would have enriched name no actor.
	auditRows := func(resourceType, resourceID string) []domain.AuditEntry {
		if h.auditStamps == nil {
			return nil
		}
		entries, err := h.auditStamps.ListByResource(ctx, tenantID, resourceType, resourceID)
		if err != nil {
			degrade("audit", err)
			return nil
		}
		return entries
	}

	// ── Read every source first, in lane order; build rows after. ──

	// The invoice's audit rows: its lifecycle rows' actors (ADR-128 rule
	// 4), and on a test clock their Recorded stamps (ADR-104 Invariant A,
	// corrected boundary №2): lifecycle rows derive from invoice state
	// columns whose stamps follow the entity's calendar, so their real-world
	// moment and their actor live only in the audit log.
	lifecycle := lifecycleAuditRows(auditRows("invoice", inv.ID))

	// Credit notes. Issued notes only — drafts aren't activity yet, voided
	// notes vanish from the story the same way Stripe's do.
	var cns []domain.CreditNote
	if h.creditNotes != nil {
		if list, err := h.creditNotes.List(ctx, tenantID, inv.ID); err != nil {
			degrade("credit_notes", err)
		} else {
			// The lister fetches at most CreditNoteLaneCap notes; at the
			// cap we can't know what fell off, only that something may
			// have (the interface carries no total).
			if len(list) >= CreditNoteLaneCap {
				truncated = true
			}
			// Store returns created_at DESC — reverse so residual
			// exact-tie insertion order is causal (oldest first).
			slices.Reverse(list)
			cns = list
		}
	}

	// Customer emails. The payment-recovery emails (reminder, final notice)
	// are not rows of their own: each is a clause on the retry or
	// retries-ran-out row it belongs to, and its own entry lives on the
	// customer page's Sent emails card (Stripe shape —
	// docs.stripe.com/invoicing/send-email lists the email log on the
	// customer page). Reminders pair with their retry by the payload's
	// attempt_number = run.AttemptCount at send time (exact key, no index
	// heuristics); the payload carries no run id, so across multiple runs on
	// one invoice a repeated attempt number keeps the newest email —
	// accepted display nuance on an already-rare shape.
	var emails []EmailEventRow
	reminderByAttempt := map[int]EmailEventRow{}
	var escalationEmail *EmailEventRow
	if h.emailEvents != nil {
		if list, err := h.emailEvents.ListByInvoice(ctx, tenantID, inv.InvoiceNumber); err != nil {
			degrade("emails", err)
		} else {
			emails, reminderByAttempt, escalationEmail = recoveryEmails(list)
		}
	}

	// Payment-recovery runs and their events. ListRuns returns created_at
	// DESC — reverse so multi-run invoices append run 1 before run 2.
	var dunningRuns []dunningRunEvents
	if h.dunningTimeline != nil {
		if runs, err := h.dunningTimeline.ListRunsByInvoice(ctx, tenantID, id); err != nil {
			degrade("dunning", err)
		} else {
			slices.Reverse(runs)
			for _, run := range runs {
				evts, err := h.dunningTimeline.ListEvents(ctx, tenantID, run.ID)
				if err != nil {
					// One run's events failing still loses a slice of the
					// lane — disclose it, don't just skip the run.
					degrade("dunning", err)
					continue
				}
				dunningRuns = append(dunningRuns, dunningRunEvents{run: run, events: evts})
			}
		}
	}

	// Card disputes (#979).
	disputes, derr := h.svc.ListDisputes(ctx, tenantID, inv.ID)
	if derr != nil {
		degrade("disputes", derr)
	}

	// Charge attempts: the one owner of payment rows (ADR-103).
	attempts, aerr := h.svc.ListChargeAttempts(ctx, tenantID, inv.ID)
	if aerr != nil {
		degrade("charge_attempts", aerr)
	}

	// What the payment-recovery events tell the lifecycle rows: how many
	// retries ran (the paid row says "After N retries"), and whether
	// exhaustion wrote the invoice off (the uncollectible row says "Payment
	// retries ran out").
	maxAttemptCount := 0
	writtenOffByRecovery := false
	for _, re := range dunningRuns {
		for _, evt := range re.events {
			if evt.AttemptCount > maxAttemptCount {
				maxAttemptCount = evt.AttemptCount
			}
			if evt.EventType == domain.DunningEventEscalated && escalationTokens(evt.Reason)[tokenInvoiceWrittenOff] {
				writtenOffByRecovery = true
			}
		}
	}

	sub := h.subscriptionLink(ctx, tenantID, inv)

	// ── Rows ──

	var events []TimelineEvent

	// Lifecycle rows synthesised from invoice columns. Without these,
	// freshly-finalized invoices that haven't seen a charge yet render an
	// empty timeline. Mirrors Stripe's "Invoice activity" section, which
	// always anchors on Created → Finalized.
	lifecycleActor := func(key string) *activity.Actor {
		if e, ok := lifecycle[key]; ok {
			return actorFrom(&e)
		}
		return nil
	}
	addLifecycle := func(idKind, eventType string, at time.Time, rank int, line activity.Line) int {
		var recorded time.Time
		if e, ok := lifecycle[eventType]; ok && isSimulated {
			recorded = e.CreatedAt
		}
		events = append(events, TimelineEvent{
			ID:           "lifecycle:" + idKind + ":" + inv.ID,
			Timestamp:    at.Format(time.RFC3339),
			sortAt:       at,
			tieRank:      rank,
			Source:       "lifecycle",
			EventType:    eventType,
			IsSimulated:  isSimulated,
			RecordedAt:   rfc3339OrEmpty(recorded),
			recordedSort: recorded,
			Line:         line,
		})
		return len(events) - 1
	}
	addLifecycle("created", "invoice.created", inv.CreatedAt, rankInvoiceCreated,
		createdLine(inv, lifecycleActor("invoice.created"), sub))
	if inv.IssuedAt != nil {
		addLifecycle("finalized", "invoice.finalized", *inv.IssuedAt, rankInvoiceFinalized,
			finalizedLine(inv, lifecycleActor("invoice.finalized")))
	}
	// (Removed: synthetic "Payment deadline" event keyed off due_at.
	// Activity is for things that happened. The deadline is already
	// surfaced in the invoice header and the attention banner's `DueBy`
	// line.)
	if inv.VoidedAt != nil {
		addLifecycle("voided", "invoice.voided", *inv.VoidedAt, rankLifecycleTerminal,
			voidedLine(inv, lifecycleActor("invoice.voided")))
	}
	if inv.UncollectibleAt != nil {
		// The amount written off is what the write-off's own audit row
		// recorded. Without that row, the amount still due is it — unless
		// the invoice was paid since, when nothing on record says it.
		var writtenOff *int64
		if e, ok := lifecycle["invoice.marked_uncollectible"]; ok {
			if n, ok := metaNumber(e.Metadata, "amount_due_cents"); ok {
				writtenOff = &n
			}
		}
		if writtenOff == nil && inv.PaidAt == nil {
			n := inv.AmountDueCents
			writtenOff = &n
		}
		addLifecycle("uncollectible", "invoice.marked_uncollectible", *inv.UncollectibleAt, rankLifecycleTerminal,
			uncollectibleLine(inv, writtenOff, writtenOffByRecovery, lifecycleActor("invoice.marked_uncollectible")))
	}
	// Each issued credit note's audit trail, read once: the paid row and the
	// credit-note rows both use it.
	cnAudit := map[string][]domain.AuditEntry{}
	for _, cn := range cns {
		if cn.Status == domain.CreditNoteIssued && cn.IssuedAt != nil {
			cnAudit[cn.ID] = auditRows("credit_note", cn.ID)
		}
	}
	if inv.PaidAt != nil {
		// A credit note that left nothing due marked the invoice paid when
		// its issue row says so (ADR-061 amendment 2026-10-19): the paid row
		// names it, and whoever issued it, instead of the failed retries.
		var byNote *activity.Part
		var byNoteActor *activity.Actor
		for _, cn := range cns {
			for _, e := range cnAudit[cn.ID] {
				if e.Action == "credit_note.issued" && e.Metadata["marked_invoice_paid"] == true {
					link := activity.L(activity.LinkCreditNote, cn.ID, cn.CreditNoteNumber)
					byNote = &link
					ec := e
					byNoteActor = actorFrom(&ec)
				}
			}
		}
		// "After N retries" when payment recovery was running — NOT on a
		// bad-debt recovery. A written-off invoice's retries all FAILED —
		// that is why it was written off — so crediting a later payment to
		// them would show the count of failures as the cause of success.
		// The write-off stamp is the discriminator: it is set only on
		// invoices whose collection was abandoned, and it survives the
		// recovery.
		retries := 0
		if inv.UncollectibleAt == nil && byNote == nil {
			retries = maxAttemptCount
		}
		var note string
		if e, ok := lifecycle["invoice.paid"]; ok {
			note, _ = e.Metadata["note"].(string)
		}
		actor := lifecycleActor("invoice.paid")
		if actor == nil {
			actor = byNoteActor
		}
		i := addLifecycle("paid", "invoice.paid", *inv.PaidAt, rankLifecycleTerminal,
			paidLine(inv, retries, note, actor, byNote))
		events[i].AttemptCount = retries
	}

	// Credit-note rows. The settlement waterfall on the page already shows
	// credit notes channel-by-channel; these rows give the SAME facts a
	// place in the chronology. There is one timeline (ADR-104): every CN
	// row sorts by its issued_at on the invoice's calendar — operator CNs
	// and engine clawbacks both issue under the invoice's clock. A CN's
	// is_simulated is its source invoice's; it gates the Recorded part.
	cnLinks := map[string]activity.Part{}
	for _, cn := range cns {
		if cn.CreditNoteNumber != "" {
			cnLinks[cn.CreditNoteNumber] = activity.L(activity.LinkCreditNote, cn.ID, cn.CreditNoteNumber)
		}
		if cn.Status != domain.CreditNoteIssued || cn.IssuedAt == nil {
			continue
		}
		// The issue row is written in the Issue transaction (ADR-090), so
		// every issued note has one: it names who issued it. A failed card
		// refund's provider reason is on its refund_status_changed row.
		var issuedBy *activity.Actor
		var refundFailure string
		for _, e := range cnAudit[cn.ID] {
			switch {
			case e.Action == "credit_note.issued" && issuedBy == nil:
				ec := e
				issuedBy = actorFrom(&ec)
			case refundFailure == "" && e.Metadata["action"] == "refund_status_changed" && e.Metadata["refund_status"] == string(domain.RefundFailed):
				refundFailure, _ = e.Metadata["provider_failure_reason"].(string)
			}
		}
		var cnRecorded time.Time
		if cn.IsSimulated && cn.RecordedAt != nil {
			cnRecorded = *cn.RecordedAt
		}
		events = append(events, TimelineEvent{
			ID:           "cn:" + cn.ID,
			Timestamp:    cn.IssuedAt.Format(time.RFC3339),
			sortAt:       *cn.IssuedAt,
			tieRank:      rankCreditNote,
			Source:       "credit_note",
			EventType:    "credit_note.issued",
			IsSimulated:  cn.IsSimulated,
			RecordedAt:   rfc3339OrEmpty(cnRecorded),
			recordedSort: cnRecorded,
			Line:         creditNoteLine(cn, issuedBy, refundFailure),
		})
	}

	// Customer-notification email rows: the durable trace that the
	// customer was told.
	for _, evt := range emails {
		var cnLink *activity.Part
		if evt.EmailType == "credit_note" && evt.CreditNoteNumber != "" {
			p, ok := cnLinks[evt.CreditNoteNumber]
			if !ok {
				p = activity.T(evt.CreditNoteNumber)
			}
			cnLink = &p
		}
		line, ok := emailLine(evt, cnLink)
		if !ok {
			continue
		}
		ts, recorded, sim := emailRowInstant(evt)
		events = append(events, TimelineEvent{
			ID:           "email:" + evt.ID,
			Timestamp:    ts.Format(time.RFC3339),
			sortAt:       ts,
			tieRank:      rankEmail,
			Source:       "email",
			EventType:    "email." + evt.EmailType,
			IsSimulated:  sim,
			RecordedAt:   rfc3339OrEmpty(recorded),
			recordedSort: recorded,
			Line:         line,
		})
	}

	// Payment-recovery rows, with the invoice page's folds: a row the
	// invoice's own lifecycle row already tells is left out.
	events = append(events, recoveryRows(recoveryRowsInput{
		inv: inv, runs: dunningRuns, reminders: reminderByAttempt, escalation: escalationEmail, sub: sub,
		resolvedBy: func(runID string) *activity.Actor { return resolvedByIn(auditRows("dunning_run", runID)) },
		fold:       true,
	})...)

	// Card disputes: an opened row at Stripe's creation time, and a closed
	// row once Stripe closes it. Both are real-world Stripe instants, never
	// the invoice's simulated calendar (an open question the dates PR
	// settles, ADR-128).
	if derr == nil {
		events = append(events, disputeTimelineEvents(disputes)...)
	}

	// Payments render from ONE owner: the charge-attempt facts (ADR-103).
	// A dunning row that already carries the attempt's PaymentIntent
	// absorbs it (the campaign row is the richer telling of the same
	// charge) — an EXACT id match. Everything else renders as its own row.
	if aerr == nil {
		events = renderChargeAttempts(events, attempts, inv)
	}

	for i := range events {
		events[i].fillFromLine()
	}

	// Full-precision ascending with CAUSAL tie ranks — see
	// sortInvoiceTimeline.
	sortInvoiceTimeline(events)

	respond.JSON(w, r, http.StatusOK, map[string]any{"events": events, "degraded": degraded, "truncated": truncated})
}

// subscriptionLink is the link part to the invoice's subscription, or nil
// when it has none, no labeler is wired, or the subscription is gone. A
// failed lookup draws no link: the row still says everything else.
func (h *Handler) subscriptionLink(ctx context.Context, tenantID string, inv domain.Invoice) *activity.Part {
	if inv.SubscriptionID == "" || h.subscriptions == nil {
		return nil
	}
	label, err := h.subscriptions.SubscriptionLabel(ctx, tenantID, inv.SubscriptionID)
	if err != nil {
		if !errors.Is(err, errs.ErrNotFound) {
			slog.WarnContext(ctx, "payment timeline: subscription label lookup failed; rows carry no subscription link",
				"invoice_id", inv.ID, "subscription_id", inv.SubscriptionID, "error", err)
		}
		return nil
	}
	if label == "" {
		label = inv.SubscriptionID
	}
	p := activity.L(activity.LinkSubscription, inv.SubscriptionID, label)
	return &p
}

// disputeTimelineEvents renders each dispute as the rows it has earned so
// far: "Payment disputed" when Stripe opened it, and "Dispute closed" with
// its outcome once Stripe closed it.
func disputeTimelineEvents(disputes []domain.InvoiceDispute) []TimelineEvent {
	var out []TimelineEvent
	for _, d := range disputes {
		opened, closed := disputeLines(d)
		out = append(out, TimelineEvent{
			ID:              "dispute:opened:" + d.ID,
			Timestamp:       d.OpenedAt.Format(time.RFC3339),
			sortAt:          d.OpenedAt,
			tieRank:         rankDisputeOpened,
			Source:          "dispute",
			EventType:       domain.EventPaymentDisputeOpened,
			PaymentIntentID: d.StripePaymentIntentID,
			Line:            opened,
		})
		if closed == nil {
			continue
		}
		out = append(out, TimelineEvent{
			ID:              "dispute:closed:" + d.ID,
			Timestamp:       d.ClosedAt.Format(time.RFC3339),
			sortAt:          *d.ClosedAt,
			tieRank:         rankDisputeClosed,
			Source:          "dispute",
			EventType:       domain.EventPaymentDisputeClosed,
			PaymentIntentID: d.StripePaymentIntentID,
			Line:            *closed,
		})
	}
	return out
}

// listDisputes returns the card disputes Stripe reported against the
// invoice's payments (#979), oldest first. An invoice without any returns an
// empty list.
func (h *Handler) listDisputes(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	// One invoice's disputes are read whole and paged here.
	disputes, err := h.svc.ListInvoiceDisputes(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "list invoice disputes", "invoice_id", id, "error", err)
		respond.InternalError(w, r)
		return
	}
	respond.List(w, r, respond.Window(disputes, page), len(disputes))
}

func (h *Handler) downloadPDF(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// One shared context builder across emailed/downloaded/hosted PDFs.
	bt, ci, cnInfos := BuildPDFContext(r.Context(), h.customers, h.settings, h.pdfCreditNotes, tenantID, &inv)

	pdfBytes, err := RenderPDF(r.Context(), inv, items, bt, cnInfos, ci)
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename=\""+inv.InvoiceNumber+".pdf\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}
