package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// PaymentSetupStore persists Stripe customer/payment method data.
type PaymentSetupStore interface {
	UpsertPaymentSetup(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error)
	// UpsertPaymentSetupAudited runs the caller-supplied audit emission on
	// the same tx as the persisted mapping write (ADR-090) — used by the
	// checkout.session.completed flip so the webhook's only durable
	// mutation carries its evidence atomically.
	UpsertPaymentSetupAudited(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup, emit func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error)
	GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error)
}

// CheckoutHandler manages Stripe Checkout Sessions for payment method setup.
// Mode-aware: picks the live or test client based on the caller's API key.
//
// Return URLs are contextual per request, not global config: the caller
// passes `return_url` in the body and the handler appends
// `?payment=success` or `?payment=cancel`. Matches the rest of the
// Checkout flows in the codebase (PortalHandler, hosted-invoice Pay,
// public update-payment) and follows Stripe's own guidance — cancel =
// "back where you were," not a stateless global page that loses context.
type CheckoutHandler struct {
	clients *StripeClients
	store   PaymentSetupStore
	audit   AuditEmitter // optional; ADR-090 in-tx emission on the mapping write
}

func NewCheckoutHandler(clients *StripeClients, store PaymentSetupStore) *CheckoutHandler {
	if !clients.Has() {
		return nil
	}
	return &CheckoutHandler{clients: clients, store: store}
}

// SetAuditLogger wires ADR-090 in-tx audit emission for POST /v1/checkout/setup.
// The route has no service and owns no transaction: its one durable local
// mutation is the customer↔Stripe-Customer mapping write, so the emission rides
// THAT write's tx (shared fate). Optional — nil skips emission, which keeps the
// handler fake-friendly in tests; production wires it and audit.MustWired fails
// the boot if it is forgotten.
func (h *CheckoutHandler) SetAuditLogger(a AuditEmitter) { h.audit = a }

func (h *CheckoutHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/setup", h.createSetupSession)
	r.Get("/status/{customerID}", h.getPaymentStatus)
	return r
}

type setupRequest struct {
	CustomerID   string `json:"customer_id"`
	CustomerName string `json:"customer_name"`
	Email        string `json:"email"`
	// Address fields for Stripe compliance (required for India)
	AddressLine1   string `json:"address_line1,omitempty"`
	AddressCity    string `json:"address_city,omitempty"`
	AddressState   string `json:"address_state,omitempty"`
	AddressZip     string `json:"address_postal_code,omitempty"`
	AddressCountry string `json:"address_country,omitempty"`
	// ReturnURL is the page the operator came from. The handler appends
	// ?payment=success / ?payment=cancel as a UI hint. If unset, the
	// handler falls back to /customers/{id}?payment=…, which is always
	// a valid page (every setup flow has a customer_id).
	ReturnURL string `json:"return_url,omitempty"`
}

type setupResponse struct {
	SessionID        string `json:"session_id"`
	URL              string `json:"url"`
	StripeCustomerID string `json:"stripe_customer_id"`
}

func (h *CheckoutHandler) createSetupSession(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	sc := h.clients.ForCtx(r.Context())
	if sc == nil {
		respond.Error(w, r, http.StatusBadGateway, "api_error", "stripe_error",
			"stripe not configured for this mode")
		return
	}

	var req setupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	if req.CustomerID == "" {
		respond.Validation(w, r, "customer_id is required")
		return
	}

	// Check if customer already has a Stripe customer ID
	// A read error is not "no Stripe customer": creating one would re-map the
	// customer and strand their saved card on the old Stripe customer.
	var stripeCustomerID string
	existing, err := h.store.GetPaymentSetup(r.Context(), tenantID, req.CustomerID)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		respond.FromError(w, r, err, "payment_setup")
		return
	}
	if err == nil {
		stripeCustomerID = existing.StripeCustomerID
	}

	// Create or update Stripe customer
	if stripeCustomerID == "" {
		cusParams := &stripe.CustomerCreateParams{
			Name:  stripe.String(req.CustomerName),
			Email: stripe.String(req.Email),
			Params: stripe.Params{
				Metadata: map[string]string{
					"velox_customer_id": req.CustomerID,
					"velox_tenant_id":   tenantID,
				},
			},
		}
		// Address required for Indian Stripe accounts (export regulations)
		if req.AddressLine1 != "" || req.AddressCountry != "" {
			cusParams.Address = &stripe.AddressParams{
				Line1:      stripe.String(req.AddressLine1),
				City:       stripe.String(req.AddressCity),
				State:      stripe.String(req.AddressState),
				PostalCode: stripe.String(req.AddressZip),
				Country:    stripe.String(req.AddressCountry),
			}
		}
		cus, err := sc.V1Customers.Create(r.Context(), cusParams)
		if err != nil {
			respond.Error(w, r, http.StatusBadGateway, "api_error", "stripe_error",
				fmt.Sprintf("failed to create Stripe customer: %v", err))
			return
		}
		stripeCustomerID = cus.ID
	} else {
		// Always sync latest customer data to Stripe on re-setup
		updateParams := &stripe.CustomerUpdateParams{
			Name:  stripe.String(req.CustomerName),
			Email: stripe.String(req.Email),
		}
		if req.AddressLine1 != "" || req.AddressCountry != "" {
			updateParams.Address = &stripe.AddressParams{
				Line1:      stripe.String(req.AddressLine1),
				City:       stripe.String(req.AddressCity),
				State:      stripe.String(req.AddressState),
				PostalCode: stripe.String(req.AddressZip),
				Country:    stripe.String(req.AddressCountry),
			}
		}
		_, _ = sc.V1Customers.Update(r.Context(), stripeCustomerID, updateParams)
	}

	// Save the Stripe customer ID immediately (status: pending until checkout
	// completes) WITH its audit row on the same tx (ADR-090).
	//
	// The error used to be discarded (`_, _ =`). It is propagated now for two
	// reasons: (1) shared fate is meaningless if the caller ignores the result
	// — a failed emission must not leave the operator with a session whose
	// mapping silently rolled back; (2) a dropped mapping write was already a
	// real bug on its own — Velox forgets the Stripe Customer it just created,
	// and the next setup call mints a SECOND one for the same customer.
	if err := h.persistStripeMapping(r.Context(), tenantID, req, stripeCustomerID); err != nil {
		slog.ErrorContext(r.Context(), "checkout setup: persist stripe customer mapping",
			"customer_id", req.CustomerID, "stripe_customer_id", stripeCustomerID, "error", err)
		respond.FromError(w, r, err, "customer")
		return
	}

	// Build contextual return URLs. If the caller passed return_url
	// (the page they came from), use it; otherwise default to the
	// customer's detail page. Either way the customer_id is in the
	// URL so the SPA can refetch payment_setup and show the result.
	base := req.ReturnURL
	if base == "" {
		base = fmt.Sprintf("http://localhost:5173/customers/%s", req.CustomerID)
	}
	successURL := appendQuery(base, "payment", "success")
	cancelURL := appendQuery(base, "payment", "cancel")

	sess, err := sc.V1CheckoutSessions.Create(r.Context(), &stripe.CheckoutSessionCreateParams{
		Customer:           stripe.String(stripeCustomerID),
		Mode:               stripe.String(string(stripe.CheckoutSessionModeSetup)),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		// Stamp velox_customer_id on the underlying SetupIntent, not just the
		// session: Stripe Checkout does NOT copy session metadata onto the
		// SetupIntent, so without this the setup_intent.succeeded webhook
		// arrives with empty metadata and must resolve the customer by Stripe
		// id — which races the customer↔Stripe-id link-write and can drop the
		// saved card. setup_intent_data.metadata makes the event self-resolving.
		SetupIntentData: &stripe.CheckoutSessionCreateSetupIntentDataParams{
			Metadata: map[string]string{
				"velox_customer_id": req.CustomerID,
				"velox_tenant_id":   tenantID,
			},
		},
		Params: stripe.Params{
			Metadata: map[string]string{
				"velox_customer_id": req.CustomerID,
				"velox_tenant_id":   tenantID,
			},
		},
	})
	if err != nil {
		// Sanitize (ADR-026): the Stripe SDK error includes raw
		// API request/response bodies. Log full server-side; surface
		// generic operator-safe message.
		slog.ErrorContext(r.Context(), "stripe checkout session create failed",
			"customer_id", req.CustomerID, "error", err)
		respond.Error(w, r, http.StatusBadGateway, "api_error", "stripe_error",
			"Could not start the payment setup flow. Please retry; if the problem persists, contact support.")
		return
	}

	respond.JSON(w, r, http.StatusCreated, setupResponse{
		SessionID:        sess.ID,
		URL:              sess.URL,
		StripeCustomerID: stripeCustomerID,
	})
}

// persistStripeMapping writes the customer↔Stripe-Customer mapping — the only
// durable LOCAL mutation POST /v1/checkout/setup makes — and rides the ADR-090
// audit emission on that write's transaction.
//
// The composite store's Audited hook (api/adapters.go →
// customer.SetStripeCustomerIDAudited) emits ONLY when the UPDATE actually
// touched a row: a setup started against a customer id that doesn't exist on
// this tenant/livemode plane writes nothing and therefore fabricates no
// "checkout_setup_started" evidence. A re-setup for a customer that already has
// the mapping DOES emit — the operator really did start a new setup flow, which
// is the fact this row records.
func (h *CheckoutHandler) persistStripeMapping(ctx context.Context, tenantID string, req setupRequest, stripeCustomerID string) error {
	var emit func(tx *sql.Tx) error
	if h.audit != nil {
		emit = func(tx *sql.Tx) error {
			return h.audit.LogInTx(ctx, tx, audit.Entry{
				Action:       domain.AuditActionUpdate,
				ResourceType: "customer",
				ResourceID:   req.CustomerID,
				// Deliberately NO label: req.CustomerName is the REQUEST's
				// claim, not the system's record of who this customer is, and
				// audit_log is append-only — a caller sending a name that
				// doesn't match the stored customer would permanently label
				// the row with a value nothing verified. The dashboard falls
				// back to the resource type and deep-links resource_id, so
				// the operator still resolves the real customer.
				ResourceLabel: "",
				Metadata: map[string]any{
					"action":             "checkout_setup_started",
					"stripe_customer_id": stripeCustomerID,
				},
			})
		}
	}
	_, err := h.store.UpsertPaymentSetupAudited(ctx, tenantID, domain.CustomerPaymentSetup{
		CustomerID:       req.CustomerID,
		TenantID:         tenantID,
		SetupStatus:      domain.PaymentSetupPending,
		StripeCustomerID: stripeCustomerID,
		UpdatedAt:        time.Now().UTC(),
	}, emit)
	return err
}

func (h *CheckoutHandler) getPaymentStatus(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customerID")

	ps, err := h.store.GetPaymentSetup(r.Context(), tenantID, customerID)
	if err != nil {
		respond.JSON(w, r, http.StatusOK, domain.CustomerPaymentSetup{
			CustomerID:  customerID,
			SetupStatus: domain.PaymentSetupMissing,
		})
		return
	}

	respond.JSON(w, r, http.StatusOK, ps)
}

// appendQuery adds key=value to base, preserving any existing query
// string. Used to stamp `?payment=success` / `?payment=cancel` onto
// the operator's source page so the SPA knows how to render the result.
func appendQuery(base, key, value string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + key + "=" + value
}
