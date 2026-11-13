package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/activity"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/user"
)

// compositePaymentSetupStore implements payment.PaymentSetupStore on the
// canonical sources: customers.stripe_customer_id and payment_methods. It
// lets payment/stripe.go keep its PaymentSetupStore wiring while the
// deprecated customer_payment_setups table is read-only on its way out. A
// read builds the CustomerPaymentSetup shape from the canonical state. A
// write keeps only stripe_customer_id, the one part the new design owns, and
// drops the rest. Card details reach paymentmethods.Service through the
// webhook attach path.
type compositePaymentSetupStore struct {
	customers *customer.PostgresStore
	pms       *paymentmethods.Service
}

func (a *compositePaymentSetupStore) GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error) {
	cust, err := a.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	pms, err := a.pms.List(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	ps := domain.CustomerPaymentSetup{
		CustomerID:       customerID,
		TenantID:         tenantID,
		StripeCustomerID: cust.StripeCustomerID,
		UpdatedAt:        cust.UpdatedAt,
		CreatedAt:        cust.CreatedAt,
	}
	// Find the default active payment method. List returns only active rows
	// (detached_at IS NULL), with the is_default row first.
	for _, pm := range pms {
		if pm.IsDefault {
			ps.SetupStatus = domain.PaymentSetupReady
			ps.DefaultPaymentMethodPresent = true
			ps.PaymentMethodType = pm.Type
			ps.StripePaymentMethodID = pm.StripePaymentMethodID
			ps.CardBrand = pm.CardBrand
			ps.CardLast4 = pm.CardLast4
			ps.CardExpMonth = pm.CardExpMonth
			ps.CardExpYear = pm.CardExpYear
			return ps, nil
		}
	}
	// A Stripe customer with no default payment method yet is pending. No
	// Stripe customer at all is missing.
	if cust.StripeCustomerID != "" {
		ps.SetupStatus = domain.PaymentSetupPending
	} else {
		ps.SetupStatus = domain.PaymentSetupMissing
	}
	return ps, nil
}

// PaymentReadyCustomers answers GetPaymentSetup's readiness question for many
// customers in two reads, instead of two per customer (#1142). Ready means an
// active default payment method (GetPaymentSetup's PaymentSetupReady) and a
// Stripe customer to charge. The invoice attention classifier asks for this
// pair.
func (a *compositePaymentSetupStore) PaymentReadyCustomers(ctx context.Context, tenantID string, customerIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(customerIDs))
	if len(customerIDs) == 0 {
		return out, nil
	}
	custs, err := a.customers.GetMany(ctx, tenantID, customerIDs)
	if err != nil {
		return nil, err
	}
	withDefault, err := a.pms.CustomersWithDefault(ctx, tenantID, customerIDs)
	if err != nil {
		return nil, err
	}
	for _, c := range custs {
		if withDefault[c.ID] && c.StripeCustomerID != "" {
			out[c.ID] = true
		}
	}
	return out, nil
}

func (a *compositePaymentSetupStore) UpsertPaymentSetup(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return a.UpsertPaymentSetupAudited(ctx, tenantID, ps, nil)
}

// UpsertPaymentSetupAudited runs the audit emission (ADR-090) on the
// transaction of the mapping write, the only lasting change on this path.
func (a *compositePaymentSetupStore) UpsertPaymentSetupAudited(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup, emit func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	// Only the stripe_customer_id mapping is written on this path. Card
	// details, the default flag and setup_status live on payment_methods,
	// which paymentmethods.Service writes from the webhook attach path. The
	// other fields on ps are ignored. Each one either repeats
	// payment_methods, or is left over from the state machine of the
	// dropped summary table.
	if ps.StripeCustomerID != "" && ps.CustomerID != "" {
		if err := a.customers.SetStripeCustomerIDAudited(ctx, tenantID, ps.CustomerID, ps.StripeCustomerID, emit); err != nil {
			return domain.CustomerPaymentSetup{}, err
		}
	}
	// When no mapping write happened (an id is missing), the emission is
	// dropped on purpose: there is no lasting change to attach evidence to.
	// Return the composed view after the write, so callers see their write.
	return a.GetPaymentSetup(ctx, tenantID, ps.CustomerID)
}

// stripeCustomerResolverAdapter implements payment.CustomerByStripeIDResolver
// over the customer store. It lets the setup_intent.succeeded webhook map a
// SetupIntent's `customer` (a Stripe customer ID) back to the Hardcount
// customer, without payment/ importing customer/. Like the other adapters
// here, it lives in the composition root: the package that wires the domains
// together.
type stripeCustomerResolverAdapter struct {
	customers *customer.PostgresStore
}

func (a *stripeCustomerResolverAdapter) CustomerIDByStripeID(ctx context.Context, tenantID, stripeCustomerID string) (string, error) {
	c, err := a.customers.GetByStripeCustomerID(ctx, tenantID, stripeCustomerID)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// paymentReadinessAdapter implements billing.PaymentReadiness. It combines
// customer.PostgresStore, for the Stripe customer ID, with
// paymentmethods.Service, for the canonical "is there a default active
// payment method?" query. Each fact has one source:
//   - customers.stripe_customer_id maps the customer to its Stripe customer
//     (one to one, created when first needed).
//   - payment_methods is the canonical store of a customer's payment
//     methods. The default row (is_default=true, detached_at IS NULL) is
//     the one to charge. List filters out detached rows in its query.
type paymentReadinessAdapter struct {
	customers *customer.PostgresStore
	pms       *paymentmethods.Service
}

func (a *paymentReadinessAdapter) ResolveForCharge(ctx context.Context, tenantID, customerID string) (string, string, error) {
	cust, err := a.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", err
	}
	if cust.StripeCustomerID == "" {
		// No Stripe customer yet, so nothing to charge automatically.
		return "", "", nil
	}
	pms, err := a.pms.List(ctx, tenantID, customerID)
	if err != nil {
		return "", "", err
	}
	for _, pm := range pms {
		if pm.IsDefault {
			// Return the default card's Stripe PaymentMethod id, so the
			// engine charges this card and not Stripe's own default.
			// List filters out detached rows, so this one is never
			// detached and can always be charged.
			return cust.StripeCustomerID, pm.StripePaymentMethodID, nil
		}
	}
	// A Stripe customer with no default payment method, for example one
	// created in Stripe whose card was removed. The engine reads the empty id
	// as "no payment method ready", and queues a retry for when one is
	// attached.
	return cust.StripeCustomerID, "", nil
}

// invoiceWriterAdapter adapts invoice.PostgresStore to billing.InvoiceWriter.
// The embedded store supplies every method whose name already matches the
// engine's. The four below exist only because the engine names them
// differently.
type invoiceWriterAdapter struct {
	*invoice.PostgresStore
}

func (a *invoiceWriterAdapter) GetInvoice(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	return a.Get(ctx, tenantID, id)
}

func (a *invoiceWriterAdapter) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

// creditGrantAdapter implements creditnote.CreditGranter over credit.Service.
type creditGrantAdapter struct {
	svc *credit.Service
}

// GrantForCreditNoteTx calls credit.Service.GrantForCreditNoteTx on the
// coordinator transaction of the credit note's Issue() (ADR-061). On that
// transaction, the credit ledger entry commits atomically with the
// draft-to-issued compare-and-set (CAS). The credit goes back to the blocks
// the invoice drew it from (ADR-132).
func (a *creditGrantAdapter) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, creditNoteID string, input creditnote.CreditGrantInput) error {
	_, err := a.svc.GrantForCreditNoteTx(ctx, tx, tenantID, creditNoteID, credit.CreditNoteReturnInput{
		CustomerID:        input.CustomerID,
		Currency:          input.Currency,
		AmountCents:       input.AmountCents,
		Description:       input.Description,
		InvoiceID:         input.InvoiceID,
		InvoicePaid:       input.InvoicePaid,
		InvoiceTotalCents: input.InvoiceTotalCents,
	})
	// The split is not returned: whatever it is, the balance rises by the
	// note's credit amount, which is what the note's documents state.
	return err
}

// LockCommitGrantForReliefTx and RetireCommitSliceForReliefTx (below) pass
// the relief pair to credit.Service, on the credit note's coordinator
// transaction. A relief credit note refunds part or all of a paid prepaid
// commit by retiring credits the customer has not used (ADR-080).
func (a *creditGrantAdapter) LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, int64, int64, bool, error) {
	return a.svc.LockCommitGrantForReliefTx(ctx, tx, tenantID, invoiceID)
}

func (a *creditGrantAdapter) RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, creditNoteID string, slice, refundedGrossCents, grossPaidCents int64) (int64, error) {
	res, err := a.svc.RetireCommitSliceForReliefTx(ctx, tx, tenantID, invoiceID, creditNoteID, slice, refundedGrossCents, grossPaidCents)
	if err != nil {
		return 0, err
	}
	return res.RemainingAfterCents, nil
}

// creditNoteListerAdapter implements invoice.CreditNoteLister over
// creditnote.Service.
type creditNoteListerAdapter struct {
	svc *creditnote.Service
}

func (a *creditNoteListerAdapter) List(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error) {
	return a.svc.List(ctx, creditnote.ListFilter{
		TenantID:  tenantID,
		InvoiceID: invoiceID,
		// Request the store's maximum. An unset Limit falls back to the
		// store default (50) without saying so. The timeline handler
		// reports `truncated` when the response comes back at the cap.
		Limit: invoice.CreditNoteLaneCap,
	})
}

// ListIssued is every issued credit note on the invoice, unpaged, for the
// invoice PDF (dashboard download, email and hosted page).
func (a *creditNoteListerAdapter) ListIssued(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error) {
	return a.svc.ListIssuedForInvoice(ctx, tenantID, invoiceID)
}

// refundIssuerAdapter adapts creditnote.Service to invoice.RefundIssuer. It
// translates the handler's invoice.RefundInput to the creditnote form. The
// two types are nearly the same on purpose, so the handler does not have to
// import creditnote just to issue a refund.
type refundIssuerAdapter struct {
	svc *creditnote.Service
}

func (a *refundIssuerAdapter) IssueRefund(ctx context.Context, tenantID string, input invoice.RefundInput) (domain.CreditNote, error) {
	return a.svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{
		InvoiceID:   input.InvoiceID,
		AmountCents: input.AmountCents,
		Reason:      input.Reason,
		Description: input.Description,
	})
}

// retryInvoiceStore is the part of the invoice store a dunning retry calls.
// *invoice.PostgresStore implements it; a test wraps the real store to fail
// one call.
type retryInvoiceStore interface {
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	ClaimChargeForDunningRetry(ctx context.Context, tenantID, id string) (domain.ChargeHold, error)
	ReleaseChargeHold(ctx context.Context, tenantID, id string, hold domain.ChargeHold) error
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
	SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error)
	RetryBlockingDecline(ctx context.Context, tenantID, invoiceID, stripePaymentMethodID string) (string, error)
	ListChargeAttemptsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceChargeAttempt, error)
}

// stripeReadiness reports whether a tenant has Stripe connected for a mode.
// *payment.StripeClients implements it (HasFor); a read error is an error,
// never "not connected".
type stripeReadiness interface {
	HasFor(ctx context.Context, tenantID string, livemode bool) (bool, error)
}

// paymentRetrierAdapter adapts Stripe and the invoice and customer stores to
// dunning.PaymentRetrier.
type paymentRetrierAdapter struct {
	charger       *payment.Stripe
	invoiceStore  retryInvoiceStore
	paymentSetups payment.PaymentSetupStore
	credits       *credit.Service
	// stripeReady says whether Stripe is connected for the invoice's mode.
	// Without Stripe, a retry charges nothing and is only a reminder
	// (ADR-060, amendment of 2026-10-30).
	stripeReady stripeReadiness
}

// RetryPayment counts a retry only when it got a definite answer; see
// classifyDunningRetryResult.
func (a *paymentRetrierAdapter) RetryPayment(ctx context.Context, tenantID, invoiceID, customerID string) error {
	return classifyDunningRetryResult(ctx, invoiceID, a.retryPayment(ctx, tenantID, invoiceID, customerID))
}

// classifyDunningRetryResult is the one rule for which retry results use up
// a dunning attempt. These results count:
//   - a decline Stripe returned (*payment.PaymentError with Unknown=false);
//   - no card to charge, counted so a card-less invoice reaches the final
//     action of its dunning run (ADR-060);
//   - a card the bank refused for good, or whose payment it wants the
//     customer to confirm;
//   - an amount below Stripe's minimum charge (domain.ErrBelowMinimumCharge),
//     which ends the run instead of counting toward its final action.
//
// Everything else becomes dunning.ErrTransientSkip, which dunning does not
// count: a database error, Stripe unreachable or rate limiting, an outcome
// Stripe did not confirm, or a settle that failed after Stripe took the
// money. Counting those would spend an attempt on a dropped connection, put
// the error text in the customer's warning email, and on the last attempt
// run the final action with no final charge.
//
// The rule sits here, at the boundary, so no call site decides it alone.
// The same class of bug was fixed one call site at a time, three times
// (#940, #978, #1302).
func classifyDunningRetryResult(ctx context.Context, invoiceID string, err error) error {
	// ErrPaymentInFlight passes: Stripe was asked and a PaymentIntent exists,
	// so the attempt counts and the run waits for its outcome (#1289).
	if err == nil || errors.Is(err, dunning.ErrTransientSkip) || errors.Is(err, dunning.ErrPaymentInFlight) ||
		errors.Is(err, domain.ErrNoPaymentMethodOnRetry) || errors.Is(err, domain.ErrCardNeedsReplacing) ||
		errors.Is(err, domain.ErrCardNeedsConfirmation) ||
		errors.Is(err, domain.ErrBelowMinimumCharge) {
		return err
	}
	var pe *payment.PaymentError
	if errors.As(err, &pe) && !pe.Unknown {
		return err
	}
	slog.WarnContext(ctx, "dunning retry: no definite answer; skipping this attempt uncounted",
		"invoice_id", invoiceID, "error", err)
	return dunning.ErrTransientSkip
}

func (a *paymentRetrierAdapter) retryPayment(ctx context.Context, tenantID, invoiceID, customerID string) error {
	inv, err := a.invoiceStore.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return fmt.Errorf("get invoice: %w", err)
	}
	if inv.AmountDueCents <= 0 {
		// Nothing to charge. The run closes as recovered only if the
		// invoice is paid, so settle it first. A credit balance applied by
		// an earlier pass whose mark-paid failed leaves the invoice open at
		// $0. Returning nil without settling would close the run with the
		// invoice still unpaid.
		inv, _, err = a.invoiceStore.SettleZeroDue(ctx, tenantID, invoiceID, clock.Now(ctx))
		if err != nil {
			return fmt.Errorf("settle invoice with nothing due: %w", err)
		}
		if inv.Status != domain.InvoicePaid {
			// A charge hold is set (a charge is running, or one's result
			// was lost), or the invoice is no longer open. The next pass
			// looks again. The paid pre-check closes a voided or
			// written-off invoice.
			slog.InfoContext(ctx, "dunning retry: invoice has nothing due but was not marked paid; skipping this attempt",
				"invoice_id", invoiceID, "status", inv.Status, "payment_status", inv.PaymentStatus)
			return dunning.ErrTransientSkip
		}
		return nil
	}

	// Take the per-invoice charge lease (the hold), which the auto-charge
	// sweep shares. The two paths' Stripe idempotency keys always differ,
	// because each has its own purpose suffix. So this lease is the only
	// guard against a double charge between them (hazard 11 in
	// docs/dev/records/ha-readiness-2026-07-06.md).
	//
	// A lost claim means the sweep or a rival dunning leader owns the
	// invoice, or payment_status moved to a state that must not be charged
	// again. ('unknown' waits for the reconciler or the stale-hold sweep.)
	// Return ErrTransientSkip, so the run does not use up an attempt on a
	// charge that never reached Stripe.
	//
	// A lost charge's hold refuses the claim too (ADR-127). The due-run scan
	// already skips such a run, so this case is the race where the hold
	// appeared after the listing.
	hold, err := a.invoiceStore.ClaimChargeForDunningRetry(ctx, tenantID, invoiceID)
	if err != nil {
		return fmt.Errorf("claim dunning retry charge: %w", err)
	}
	if !hold.Held() {
		return dunning.ErrTransientSkip
	}
	// Every path below that sends no charge releases the hold by its token.
	// The store runs the release even after ctx ends.
	release := func() {
		if err := a.invoiceStore.ReleaseChargeHold(ctx, tenantID, invoiceID, hold); err != nil {
			slog.WarnContext(ctx, "dunning retry: releasing the charge hold failed — the invoice waits for the stale-hold sweep",
				"invoice_id", invoiceID, "error", err)
		}
	}

	// Apply customer credits again before retrying the card, as the
	// auto-charge sweep does (processAutoCharge). Credits granted since the
	// first failure, or whose apply failed at cycle close, must reduce the
	// charge. They must not sit unused while the card is charged the full
	// amount. ApplyToInvoiceAt is idempotent: it drains min(due, balance). An
	// apply failure never reaches Stripe, so RetryPayment logs it and skips
	// the attempt uncounted.
	if a.credits != nil {
		at := clock.Now(ctx) // sim-aware: catchup binds effective-now on ctx
		if _, err := a.credits.ApplyToInvoiceAt(ctx, tenantID, customerID, invoiceID, inv.AmountDueCents, at, inv.InvoiceNumber); err != nil {
			// This failed before any Stripe call. Release the hold, so the
			// next due tick retries without waiting for the stale-hold
			// sweep.
			release()
			return fmt.Errorf("apply credits: %w", err)
		}
		inv, err = a.invoiceStore.Get(ctx, tenantID, invoiceID)
		if err != nil {
			release() // provably pre-Stripe
			return fmt.Errorf("refresh invoice after credit apply: %w", err)
		}
		if inv.AmountDueCents <= 0 {
			// Credits cover the invoice, so settle it without a card
			// charge. A nil return means recovered, so the dunning run
			// resolves. MarkPaid fires the transactional invoice.paid
			// event. No charge is sent, so the hold is released either way.
			_, err := a.invoiceStore.MarkPaid(ctx, tenantID, invoiceID, "", at)
			release()
			if err != nil {
				return fmt.Errorf("mark credit-covered invoice paid: %w", err)
			}
			return nil
		}
	}

	// Without Stripe for the invoice's mode, no charge is attempted, whatever
	// card is on file, and none can succeed until Stripe is connected. The
	// attempt counts (ErrNoOnlinePaymentOnRetry is a kind of
	// ErrNoPaymentMethodOnRetry), so the run still reaches its final action.
	// Its reminder asks the customer to pay the invoice (ADR-060, amendment
	// of 2026-10-30). A failed read does not mean "not connected": skip this
	// tick, uncounted, as for the payment-setup read below. Either way this
	// is before any Stripe call, so the hold is released.
	connected, err := a.stripeConnected(ctx, tenantID)
	if err != nil {
		release()
		slog.WarnContext(ctx, "dunning retry: Stripe connection read failed; skipping this attempt",
			"invoice_id", invoiceID, "error", err)
		return dunning.ErrTransientSkip
	}
	if !connected {
		release()
		return domain.ErrNoOnlinePaymentOnRetry
	}

	ps, err := a.paymentSetups.GetPaymentSetup(ctx, tenantID, customerID)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		// A read error does not mean "no card". Reporting
		// ErrNoPaymentMethodOnRetry would spend a counted dunning attempt on
		// a database blip, and could drive the invoice to its final action.
		// Skip this tick, uncounted.
		release()
		slog.WarnContext(ctx, "dunning retry: payment setup read failed; skipping this attempt",
			"invoice_id", invoiceID, "customer_id", customerID, "error", err)
		return dunning.ErrTransientSkip
	}
	if err != nil || ps.StripeCustomerID == "" || ps.StripePaymentMethodID == "" {
		// This is before any Stripe call, so release the lease before
		// reporting. The typed sentinel lets the dunning warning email show
		// plain text for the customer instead of an internal error string.
		release()
		return domain.ErrNoPaymentMethodOnRetry
	}

	// Two answers from the bank stop a retry from charging this card for
	// this invoice. One is a card refused for good (lost, stolen and the
	// rest of domain.IsHardDecline, #966): the retry cannot succeed, and the
	// card networks charge merchants for trying. The other is
	// authentication_required: the bank wants the customer to confirm the
	// payment, which a charge made without them cannot do. The customer pays
	// on the hosted invoice page, where Stripe runs the confirmation.
	//
	// Stripe Billing lists both among the codes it does not retry. Its
	// schedule and attempt count carry on, but a retry charges only once it
	// sees a new payment method. Hardcount does the same: this attempt
	// counts and charges nothing until a different card is the customer's
	// default, and the next scheduled retry charges that card. This is
	// before any Stripe call, so the lease is released. A read error does
	// not mean "no refusal": skip this tick uncounted, as for the
	// payment-setup read.
	blockCode, err := a.invoiceStore.RetryBlockingDecline(ctx, tenantID, invoiceID, ps.StripePaymentMethodID)
	if err != nil {
		release()
		slog.WarnContext(ctx, "dunning retry: refused-card read failed; skipping this attempt",
			"invoice_id", invoiceID, "customer_id", customerID, "error", err)
		return dunning.ErrTransientSkip
	}
	if block := domain.RetryBlock(blockCode); block != nil {
		release()
		slog.InfoContext(ctx, "dunning retry: not charging this card; the bank refused it for good or wants the customer to confirm the payment; waiting for the customer or a different default card",
			"invoice_id", invoiceID, "customer_id", customerID, "decline_code", blockCode)
		return block
	}

	// Give the Stripe call at most 15s. A scheduler tick runs tens of
	// retries one after another. Without this limit, one tenant's Stripe
	// call, stuck behind a network partition, could hold the goroutine for
	// the full request deadline (minutes). That would starve every other
	// tenant's retry in this tick.
	chargeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// The dedicated retry-charge method tags the PaymentIntent with
	// hardcount_purpose=dunning_retry. A report of its decline, from this
	// call's own response or from the payment_intent.payment_failed
	// webhook, then skips the generic payment-failed email. Dunning sends
	// its own warning or escalation email inline. Without the tag, the
	// customer gets two emails per failed retry: "Payment failed for
	// invoice X" and dunning's "Action required — payment retry for invoice
	// X (Attempt N of M)".
	charged, err := a.charger.ChargeInvoiceForDunningRetry(chargeCtx, tenantID, inv, hold, ps.StripeCustomerID, ps.StripePaymentMethodID)
	if re := definiteRetryFailure(err); re != nil {
		return re
	}
	if err != nil {
		return err
	}
	return a.chargedOutcome(ctx, tenantID, charged)
}

// definiteRetryFailure wraps a charge that Stripe definitely refused, for
// dunning. The wrapper is typed, so dunning does not import payment. It
// returns nil for any other result, which RetryPayment classifies.
//
//   - PaymentIntentID: a decline carries its PaymentIntent, so the failed
//     retry's timeline row matches its payment_intent.payment_failed twin.
//   - Ask: a hard decline asks the customer for a different card (#966),
//     and authentication_required asks for the confirmation. The next
//     retry's RetryBlockingDecline read is what stops the card being charged
//     again.
//   - NoReason: the warning email's reason line is Stripe's card message
//     (payment.PaymentError.CardMessage), as in the payment-failed email.
//     It is the one message Stripe says can be shown to the customer. Any
//     other refusal (an invalid_request_error, which has no PaymentIntent)
//     is worded for the developer, so its text must never become the
//     email's "Reason:".
func definiteRetryFailure(err error) *dunning.RetryError {
	var pe *payment.PaymentError
	if !errors.As(err, &pe) || pe.Unknown {
		return nil
	}
	return &dunning.RetryError{
		PaymentIntentID: pe.PaymentIntentID,
		Err:             err,
		Ask:             domain.DeclineAskOf(pe.DeclineCode),
		NoReason:        pe.CardMessage == "",
	}
}

// chargedOutcome reads what a charge that returned no error did, from the
// invoice it left behind (#1289). No error does not mean "paid". The charge
// also returns the invoice when its PaymentIntent is still processing, and
// when its stamp was refused because another outcome was recorded during
// the call. Dunning closes the run as recovered only on a paid invoice.
//
//   - Paid: nil. The retry recovered the invoice.
//   - Payment in flight: dunning.ErrPaymentInFlight. It names the
//     PaymentIntent the run now waits on. The charge keeps its hold; the
//     settle that ends the payment frees it (ADR-137).
//   - Failed: the payment_intent.payment_failed webhook recorded the failure
//     first. The result is a definite decline, built from that attempt's own
//     row (its decline code gives the Ask), so dunning counts it and warns
//     as for any decline. The invoice's last_payment_error is never the
//     reason. The round trip is over, and the failure settle freed the
//     hold with the write that recorded the failure.
//   - Anything else (pending, void, written off): a loud error, which
//     RetryPayment turns into an uncounted skip. The next tick reads the
//     invoice again.
func (a *paymentRetrierAdapter) chargedOutcome(ctx context.Context, tenantID string, inv domain.Invoice) error {
	switch {
	case inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded:
		return nil
	case inv.PaymentInFlight():
		if inv.StripePaymentIntentID == "" {
			slog.ErrorContext(ctx, "dunning retry: the charge left the invoice in flight with no PaymentIntent; skipping this attempt uncounted",
				"invoice_id", inv.ID, "payment_status", inv.PaymentStatus)
			return fmt.Errorf("dunning retry: invoice %s is %s with no PaymentIntent after the charge", inv.ID, inv.PaymentStatus)
		}
		return &dunning.RetryError{PaymentIntentID: inv.StripePaymentIntentID, Err: dunning.ErrPaymentInFlight}
	case inv.PaymentStatus == domain.PaymentFailed && inv.StripePaymentIntentID != "":
		return a.retryFailure(ctx, tenantID, inv.ID, inv.StripePaymentIntentID)
	}
	slog.ErrorContext(ctx, "dunning retry: the charge returned no error but left the invoice neither paid, in flight nor failed; skipping this attempt uncounted",
		"invoice_id", inv.ID, "status", inv.Status, "payment_status", inv.PaymentStatus)
	return fmt.Errorf("dunning retry: invoice %s is %s/%s after the charge", inv.ID, inv.Status, inv.PaymentStatus)
}

// retryFailure returns the failure of PaymentIntent pi as a counted decline:
// a *dunning.RetryError that wraps a definite *payment.PaymentError, with the
// attempt row's decline code as its Ask. The attempt row holds Stripe's
// decline code but not its card message, so the warning has no reason line.
// It never falls back to the invoice's last_payment_error. A failed read is
// logged and leaves the Ask unset, and the attempt still counts.
func (a *paymentRetrierAdapter) retryFailure(ctx context.Context, tenantID, invoiceID, pi string) error {
	var code string
	attempts, err := a.invoiceStore.ListChargeAttemptsByInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		slog.WarnContext(ctx, "dunning retry: could not read the failed attempt's decline code; the warning asks nothing specific",
			"invoice_id", invoiceID, "payment_intent_id", pi, "error", err)
	}
	for _, at := range attempts {
		if at.StripePaymentIntentID == pi {
			code = at.DeclineCode
			break
		}
	}
	return &dunning.RetryError{
		PaymentIntentID: pi,
		Err:             &payment.PaymentError{Message: "payment failed", DeclineCode: code, PaymentIntentID: pi},
		Ask:             domain.DeclineAskOf(code),
		NoReason:        true,
	}
}

// ExplainRetryFailure implements dunning.RetryFailureExplainer: a retry whose
// payment failed before its in-flight write is recorded as the decline
// retryFailure builds.
func (a *paymentRetrierAdapter) ExplainRetryFailure(ctx context.Context, tenantID, invoiceID, paymentIntentID string) error {
	return a.retryFailure(ctx, tenantID, invoiceID, paymentIntentID)
}

// stripeConnected reports whether the tenant has Stripe connected for the
// mode the retry runs in (ctx's livemode, which the dunning sweeps carry).
func (a *paymentRetrierAdapter) stripeConnected(ctx context.Context, tenantID string) (bool, error) {
	if a.stripeReady == nil {
		return false, errors.New("dunning retry: no Stripe connection check wired")
	}
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return false, err
	}
	return a.stripeReady.HasFor(ctx, tenantID, live)
}

// dunningRunStarter is the part of dunning.Service the adapter needs. It is
// an interface so a unit test can reach the disabled-skip branch.
type dunningRunStarter interface {
	StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, simFailedAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error)
}

// dunningStarterAdapter implements billing.DunningStarter over
// dunning.Service. The engine's no-payment enrollment sweep uses it to route
// a card-less, auto_charge_pending invoice into a dunning campaign.
// Otherwise that invoice would be retried forever with nothing to charge,
// and never reach a final action.
//
// StartDunning returns InvalidState for two skips that are on purpose, and
// this adapter turns both into a no-op. One is dunning disabled. The other
// is dunning not configured: the tenant has no effective or default policy.
// StartDunning maps that ErrNotFound to InvalidState, so an optional feature
// left unconfigured never fails the money-path sweep. A tenant in either
// state gets no automatic retries for a missing card, as it gets none for a
// declined card. Turning both cases into a no-op means the sweep, which runs
// every tick, does not report a false error for every stalled invoice. A
// real error (the database is down, for
// example) is not InvalidState, and still returns as a sweep error.
type dunningStarterAdapter struct {
	dunning dunningRunStarter
}

func (a *dunningStarterAdapter) StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, simFailedAt time.Time, cause domain.DunningStartCause) error {
	if _, err := a.dunning.StartDunning(ctx, tenantID, invoiceID, customerID, simFailedAt, cause); err != nil {
		if errors.Is(err, errs.ErrInvalidState) {
			return nil // dunning disabled — deliberate skip, not a sweep error
		}
		return err
	}
	return nil
}

// subscriptionPauserAdapter implements dunning.SubscriptionPauser over
// subscription.Service. It sets a keep_as_draft pause marked as dunning's.
// Paying the invoice the pause was set for lifts it (ADR-036 amendments).
type subscriptionPauserAdapter struct {
	svc *subscription.Service
}

func (a *subscriptionPauserAdapter) PauseCollection(ctx context.Context, tenantID, subID, invoiceID string) (bool, error) {
	applied, err := a.svc.PauseCollectionForDunning(ctx, tenantID, subID, invoiceID)
	return applied, dunningGraceWait(err)
}

// dunningGraceWait maps subscription's answer "the period is inside its
// invoice grace" to dunning's SubscriptionActionNotYet. The invoice grace is
// the wait after a period ends before its invoice is built. With this
// mapping, the run waits for the grace instead of counting a failed action
// (ADR-120, amendment of 2026-10-22). Every other error passes through.
func dunningGraceWait(err error) error {
	var inGrace *subscription.InsideInvoiceGraceError
	if errors.As(err, &inGrace) {
		return &dunning.SubscriptionActionNotYet{Until: inGrace.Until}
	}
	return err
}

// subscriptionCancelerAdapter implements dunning.SubscriptionCanceler over
// subscription.Service. Cancelling is Stripe's default final action for
// dunning (ADR-036 amendment).
type subscriptionCancelerAdapter struct {
	svc *subscription.Service
}

func (a *subscriptionCancelerAdapter) Cancel(ctx context.Context, tenantID, id string) error {
	// Stamp the origin, so the cancel's audit row and its outbound
	// subscription.canceled webhook name dunning as the driver. Both are
	// written in the cancel transaction. Without the stamp, each would have
	// to guess from the missing request actor (ADR-090).
	ctx = subscription.WithCancelOrigin(ctx, "dunning")
	// CancelForDunning, not the operator's Cancel: it waits for the invoice
	// grace rather than closing the ended period now.
	_, _, err := a.svc.CancelForDunning(ctx, tenantID, id)
	return dunningGraceWait(err)
}

// subscriptionStateAdapter implements dunning.SubscriptionStateReader over
// subscription.Service. It is the skip-if-done read for exhaustion, the
// final actions a run takes when its retries run out. With it, a repeated
// exhaustion skips the half that already happened, instead of failing on
// that half forever (ADR-112).
type subscriptionStateAdapter struct {
	svc *subscription.Service
}

func (a *subscriptionStateAdapter) GetTerminalState(ctx context.Context, tenantID, id string) (dunning.SubscriptionTerminalState, error) {
	sub, err := a.svc.Get(ctx, tenantID, id)
	if err != nil {
		return dunning.SubscriptionTerminalState{}, err
	}
	// Terminated marks the state in which both actions, cancel and pause,
	// refuse. The cancel's compare-and-set (CancelAtomicWithBill) matches
	// only a draft, trialing or active row, and SetPauseCollection rejects
	// canceled outright.
	st := dunning.SubscriptionTerminalState{
		Terminated: sub.Status == domain.SubscriptionCanceled,
	}
	if sub.CurrentBillingPeriodStart != nil {
		st.PeriodStart = *sub.CurrentBillingPeriodStart
	}
	return st, nil
}

// invoiceUncollectibleAdapter implements dunning.InvoiceUncollectibleMarker
// over invoice.Service. Marking the invoice uncollectible is Stripe's
// standard final action for dunning (ADR-036 amendment). then is dunning's
// escalation, run in the write-off's transaction.
type invoiceUncollectibleAdapter struct {
	svc *invoice.Service
}

func (a *invoiceUncollectibleAdapter) MarkUncollectible(ctx context.Context, tenantID, id string, then func(tx *sql.Tx) error) error {
	_, err := a.svc.MarkUncollectibleThen(ctx, tenantID, id, then)
	return err
}

// invoiceAuditStampsAdapter implements invoice.AuditStampFetcher over
// audit.Logger.Query. The timeline uses it for the wall-clock stamp it adds
// to lifecycle rows when they are read (ADR-104), and for who did it on
// lifecycle, credit-note and resolve rows (ADR-128). It is narrow on
// purpose: one resource's entries, capped. An invoice's, a credit note's or
// a payment-recovery run's audit trail is dozens of rows at most, and the
// join reads only the transition actions.
type invoiceAuditStampsAdapter struct {
	logger *audit.Logger
}

func (a *invoiceAuditStampsAdapter) ListByResource(ctx context.Context, tenantID, resourceType, resourceID string) ([]domain.AuditEntry, error) {
	// 100 is Query's hard cap: it clamps a larger limit, so asking for more
	// would be a silent fallback. Rows come newest first. A resource with
	// more than 100 audit rows would lose its oldest entries, which are the
	// transition stamps the join wants. Such a row then shows bare rather
	// than wrong. This is accepted: 100 audit rows on one invoice means
	// about 90 operator sends or exports. If a real invoice ever gets there,
	// revisit this with a query filtered by Action.
	entries, _, err := a.logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Limit:        100,
	})
	return entries, err
}

// invoiceSubscriptionLabelAdapter implements invoice.SubscriptionLabeler over
// subscription.PostgresStore. It returns the name an invoice's activity rows
// show on their link to its subscription (ADR-128 rule 8).
type invoiceSubscriptionLabelAdapter struct {
	store *subscription.PostgresStore
}

func (a *invoiceSubscriptionLabelAdapter) SubscriptionLabel(ctx context.Context, tenantID, subscriptionID string) (string, error) {
	sub, err := a.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return "", err
	}
	if sub.DisplayName != "" {
		return sub.DisplayName, nil
	}
	return sub.Code, nil
}

// subscriptionInvoiceLabelAdapter implements subscription.InvoiceLabeler over
// invoice.PostgresStore. It returns the number a subscription's activity rows
// show on their link to an invoice (ADR-128 rule 8). A missing invoice is an
// error, so its row links nothing.
type subscriptionInvoiceLabelAdapter struct {
	store *invoice.PostgresStore
}

func (a *subscriptionInvoiceLabelAdapter) InvoiceNumber(ctx context.Context, tenantID, invoiceID string) (string, error) {
	inv, err := a.store.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return "", err
	}
	return inv.InvoiceNumber, nil
}

// dunningTimelineAdapter implements invoice.DunningTimelineFetcher over
// dunning.Store.
type dunningTimelineAdapter struct {
	store *dunning.PostgresStore
}

func (a *dunningTimelineAdapter) ListRunsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDunningRun, error) {
	runs, _, err := a.store.ListRuns(ctx, dunning.RunListFilter{TenantID: tenantID, InvoiceID: invoiceID})
	return runs, err
}

func (a *dunningTimelineAdapter) ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error) {
	return a.store.ListEvents(ctx, tenantID, runID)
}

// customerEmailFetcherAdapter adapts customer.PostgresStore to
// dunning.CustomerEmailFetcher and payment.CustomerEmailResolver.
type customerEmailFetcherAdapter struct {
	store *customer.PostgresStore
}

func (a *customerEmailFetcherAdapter) GetCustomerEmail(ctx context.Context, tenantID, customerID string) (string, string, []string, error) {
	cust, err := a.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", nil, err
	}
	email := cust.Email
	name := cust.DisplayName
	// customers.email is the one canonical recipient. The billing profile's
	// legal_name still wins for display, because the operator types the
	// document's display name there. The send target is always cust.Email.
	if bp, err := a.store.GetBillingProfile(ctx, tenantID, customerID); err == nil {
		if bp.LegalName != "" {
			name = bp.LegalName
		}
	}
	// AdditionalEmails come from the same row read, with no extra query.
	// They are CC'd on the billing-state emails (dunning, receipt, payment
	// failed), as the ADR-082 coverage matrix sets out.
	return email, name, cust.AdditionalEmails, nil
}

// passwordResetEmailAdapter adapts email.OutboxSender, or the direct
// *email.Sender, to the narrower 2-arg signature of user.EmailSender. The
// user package sends password-reset emails before the operator signs in, so
// it has no tenant or display-name context. Empty values flow through to
// email.Sender.brandingFor, which falls back to the platform defaults.
// Production wires the outbox-backed sender, so reset emails are durable.
type passwordResetEmailAdapter struct {
	sender interface {
		SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error
	}
}

func (a *passwordResetEmailAdapter) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	// The email address is also the display name, so the body says "Hi
	// alice@example.com" rather than "Hi Alice". That is accepted, in
	// exchange for not passing the user's display_name through the
	// password-reset request flow. tenantID comes from
	// user.Service.IssueResetToken, so the outbox row carries the right
	// tenant for operators to see.
	return a.sender.SendPasswordReset(ctx, tenantID, email, email, resetLink)
}

// invoiceEmailEventsAdapter implements invoice.EmailEventLister over
// email.OutboxStore.ListByInvoice. The invoice timeline uses it to show the
// customer's notification emails beside Stripe webhooks and dunning events.
// It only converts shapes, because the query already keeps only the email
// types that concern an invoice.
type invoiceEmailEventsAdapter struct {
	store *email.OutboxStore
}

func (a *invoiceEmailEventsAdapter) ListByInvoice(ctx context.Context, tenantID, invoiceNumber string) ([]invoice.EmailEventRow, error) {
	rows, err := a.store.ListByInvoice(ctx, tenantID, invoiceNumber)
	if err != nil {
		return nil, err
	}
	out := make([]invoice.EmailEventRow, 0, len(rows))
	for _, r := range rows {
		to, _ := r.Payload["to"].(string)
		// JSON numbers decode to float64. A dunning_warning payload
		// carries attempt_number, which pairs the email with its retry
		// row.
		attempt, _ := r.Payload["attempt_number"].(float64)
		trigger, _ := r.Payload["trigger"].(string)
		cnNumber, _ := r.Payload["credit_note_number"].(string)
		out = append(out, invoice.EmailEventRow{
			ID:               r.ID,
			EmailType:        r.EmailType,
			Status:           r.Status,
			DeliveryState:    r.DeliveryState,
			CreatedAt:        r.CreatedAt,
			DispatchedAt:     r.DispatchedAt,
			To:               to,
			AttemptNumber:    int(attempt),
			Trigger:          trigger,
			SimEffectiveAt:   r.SimEffectiveAt,
			SendFailure:      emailSendFailure(r.Status, r.LastError),
			SkipReason:       r.SkipReason,
			CreditNoteNumber: cnNumber,
		})
	}
	return out, nil
}

// emailSendFailure maps a failed outbox row's stored error to a send failure
// that the activity line names in plain words (activity.SendFailure*). The
// stored error is the dispatcher's hErr.Error(), which keeps each sentinel's
// own message. So matching a sentinel's text is an exact key into this
// package's own errors. Any other error has no plain-words name, and stays in
// the server's records.
func emailSendFailure(status, lastError string) string {
	if status != "failed" || lastError == "" {
		return ""
	}
	switch {
	case strings.Contains(lastError, email.ErrSMTPNotConfigured.Error()):
		return activity.SendFailureSMTPNotConfigured
	case strings.Contains(lastError, email.ErrRecipientSuppressed.Error()):
		return activity.SendFailureRecipientSuppressed
	case strings.Contains(lastError, email.ErrInvalidRecipient.Error()):
		return activity.SendFailureInvalidRecipient
	}
	return ""
}

// paymentSetupEmailSender is the narrow part of email/ that
// noPaymentMethodNotifierAdapter needs. It is defined here, by its consumer,
// so the email package does not leak an interface name into the dependency
// graph of api and payment. Both *email.Sender and *email.OutboxSender
// satisfy it.
type paymentSetupEmailSender interface {
	SendPaymentSetupRequest(ctx context.Context, tenantID, to, customerName, invoiceNumber string, amountDueCents int64, currency, updateURL string, writtenOff bool, trigger string) error
}

// customerSentEmailsAdapter implements customer.SentEmailsLister over
// email.OutboxStore.ListByCustomer. It converts shapes, reads fields from the
// payload (recipient, invoice and credit-note numbers, trigger), and applies
// the send-failure mapping the invoice timeline uses. So the customer package
// does not import the email package, and both cards word an email from the
// same facts.
type customerSentEmailsAdapter struct {
	store *email.OutboxStore
}

func (a *customerSentEmailsAdapter) ListByCustomer(ctx context.Context, tenantID, customerID string) ([]customer.SentEmailOutboxRow, error) {
	rows, err := a.store.ListByCustomer(ctx, tenantID, customerID)
	if err != nil {
		return nil, err
	}
	out := make([]customer.SentEmailOutboxRow, 0, len(rows))
	for _, r := range rows {
		to, _ := r.Payload["to"].(string)
		invNum, _ := r.Payload["invoice_number"].(string)
		cnNumber, _ := r.Payload["credit_note_number"].(string)
		trigger, _ := r.Payload["trigger"].(string)
		out = append(out, customer.SentEmailOutboxRow{
			ID:               r.ID,
			EmailType:        r.EmailType,
			Recipient:        to,
			Status:           r.Status,
			DeliveryState:    r.DeliveryState,
			LastError:        r.LastError,
			CreatedAt:        r.CreatedAt,
			DispatchedAt:     r.DispatchedAt,
			InvoiceNumber:    invNum,
			InvoiceID:        r.InvoiceID,
			CreditNoteNumber: cnNumber,
			CreditNoteID:     r.CreditNoteID,
			Trigger:          trigger,
			SendFailure:      emailSendFailure(r.Status, r.LastError),
			SkipReason:       r.SkipReason,
			SimEffectiveAt:   r.SimEffectiveAt,
		})
	}
	return out, nil
}

// canEmailSetupLinks reports whether a setup-link email can be sent at all.
// It needs SMTP, the payment-update page for the link to open
// (PAYMENT_UPDATE_URL), and the page Stripe sends the customer back to
// (PAYMENT_UPDATE_RETURN_URL). Without the last one, the page refuses to add
// the card, so the email would carry a link that can never work.
// NotifyNoPaymentMethod refuses without either address. The invoice notices
// read this to decide whether to say a link is emailed and whether to offer
// Resend.
func canEmailSetupLinks(smtpConfigured bool, paymentUpdateURL, paymentUpdateReturnURL string) bool {
	return smtpConfigured && paymentUpdateURL != "" && paymentUpdateReturnURL != ""
}

// noPaymentMethodNotifierAdapter implements billing.NoPaymentMethodNotifier
// with email.SendPaymentSetupRequest. The engine uses it at finalize to send
// a "set up your payment method" email when the customer has no payment
// method ready. The post-decline path (Stripe.handlePaymentFailed) is
// different: it sends the customer to the hosted invoice URL. This email
// instead creates a single-use token for the /update-payment route of the
// single-page app (SPA). See ADR-013 and ADR-023.
//
// The link goes to the SPA with a token, not straight to Stripe Checkout.
// The SPA checks the customer-scoped token, shows "for invoice X, $Y", and
// only then creates a Checkout session. That suits a link the customer may
// click days after it is sent: the operator can invalidate tokens at any
// time, and the customer sees the invoice before reaching Stripe.
type noPaymentMethodNotifierAdapter struct {
	email            paymentSetupEmailSender
	customerEmail    *customerEmailFetcherAdapter
	paymentUpdateURL string
	// paymentUpdateReturnURL is PAYMENT_UPDATE_RETURN_URL. The page the link
	// opens refuses without it, so the email is not sent either.
	paymentUpdateReturnURL string
	tokenSvc               *payment.TokenService
	auditLogger            *audit.Logger // optional — engine fires this background task; audit row records the send for operator forensics.
}

// NotifyNoPaymentMethod emails the customer a setup link for inv. The caller
// supplies trigger, because only the caller knows why the link is being
// sent. With a fixed trigger, an operator's "Resend setup link" would write a
// permanent row saying the finalize path sent the mail, when it never ran. A
// row that names the wrong cause is false evidence (ADR-090).
func (a *noPaymentMethodNotifierAdapter) NotifyNoPaymentMethod(ctx context.Context, tenantID string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	if a.paymentUpdateURL == "" {
		return "", errors.New("PAYMENT_UPDATE_URL not configured")
	}
	if a.paymentUpdateReturnURL == "" {
		return "", errors.New("PAYMENT_UPDATE_RETURN_URL not configured")
	}
	if a.tokenSvc == nil {
		// The SPA requires ?token= and rejects a URL without it ("Link
		// expired or invalid · No payment update token provided").
		// Refusing to send is always better than emailing the customer a
		// link that can never work.
		return "", errors.New("payment update TokenService not wired")
	}
	// Drop the CC list on purpose. payment_setup_request carries a
	// single-use URL, with a token, for entering payment details. So it is
	// never CC'd (ADR-082 coverage matrix).
	to, name, _, err := a.customerEmail.GetCustomerEmail(ctx, tenantID, inv.CustomerID)
	if err != nil || to == "" {
		// A missing email is a delivery gap, not a billing failure. It is
		// still reported, not hidden. The typed skip lets the engine log
		// what really happened, and lets the resend endpoint answer with a
		// typed 409 instead of a false "sent".
		return domain.NotifySkippedNoEmail, nil
	}
	rawToken, err := a.tokenSvc.Create(ctx, tenantID, inv.CustomerID, inv.ID)
	if err != nil {
		return "", fmt.Errorf("create payment update token: %w", err)
	}
	updateURL := fmt.Sprintf("%s?token=%s", a.paymentUpdateURL, rawToken)
	if err := a.email.SendPaymentSetupRequest(ctx, tenantID, to, name, inv.InvoiceNumber, inv.AmountDueCents, inv.Currency, updateURL, inv.Status == domain.InvoiceUncollectible, trigger); err != nil {
		return "", err
	}
	// Audit the send the engine made, so the operator can answer "did we
	// email the customer at finalize?" from the AuditLog page, without
	// searching the outbox tables. The row matches the setup_link_sent row
	// that paymentmethods.Handler writes for an operator's send.
	if a.auditLogger != nil {
		// The append-only row holds no recipient address (GDPR erasure).
		// The email outbox holds the delivery record, and the row links
		// the customer.
		_ = a.auditLogger.Log(ctx, tenantID, "update", "customer", inv.CustomerID, name, map[string]any{
			"action":         "setup_link_sent",
			"invoice_id":     inv.ID,
			"invoice_number": inv.InvoiceNumber,
			"trigger":        trigger,
		})
	}
	return domain.NotifySent, nil
}

// prorationInvoiceCreatorAdapter adapts invoice.PostgresStore and
// tenant.SettingsStore to subscription.ProrationInvoiceCreator. The embedded
// store supplies the methods whose names already match
// (GetByProrationSource, FindBaseInvoiceForPeriod,
// FindFundingInvoicesForPeriod and SetAutoChargePendingTx). The methods
// below rename a store method or call the numberer.
type prorationInvoiceCreatorAdapter struct {
	*invoice.PostgresStore
	numberer invoice.InvoiceNumberer
}

// CreateInvoiceWithLineItemsTx and NextInvoiceNumberTx write the proration
// invoice on the item change's transaction, which the caller
// (subscription.Handler) owns. This adapter only forwards to the Tx methods
// of the store and the numberer.
func (a *prorationInvoiceCreatorAdapter) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

func (a *prorationInvoiceCreatorAdapter) NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	return a.numberer.NextInvoiceNumberTx(ctx, tx, tenantID)
}

// prorationTaxApplierAdapter adapts billing.Engine to
// subscription.ProrationTaxApplier. It keeps the engine's signature but
// returns subscription's own named result type, so the subscription package
// does not import billing.
type prorationTaxApplierAdapter struct {
	engine *billing.Engine
}

func (a *prorationTaxApplierAdapter) ApplyTaxToLineItems(ctx context.Context, tenantID, customerID, currency string, subtotal, discount int64, lineItems []domain.InvoiceLineItem) (subscription.ProrationTaxResult, error) {
	r, err := a.engine.ApplyTaxToLineItems(ctx, tenantID, customerID, currency, subtotal, discount, lineItems)
	if err != nil {
		return subscription.ProrationTaxResult{}, err
	}
	return subscription.ProrationTaxResult{
		// One assignment carries every tax fact (see domain/tax_facts.go). A
		// field-by-field copy here could drop a fact. One assignment cannot.
		TaxFacts:      r.TaxFacts,
		SubtotalCents: r.SubtotalCents,
		DiscountCents: r.DiscountCents,
	}, nil
}

// CommitTax commits a provider tax calculation into a reportable tax
// transaction. It calls the same engine method the cycle and create paths
// use. It is a no-op for the manual and none providers, which have no
// calculation id.
func (a *prorationTaxApplierAdapter) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	return a.engine.CommitTax(ctx, tenantID, invoiceID, calculationID)
}

// pmCustomerLookupAdapter implements paymentmethods.CustomerLookup over
// customer.PostgresStore. paymentmethods does not import customer, which
// keeps the dependency graph one-way. router.go wires this adapter, so the
// operator's send-setup-email endpoint can read the recipient address and
// display name from the canonical customer row.
type pmCustomerLookupAdapter struct {
	store *customer.PostgresStore
}

func (a *pmCustomerLookupAdapter) GetForSetupLink(ctx context.Context, tenantID, customerID string) (email, displayName string, err error) {
	c, err := a.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", err
	}
	return c.Email, c.DisplayName, nil
}

// bounceReporterAdapter connects email.Sender to customer.Service. When SMTP
// rejects a send with a permanent failure (5xx), the Sender calls
// ReportBounce with (tenantID, email, reason). This adapter finds the
// customers with that email through the blind index: a keyed hash of the
// address, so an encrypted email can still be found by exact match. It then
// sets email_status on every matching customer in that tenant. Several
// customers can share an email, which is rare, and then all are marked.
//
// It lives in internal/api so email does not import customer, which keeps
// the layers coupled one way only.
type bounceReporterAdapter struct {
	blinder *crypto.Blinder
	store   *customer.PostgresStore
	svc     *customer.Service
}

func (a *bounceReporterAdapter) ReportBounce(ctx context.Context, tenantID, email, reason string) {
	if a == nil || a.store == nil || a.svc == nil || a.blinder == nil || email == "" {
		return
	}
	blind := a.blinder.Blind(email)
	if blind == "" {
		return
	}
	matches, err := a.store.FindByEmailBlindIndex(ctx, blind, 10)
	if err != nil {
		return
	}
	for _, m := range matches {
		if m.TenantID != tenantID {
			continue
		}
		err := a.svc.MarkEmailBounced(ctx, tenantID, m.CustomerID, reason)
		// ErrNotFound is a customer deleted after the lookup: there is
		// nothing left to mark.
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			// Log every other failure at ERROR, because this line is the
			// only record of the bounce. The bounced status and its
			// customer.email_bounced event commit together, so a failure
			// loses both, and nothing sends an SMTP bounce again. Without
			// the status, IsSuppressed keeps mailing the dead address.
			slog.ErrorContext(ctx, "email bounce not recorded: the customer's bounced status and its webhook event are both lost",
				"tenant_id", tenantID, "customer_id", m.CustomerID,
				"event_type", domain.EventCustomerEmailBounced, "error", err)
		}
	}
}

// SuppressBounced and SuppressComplained (below) satisfy
// email.RecipientSuppressor for the Postmark webhook (ADR-098). They find
// customers through the blind index, as ReportBounce does, but they return
// their errors. A transient database failure then makes the webhook answer
// 5xx, and Postmark's redelivery runs the idempotent writes again. Zero
// matches is harmless, and is only logged (ADR-082): the address may be a CC
// alias that was never a customer. A store-level ErrNotFound is harmless
// too, when the customer was deleted between the lookup and the write.
func (a *bounceReporterAdapter) SuppressBounced(ctx context.Context, tenantID, email, reason string) error {
	return a.suppress(ctx, tenantID, email, reason, false)
}

func (a *bounceReporterAdapter) SuppressComplained(ctx context.Context, tenantID, email, reason string) error {
	return a.suppress(ctx, tenantID, email, reason, true)
}

func (a *bounceReporterAdapter) suppress(ctx context.Context, tenantID, email, reason string, complaint bool) error {
	if a == nil || a.store == nil || a.svc == nil || a.blinder == nil || email == "" {
		return nil
	}
	blind := a.blinder.Blind(email)
	if blind == "" {
		return nil
	}
	matches, err := a.store.FindByEmailBlindIndex(ctx, blind, 10)
	if err != nil {
		return err
	}
	for _, m := range matches {
		if m.TenantID != tenantID {
			continue
		}
		var markErr error
		if complaint {
			markErr = a.svc.MarkEmailComplained(ctx, tenantID, m.CustomerID, reason)
		} else {
			markErr = a.svc.MarkEmailBounced(ctx, tenantID, m.CustomerID, reason)
		}
		if markErr != nil && !errors.Is(markErr, errs.ErrNotFound) {
			return markErr
		}
	}
	return nil
}

// suppressionCheckerAdapter is the inverse of bounceReporterAdapter. Given
// (tenant, email), it answers "should this recipient be suppressed, because
// they already bounced or complained?". Both email.Sender and
// email.OutboxSender ask it before every send. So a bounced address does not
// get retry after retry that ends in the dead-letter queue (DLQ).
//
// It returns the first matching customer's email_status. When several
// customers share the email (rare) and any one is suppressed, the address
// counts as suppressed for that tenant. Suppressing one valid send is better
// than mailing a dead address again and again.
type suppressionCheckerAdapter struct {
	blinder *crypto.Blinder
	store   *customer.PostgresStore
}

func (a *suppressionCheckerAdapter) IsSuppressed(ctx context.Context, tenantID, emailAddr string) (bool, string, error) {
	if a == nil || a.store == nil || a.blinder == nil || emailAddr == "" || tenantID == "" {
		return false, "", nil
	}
	blind := a.blinder.Blind(emailAddr)
	if blind == "" {
		return false, "", nil
	}
	matches, err := a.store.FindByEmailBlindIndex(ctx, blind, 10)
	if err != nil {
		return false, "", err
	}
	for _, m := range matches {
		if m.TenantID != tenantID {
			continue
		}
		c, err := a.store.Get(ctx, tenantID, m.CustomerID)
		if err != nil {
			continue
		}
		switch c.EmailStatus {
		case domain.EmailStatusBounced:
			return true, "bounced: " + c.EmailBounceReason, nil
		case domain.EmailStatusComplained:
			return true, "complained", nil
		}
	}
	return false, "", nil
}

// stripeCustomerEnsurer finds or creates the Stripe customer for a Hardcount
// customer, and saves the mapping. *paymentmethods.StripeAdapter satisfies
// it. It is a one-method interface here, so api/adapters.go does not need to
// import paymentmethods directly.
type stripeCustomerEnsurer interface {
	EnsureStripeCustomer(ctx context.Context, tenantID, customerID string) (string, error)
}

// hostedInvoiceStripeAdapter implements hostedinvoice.CheckoutSessionCreator
// over *payment.StripeClients. The caller is a public, unauthenticated
// request. Livemode comes from the request ctx, where
// hostedinvoice.resolveInvoice pinned it from the GetByPublicToken row. The
// adapter picks the matching Stripe key and builds a Checkout Session in
// payment mode, with one line item that holds the whole total. Hardcount
// computes the tax, so Stripe's automatic_tax is off on purpose: the
// UnitAmount already includes the tax from the invoice row.
//
// The metadata sets hardcount_purpose=hosted_invoice_pay. The payment-intent
// webhook path then routes a successful charge to Invoice.RecordPayment by
// its invoice_id, instead of taking it for a subscription-billing charge.
//
// `ensurer` finds or creates the Stripe customer, so the Checkout session
// can attach the saved card to it. Without `customer` and
// `setup_future_usage=off_session`, Stripe takes the card but never links it
// to the Hardcount customer's Stripe customer record. The "save card" tick
// on the hosted page would then do nothing.
type hostedInvoiceStripeAdapter struct {
	clients *payment.StripeClients
	ensurer stripeCustomerEnsurer
	// sessions is the checkout-claim ledger (ADR-068). The claim is written
	// first, and the Stripe idempotency key comes from the claim, so one
	// invoice can never hold two live sessions that take payment.
	sessions *payment.CheckoutSessionStore
	// audit emits inside the transaction (ADR-090), so the hosted-checkout
	// claim, the flow's one lasting change, carries its evidence atomically.
	// It is set at construction: a public money path reached with a
	// customer's token must never run unaudited without a word.
	audit *audit.Logger
	// belowMinimum records Stripe refusing the session as below its minimum
	// charge, as a charge's refusal is recorded (payment.Stripe). For a
	// customer with no card, the Pay click is the only place Hardcount
	// learns it.
	belowMinimum checkoutBelowMinimumRecorder
}

// checkoutBelowMinimumRecorder is the one method of payment.Stripe the hosted
// Pay flow needs.
type checkoutBelowMinimumRecorder interface {
	RecordCheckoutBelowMinimum(ctx context.Context, tenantID string, inv domain.Invoice, bm *payment.BelowMinimumError)
}

func (a *hostedInvoiceStripeAdapter) CreateInvoicePaymentSession(
	ctx context.Context, tenantID string, inv domain.Invoice, successURL, cancelURL string,
) (string, error) {
	if a == nil || a.clients == nil {
		return "", fmt.Errorf("stripe not configured")
	}
	if a.sessions == nil {
		// Fail loudly. Creating a session without the claim ledger allows
		// the double charge this adapter exists to prevent (ADR-068).
		return "", fmt.Errorf("hosted invoice: checkout session store not wired")
	}
	// Livemode comes from ctx, where resolveInvoice pinned it from the
	// public_token lookup. Do not read it with a raw db.Pool query on
	// `invoices`: that table has RLS, and without app.tenant_id or
	// app.bypass_rls on the session the policy returns no rows ("resolve
	// livemode: sql: no rows in result set").
	livemode, err := postgres.Livemode(ctx)
	if err != nil {
		return "", err
	}
	sc, err := a.clients.Resolve(ctx, tenantID, livemode)
	if err != nil {
		return "", fmt.Errorf("read Stripe credentials for livemode=%v: %w", livemode, err)
	}
	if sc == nil {
		return "", errs.StripeNotConnected()
	}
	if a.ensurer == nil {
		return "", fmt.Errorf("hosted invoice: Stripe customer ensurer not wired")
	}
	stripeCustomerID, err := a.ensurer.EnsureStripeCustomer(ctx, tenantID, inv.CustomerID)
	if err != nil {
		return "", fmt.Errorf("ensure stripe customer: %w", err)
	}

	// ---- Claim protocol (ADR-068) ----
	// Reuse an open claim when its session is still live and its amount
	// still matches. Supersede it when it has expired or the amount changed.
	// When two POSTs race, the loser gets the winner's session through the
	// claim's idempotency key. To re-drive a claim is to send its Stripe
	// create again with that key.
	claim, err := a.sessions.GetOpenForInvoice(ctx, tenantID, inv.ID)
	switch {
	case err == nil:
		expired := claim.ExpiresAt != nil && time.Now().After(claim.ExpiresAt.Add(-time.Minute))
		if claim.URL == "" && time.Since(claim.CreatedAt) > checkoutPendingRedriveLimit {
			// A pending claim too old to re-drive: supersede and claim fresh.
			expired = true
		}
		drifted := claim.AmountCents != inv.AmountDueCents
		if !expired && !drifted && claim.URL != "" {
			// Plain reuse: both devices get the same session. Nothing
			// changes, because the claim already exists and only its
			// winner emitted a row. Tell the audit observer so. Otherwise
			// a customer who clicks "Pay" twice is reported as an
			// uncovered change forever. A detector that raises false
			// alarms on a normal retry is one nobody keeps (ADR-090).
			audit.MarkSkip(ctx)
			return claim.URL, nil
		}
		if !expired && !drifted && claim.URL == "" {
			// A pending claim: a crash between insert and create, or a
			// racing winner still creating. Re-drive the create with the
			// claim's own idempotency key, and Stripe returns the same
			// session either way. There is no new claim row, so there is
			// no emission: declare the skip, as above.
			audit.MarkSkip(ctx)
			return a.mintForClaim(ctx, sc, claim, stripeCustomerID, inv, successURL, cancelURL)
		}
		if (drifted || expired) && claim.StripeSessionID != "" {
			// The amount changed, or the claim is at the end of its life,
			// and its session may still be live: the customer may be
			// paying it right now. Ask Stripe for the session's real state
			// before creating a second way to pay. Expiring a completed
			// session fails, and creating a new one anyway would charge
			// twice.
			if perr := a.expireStripeSession(ctx, sc, claim.StripeSessionID); perr != nil {
				if errors.Is(perr, payment.ErrSessionCompleted) {
					// The customer already paid the old amount. The
					// webhook settles it and escalates the amount
					// mismatch. Never create a new session here.
					return "", payment.ErrChargeInFlight
				}
				// Neither expired nor confirmed closed: the old page may
				// still take payment, so a second page could take two
				// payments. Fail, and the customer retries. The old page
				// also expires on its own within the hour.
				return "", fmt.Errorf("expire the previous payment page: %w", perr)
			}
		}
		// Supersede with a compare-and-set, so exactly one concurrent
		// caller wins the right to create the new session.
		if won, sErr := a.sessions.Supersede(ctx, tenantID, claim.ID); sErr != nil {
			return "", fmt.Errorf("supersede stale claim: %w", sErr)
		} else if !won {
			// Another caller superseded and is creating the session. Fall
			// through to ClaimOpen, which returns that caller's new claim
			// (the loser protocol).
			_ = won
		}
	case errors.Is(err, errs.ErrNotFound):
		// No open claim. Claim fresh below, after closing any session that
		// a credit apply superseded. A credit apply changes the row in its
		// own transaction and never reaches Stripe. So the old session can
		// still take payment, and a new one beside it would leave the
		// customer two live pages.
		if cerr := a.expireSupersededSessions(ctx, sc, tenantID, inv.ID); cerr != nil {
			return "", cerr
		}
	default:
		return "", fmt.Errorf("read open checkout claim: %w", err)
	}

	var emit func(tx *sql.Tx, claim payment.CheckoutClaim) error
	if a.audit != nil {
		emit = func(tx *sql.Tx, claim payment.CheckoutClaim) error {
			return a.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionUpdate,
				ResourceType:  "invoice",
				ResourceID:    inv.ID,
				ResourceLabel: inv.InvoiceNumber,
				Metadata: map[string]any{
					"action":       "hosted_checkout_started",
					"amount_cents": claim.AmountCents,
					"currency":     claim.Currency,
					"claim_id":     claim.ID,
				},
			})
		}
	}
	freshClaim, _, err := a.sessions.ClaimOpenAudited(ctx, tenantID, inv.ID, inv.AmountDueCents, inv.Currency, livemode, emit)
	if err != nil {
		// ErrInvoiceNotPayable and ErrChargeInFlight return typed, and the
		// handler maps them to accurate 409s.
		return "", err
	}
	if freshClaim.URL != "" {
		// Loser protocol fast path: the winner already filled the session.
		return freshClaim.URL, nil
	}
	return a.mintForClaim(ctx, sc, freshClaim, stripeCustomerID, inv, successURL, cancelURL)
}

// hostedInvoiceCheckoutParams builds the Stripe Checkout session request for
// one hosted-invoice claim. It is a pure function of its inputs: every
// re-drive replays the claim id as the idempotency key, and Stripe rejects a
// replayed key whose params changed.
func hostedInvoiceCheckoutParams(
	claim payment.CheckoutClaim, stripeCustomerID string, inv domain.Invoice, successURL, cancelURL string,
) *stripe.CheckoutSessionCreateParams {
	currency := strings.ToLower(claim.Currency)
	productName := "Invoice " + inv.InvoiceNumber
	// Put the metadata on both the session and the PaymentIntent. Session
	// metadata lives only on the checkout_session object, and Stripe does
	// not copy it to the PaymentIntent. The webhook path routes
	// payment_intent.succeeded to Invoice.MarkPaid by reading
	// hardcount_invoice_id from the PaymentIntent's metadata
	// (payment/handler.go line 195). So PaymentIntentData.Metadata is the
	// copy that matters. The session copy lets an operator see it when
	// looking at a checkout session in the Stripe dashboard.
	meta := map[string]string{
		"hardcount_invoice_id":  inv.ID,
		"hardcount_tenant_id":   claim.TenantID,
		"hardcount_customer_id": inv.CustomerID,
		"hardcount_purpose":     payment.PurposeHostedInvoicePay,
	}
	// `customer` with `setup_future_usage=off_session` is Stripe's standard
	// pattern for "charge now, and save the card to charge future invoices
	// automatically" (https://docs.stripe.com/payments/save-during-payment).
	// Without `customer`, Stripe makes a guest checkout, and the
	// PaymentMethod is never attached to anything. Without
	// `setup_future_usage`, the one-time PaymentIntent uses up the card and
	// detaches it. Dunning's automatic retries need both.
	return &stripe.CheckoutSessionCreateParams{
		Customer: stripe.String(stripeCustomerID),
		Mode:     stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
					Currency:   stripe.String(currency),
					UnitAmount: stripe.Int64(claim.AmountCents),
					ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
						Name: stripe.String(productName),
					},
				},
			},
		},
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Metadata:         meta,
			Description:      stripe.String(productName),
			SetupFutureUsage: stripe.String("off_session"),
		},
		// Expire the session at Stripe after 1h (Stripe allows 30m at
		// least, and defaults to 24h). With only the database TTL, a session
		// could take payment for about 23h with no one seeing it (ADR-068).
		// The expiry counts from the claim's creation, not from this call.
		// Every re-drive reuses the claim id as the idempotency key, and
		// Stripe rejects a replayed key whose params changed. So an expiry
		// counted from the call would break every retry.
		ExpiresAt: stripe.Int64(checkoutSessionExpiry(claim).Unix()),
		// Charge in the invoice's own currency. Unset, Stripe applies the
		// account's Adaptive Pricing setting, and may preselect the buyer's
		// local currency with a 2-4% conversion fee. The bank statement
		// then no longer matches the invoice and receipt (for example,
		// USD 10.29 charged as INR 1,010.68).
		AdaptivePricing: &stripe.CheckoutSessionCreateAdaptivePricingParams{
			Enabled: stripe.Bool(false),
		},
		Params: stripe.Params{
			Metadata: meta,
			// The claim id, so every re-drive for this claim gets the
			// same Stripe session.
			IdempotencyKey: stripe.String(claim.ID),
		},
	}
}

// mintForClaim creates the Stripe Checkout session for one claim, or
// re-drives it idempotently, and fills the claim row. The idempotency key is
// the claim id. So a crash before the fill, a concurrent loser and a retry
// all get the same session from Stripe, and one invoice can never have two
// live sessions that take payment.
func (a *hostedInvoiceStripeAdapter) mintForClaim(
	ctx context.Context, sc *stripe.Client, claim payment.CheckoutClaim,
	stripeCustomerID string, inv domain.Invoice, successURL, cancelURL string,
) (string, error) {
	sess, err := sc.V1CheckoutSessions.Create(ctx, hostedInvoiceCheckoutParams(claim, stripeCustomerID, inv, successURL, cancelURL))
	if err != nil {
		// A definite refusal made no session, so nothing can be paid on
		// this claim: release it. Left open, an unfilled claim reads as a
		// link about to reach the customer. It then holds every charge and
		// dunning retry on the invoice for 65 minutes
		// (domain.HostedCheckoutLiveSQL). An unclear answer keeps the claim,
		// because its re-drive replays the same idempotency key.
		if payment.IsDefiniteRefusal(err) {
			if rerr := a.sessions.ReleaseRefused(ctx, claim.TenantID, claim.ID); rerr != nil {
				slog.WarnContext(ctx, "checkout: releasing a claim Stripe refused failed; it stops holding charges 65 minutes after it was made",
					"claim_id", claim.ID, "invoice_id", claim.InvoiceID, "error", rerr)
			}
		}
		// Below the minimum, the customer cannot pay this amount online at
		// all, and the page says so (409 amount_below_minimum). Record it
		// the way a charge's refusal is recorded. The invoice is stamped, so
		// the page drops its Pay button, the operator sees the notice, and
		// a payment recovery on the invoice ends. Unrecorded, a card-less
		// invoice would keep its Pay button, and its no-card recovery would
		// run on to the final action.
		if bm := payment.BelowMinimumOf(err); bm != nil {
			if a.belowMinimum == nil {
				slog.ErrorContext(ctx, "checkout: Stripe refused the session as below its minimum, and no recorder is wired to record it",
					"invoice_id", inv.ID)
			} else {
				a.belowMinimum.RecordCheckoutBelowMinimum(ctx, claim.TenantID, inv, bm)
			}
			return "", bm
		}
		return "", fmt.Errorf("checkout session: %w", err)
	}
	// Save Stripe's actual expiry, which Stripe may clamp, with a
	// compare-and-set. Losing the compare-and-set to a concurrent re-drive is
	// fine: same session, same values.
	expiresAt := time.Unix(sess.ExpiresAt, 0).UTC()
	status, fErr := a.sessions.FillSession(ctx, claim.TenantID, claim.ID, sess.ID, sess.URL, expiresAt)
	if fErr != nil {
		// Never hand out a session Hardcount has not recorded. The
		// reconciler and the expire sweeps find sessions by their recorded
		// id, so an unrecorded one could be paid with nothing ever looking
		// at it. The customer retries, and the re-drive replays the same
		// session and fills the row.
		return "", fmt.Errorf("record checkout session: %w", fErr)
	}
	if status != "open" {
		// The claim was replaced while this create was in flight, by a
		// credit apply, a settle or the pending-claim supersede. A newer
		// claim owns the invoice, so handing out this URL would leave two
		// live pages. Close this session. If this expire fails, the
		// recorded id lets the sweeps finish the job.
		if perr := a.expireStripeSession(ctx, sc, sess.ID); perr != nil {
			slog.WarnContext(ctx, "checkout: expire of a replaced claim's session failed",
				"claim_id", claim.ID, "error", perr)
		}
		return "", payment.ErrChargeInFlight
	}
	return sess.URL, nil
}

// checkoutSessionTTL is how long a hosted Checkout session stays payable at
// Stripe. domain.HostedCheckoutLiveSQL writes this window as SQL literals
// (an unfilled claim blocks charges for 1h + 5m), so a change here must
// change those too.
const checkoutSessionTTL = time.Hour

// checkoutPendingRedriveLimit bounds how long a claim whose Stripe create
// never landed may be re-driven. Stripe requires expires_at at least 30
// minutes ahead, and the expiry is fixed when the claim is created. So past
// this age, a create that never reached Stripe can no longer succeed. Such a
// claim's URL was never handed out, because mintForClaim returns a URL only
// once it is recorded. So superseding the claim leaves nothing payable
// behind.
const checkoutPendingRedriveLimit = 25 * time.Minute

func checkoutSessionExpiry(claim payment.CheckoutClaim) time.Time {
	return claim.CreatedAt.Add(checkoutSessionTTL)
}

// expireSupersededSessions expires, at Stripe, every superseded session of
// the invoice that may still be live. A completed one means the customer
// already paid it. It then returns ErrChargeInFlight, so no new session is
// created, and the webhook or the reconciler settles the payment. A session
// that can be neither expired nor confirmed closed fails the create, because
// two payable pages for one invoice could take two payments.
func (a *hostedInvoiceStripeAdapter) expireSupersededSessions(ctx context.Context, sc *stripe.Client, tenantID, invoiceID string) error {
	live, err := a.sessions.ListUnresolvedForInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return fmt.Errorf("list superseded checkout sessions: %w", err)
	}
	for _, c := range live {
		if c.Status != "superseded" {
			continue
		}
		perr := a.expireStripeSession(ctx, sc, c.StripeSessionID)
		switch {
		case perr == nil:
			if mErr := a.sessions.MarkExpired(ctx, tenantID, c.ID); mErr != nil {
				slog.WarnContext(ctx, "checkout: record superseded session expiry failed",
					"invoice_id", invoiceID, "claim_id", c.ID, "error", mErr)
			}
		case errors.Is(perr, payment.ErrSessionCompleted):
			return payment.ErrChargeInFlight
		default:
			// Unknown state: the superseded page may still take payment,
			// so do not create another one beside it. Fail, and the
			// customer retries.
			return fmt.Errorf("expire superseded payment page %s: %w", c.StripeSessionID, perr)
		}
	}
	return nil
}

// expireStripeSession tries to expire a live session, and classifies a
// failure. A completed session returns payment.ErrSessionCompleted, and the
// caller must not create a new one. An already expired session counts as
// success.
func (a *hostedInvoiceStripeAdapter) expireStripeSession(ctx context.Context, sc *stripe.Client, sessionID string) error {
	_, err := sc.V1CheckoutSessions.Expire(ctx, sessionID, &stripe.CheckoutSessionExpireParams{})
	if err == nil {
		return nil
	}
	// Classify by the session's live status. The text of the expire error is
	// not a contract.
	sess, rErr := sc.V1CheckoutSessions.Retrieve(ctx, sessionID, &stripe.CheckoutSessionRetrieveParams{})
	if rErr != nil {
		return err // original failure: state unknown, callers do not mint
	}
	switch sess.Status {
	case stripe.CheckoutSessionStatusComplete:
		return payment.ErrSessionCompleted
	case stripe.CheckoutSessionStatusExpired:
		return nil // already expired = the outcome we wanted
	}
	return err
}

// subscriptionCheckerAdapter implements customer.SubscriptionChecker over the
// subscription store, for the archive and currency guards (ADR-067,
// ADR-100).
type subscriptionCheckerAdapter struct {
	subs subscriptionReader
}

// subscriptionReader is the subscription reads the archive and currency
// guards need. Satisfied by *subscription.PostgresStore.
type subscriptionReader interface {
	List(ctx context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error)
	CustomerSubPlanCurrencies(ctx context.Context, tenantID, customerID, excludeSubID string) ([]subscription.SubPlanCurrency, error)
}

// NonTerminalForCustomer reads one status at a time: trialing, then active.
// The customer service calls it under the customer-rules lock, which creates
// and draft activations also take, so those cannot move a subscription into
// the set between the reads. A trial end (trialing to active) takes no
// customer lock. Reading trialing first finds such a subscription in the
// first read if the trial has not ended, and in the second if it has; an id
// already collected is skipped.
func (a *subscriptionCheckerAdapter) NonTerminalForCustomer(ctx context.Context, tenantID, customerID string) ([]customer.NonTerminalSubscription, error) {
	var out []customer.NonTerminalSubscription
	statuses := []string{string(domain.SubscriptionTrialing), string(domain.SubscriptionActive)}
	collected := map[string]bool{}
	for _, status := range statuses {
		subs, _, err := a.subs.List(ctx, subscription.ListFilter{
			TenantID:   tenantID,
			CustomerID: customerID,
			Status:     status,
			Limit:      100,
		})
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if collected[sub.ID] {
				continue
			}
			collected[sub.ID] = true
			name := sub.DisplayName
			if name == "" {
				name = sub.Code
			}
			out = append(out, customer.NonTerminalSubscription{ID: sub.ID, Status: string(sub.Status), Name: name})
		}
	}
	return out, nil
}

// PlanCurrencies is the currency pin's own query (ADR-100 G3): one read of
// every draft, trialing and active subscription of the customer, with no
// page, so the profile guard and the pin judge the same set. One statement
// sees one snapshot, so a trial that ends meanwhile is still in it.
func (a *subscriptionCheckerAdapter) PlanCurrencies(ctx context.Context, tenantID, customerID string) ([]customer.SubscriptionCurrency, error) {
	pairs, err := a.subs.CustomerSubPlanCurrencies(ctx, tenantID, customerID, "")
	if err != nil {
		return nil, err
	}
	out := make([]customer.SubscriptionCurrency, len(pairs))
	for i, p := range pairs {
		out[i] = customer.SubscriptionCurrency{Code: p.SubCode, Currency: p.Currency}
	}
	return out, nil
}

// memberUserDirectoryAdapter implements dashmembers.UserDirectory over the
// user package: the store for lookups and for the account and membership
// writes on the accept's transaction, and user.HashPassword for the password
// rules. It keeps dashmembers decoupled from internal/user.
type memberUserDirectoryAdapter struct {
	*user.PostgresStore
}

func (memberUserDirectoryAdapter) HashPassword(plaintext string) (string, error) {
	return user.HashPassword(plaintext)
}

// memberTenantNamerAdapter resolves the workspace display name for
// invite emails and the accept page.
type memberTenantNamerAdapter struct {
	svc *tenant.Service
}

func (a *memberTenantNamerAdapter) GetTenantName(ctx context.Context, tenantID string) (string, error) {
	t, err := a.svc.Get(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return t.Name, nil
}

// costDashboardCredits adapts the credit service to the narrow reader the
// public cost-dashboard assembler uses (usage.CostDashboardCreditReader), so
// the usage package does not import the credit domain.
type costDashboardCredits struct{ svc *credit.Service }

// CreditSummary returns, per currency (ADR-129), the customer's ledger
// balance and what is left of their prepaid commit and promotional credit.
// Both reads run on a tenant-scoped transaction in the mode already pinned on
// ctx.
func (a costDashboardCredits) CreditSummary(ctx context.Context, tenantID, customerID string) ([]usage.CostDashboardCredits, error) {
	balances, err := a.svc.GetBalance(ctx, tenantID, customerID)
	if err != nil {
		return nil, fmt.Errorf("credit balance: %w", err)
	}
	grants, err := a.svc.ListGrants(ctx, tenantID, customerID, false)
	if err != nil {
		return nil, fmt.Errorf("credit grants: %w", err)
	}
	remaining := map[string]credit.CurrencyRemaining{}
	for _, r := range grants.RemainingByCurrency {
		remaining[r.Currency] = r
	}
	out := make([]usage.CostDashboardCredits, 0, len(balances))
	for _, b := range balances {
		out = append(out, usage.CostDashboardCredits{
			Currency:                  b.Currency,
			BalanceCents:              b.BalanceCents,
			CommitRemainingCents:      remaining[b.Currency].CommitRemainingCents,
			PromotionalRemainingCents: remaining[b.Currency].PromotionalRemainingCents,
		})
	}
	return out, nil
}

// usageOpenPeriods implements usage.OpenPeriodReader. Usage ingest and the
// held-usage release read it to answer an event with its billing_status
// (ADR-120, ADR-130). Before the write, they read the open periods with
// their plans' prices. After the write, on its transaction, they read the
// meter's aggregation. The rule itself lives in usage (usage.OpenPeriodOf,
// usage.BucketsOf), and this type only reads.
type usageOpenPeriods struct {
	subs       usageLiveSubscriptions
	pricing    usageMeterReader
	thresholds usageThresholdReader
}

type usageLiveSubscriptions interface {
	ListLiveByCustomer(ctx context.Context, tenantID, customerID string) ([]domain.Subscription, error)
	ListItemIntervals(ctx context.Context, tenantID, subscriptionID string) ([]domain.ItemInterval, error)
	ListCustomerMeterCoverageEnds(ctx context.Context, tenantID, customerID, excludeSubID string, from, to time.Time) ([]domain.MeterCoverageEnd, error)
}

type usageMeterReader interface {
	GetMeterTx(ctx context.Context, tx *sql.Tx, id string) (domain.Meter, error)
	PriceMatchesAt(ctx context.Context, tenantID string, pins []pricing.PlanPin) ([]pricing.PinPrices, error)
}

type usageThresholdReader interface {
	ThresholdWatermarks(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (billedThrough, paidThrough *time.Time, err error)
}

// OpenPeriods reads, in one transaction, every active and trialing
// subscription of the customer. For each one it reads:
//   - the threshold watermarks of its current period: how far the period's
//     threshold invoices have billed and been paid;
//   - how the close bills the period (billing.PeriodPlans): the plan
//     stretches, from its billing intervals when it is active, and each
//     plan's pin, the instant its prices are read for the period;
//   - where the customer's other subscriptions stopped covering a meter in
//     the period.
//
// Then, in one more transaction, it reads each plan's prices at the plan's
// pin of the period, in the subscription's own time domain (ADR-130 §4).
// Each set of prices records whether its pin had passed. From the meters the
// plans price at their pins, it builds the period's usage windows, the same
// ones the close uses (billing.PeriodPlan.UsageWindows).
//
// The watermarks are read whether or not the subscription still has
// thresholds, because the close keys them on the period's threshold
// invoices, not on the setting.
func (a usageOpenPeriods) OpenPeriods(ctx context.Context, tenantID, customerID string) ([]usage.OpenPeriod, error) {
	subs, err := a.subs.ListLiveByCustomer(ctx, tenantID, customerID)
	if err != nil {
		return nil, fmt.Errorf("list live subscriptions: %w", err)
	}
	type subPins struct {
		sub      domain.Subscription
		wm       usage.ThresholdWatermark
		period   billing.PeriodPlan
		coverage []domain.MeterCoverageEnd
		atPin    map[string]int // plan id → index in pins, at its pin of the period
	}
	var (
		pins []pricing.PlanPin
		read []subPins
	)
	for _, sub := range subs {
		if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
			continue
		}
		start, end := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd
		sp := subPins{sub: sub, atPin: map[string]int{}}
		sp.wm.BilledThrough, sp.wm.PaidThrough, err = a.thresholds.ThresholdWatermarks(ctx, tenantID, sub.ID, start, end)
		if err != nil {
			return nil, fmt.Errorf("threshold watermarks of %s: %w", sub.ID, err)
		}
		sp.period, err = billing.PeriodPlans(sub, start, end, func() ([]domain.ItemInterval, error) {
			return a.subs.ListItemIntervals(ctx, tenantID, sub.ID)
		})
		if err != nil {
			return nil, err
		}
		sp.coverage, err = a.subs.ListCustomerMeterCoverageEnds(ctx, tenantID, sub.CustomerID, sub.ID, start, end)
		if err != nil {
			return nil, fmt.Errorf("other subscriptions' meter coverage of %s: %w", sub.ID, err)
		}
		for _, planID := range slices.Sorted(maps.Keys(sp.period.Pins)) {
			sp.atPin[planID] = len(pins)
			pins = append(pins, pricing.PlanPin{PlanID: planID, At: sp.period.Pins[planID], ClockID: sub.TestClockID})
		}
		read = append(read, sp)
	}
	out := make([]usage.OpenPeriod, 0, len(read))
	if len(read) == 0 {
		return out, nil
	}
	prices, err := a.pricing.PriceMatchesAt(ctx, tenantID, pins)
	if err != nil {
		return nil, fmt.Errorf("prices of the plans: %w", err)
	}
	priceSet := func(i int) usage.PriceSet {
		ps := usage.PriceSet{Ahead: !prices[i].Passed, ByMeter: map[string][]map[string]string{}}
		for _, pm := range prices[i].Matches {
			ps.ByMeter[pm.MeterID] = append(ps.ByMeter[pm.MeterID], pm.Match)
		}
		return ps
	}
	for _, sp := range read {
		atPin := map[string]usage.PriceSet{}
		planMeters := map[string][]string{}
		for planID, i := range sp.atPin {
			atPin[planID] = priceSet(i)
			planMeters[planID] = slices.Sorted(maps.Keys(atPin[planID].ByMeter))
		}
		var stretches []usage.Stretch
		for _, st := range sp.period.Stretches() {
			stretches = append(stretches, usage.Stretch{PlanID: st.PlanID, From: st.From, To: st.To})
		}
		windows := map[string][]usage.Window{}
		for meterID, ws := range sp.period.UsageWindows(planMeters, sp.coverage) {
			for _, w := range ws {
				windows[meterID] = append(windows[meterID], usage.Window{PlanID: w.PlanID, From: w.From, To: w.To})
			}
		}
		if p, ok := usage.OpenPeriodOf(sp.sub, stretches, windows, atPin, sp.period.BilledFrom, sp.wm); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// MeterBucketsTx reads the meter on tx, the caller's open transaction: its
// one aggregation decides its buckets (ADR-130 §2).
func (a usageOpenPeriods) MeterBucketsTx(ctx context.Context, tx *sql.Tx, meterID string) (usage.MeterBuckets, error) {
	m, err := a.pricing.GetMeterTx(ctx, tx, meterID)
	if err != nil {
		return usage.MeterBuckets{}, fmt.Errorf("get meter: %w", err)
	}
	return usage.BucketsOf(m), nil
}

// customerCurrencyFactsAdapter is invoice.CustomerCurrencyFacts: the stored
// facts a manual invoice's currency must agree with (ADR-100, ADR-129), as
// plain strings so the invoice package imports neither domain.
type customerCurrencyFactsAdapter struct {
	profiles interface {
		GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
	}
	subs interface {
		CustomerSubPlanCurrencies(ctx context.Context, tenantID, customerID, excludeSubID string) ([]subscription.SubPlanCurrency, error)
	}
}

// CustomerCurrencyFacts returns the billing profile's currency ("" when there
// is no profile) and the plan currencies of the customer's non-terminal
// subscriptions, pending plan swaps included. A read error is returned.
func (a customerCurrencyFactsAdapter) CustomerCurrencyFacts(ctx context.Context, tenantID, customerID string) (string, []string, error) {
	profile := ""
	bp, err := a.profiles.GetBillingProfile(ctx, tenantID, customerID)
	switch {
	case err == nil:
		profile = bp.Currency
	case !errors.Is(err, errs.ErrNotFound):
		return "", nil, fmt.Errorf("read billing profile: %w", err)
	}
	subs, err := a.subs.CustomerSubPlanCurrencies(ctx, tenantID, customerID, "")
	if err != nil {
		return "", nil, fmt.Errorf("read subscription plan currencies: %w", err)
	}
	plans := make([]string, 0, len(subs))
	for _, s := range subs {
		plans = append(plans, s.Currency)
	}
	return profile, plans, nil
}

// heldReliefIssuerAdapter adapts creditnote.Service to
// invoice.HeldReliefIssuer: finalize issues the relief notes held on a draft
// on its own transaction, and runs their after-commit steps once it commits.
type heldReliefIssuerAdapter struct {
	svc *creditnote.Service
}

func (a heldReliefIssuerAdapter) IssueHeldReliefTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice) (func(context.Context), error) {
	issued, err := a.svc.IssueHeldReliefTx(ctx, tx, tenantID, inv)
	if err != nil || len(issued) == 0 {
		return nil, err
	}
	return func(ctx context.Context) { a.svc.FinishHeldRelief(ctx, tenantID, issued) }, nil
}
