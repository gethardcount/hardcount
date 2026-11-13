package customer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/tax"
)

var phonePattern = regexp.MustCompile(`^[\+\d\s\-\(\)]{7,20}$`)

// countryPattern matches an uppercase ISO-3166-1 alpha-2 code. Stripe (Tax and
// payments) expects alpha-2 for every address; validating the format here turns
// a bad code ("USA") into a clean 400 instead of an opaque Stripe-side
// rejection at invoice-compute time. Mirrors the tenant company_country check.
var countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

// currencyPattern: ISO-4217 alpha-3, canonical UPPERCASE (ADR-067 currency guard).
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// StripeSyncer syncs billing profile data to Stripe when a Stripe customer exists.
type StripeSyncer interface {
	SyncBillingProfile(ctx context.Context, stripeCustomerID, customerEmail string, bp domain.CustomerBillingProfile) error
}

// TestClockChecker validates a test_clock_id at customer-create time (Stripe
// parity, ADR-027) and hands back that clock's simulated instant with it.
// Implemented by *testclock.PostgresStore.
//
// ONE method, ONE read: existence and the instant come back together, so the
// Sim the caller binds can never pair a real clock id with an instant fetched
// separately — the half-truth clock.Sim exists to make unrepresentable
// (ADR-090 §5). An "exists?" bool would force Create to go back for the
// instant, which is precisely how the two halves drift apart.
//
// ok=false means "no such clock for this tenant" (a validation failure, not an
// error).
type TestClockChecker interface {
	ResolveSim(ctx context.Context, tenantID, clockID string) (sim clock.Sim, ok bool, err error)
}

// TaxFlusher fans out a tax-retry across every draft invoice for one
// customer that's stuck on `customer_data_invalid`. Fired from
// UpsertBillingProfile after a successful save — the only operator
// action that can resolve customer_data_invalid is editing the
// billing profile, so the moment the save lands we replay the tax
// calc against the (now possibly-correct) fields. Operator never has
// to click Retry per-invoice.
//
// Narrow shape (single method, no return values used by the caller)
// keeps the customer→invoice dep one-way: customer doesn't need to
// know what tax retry means, only that it exists. Implemented by
// *invoice.Service.
//
// Mirrors the ADR-019 Stripe-reconnect flush at per-customer scope.
type TaxFlusher interface {
	RetryCustomerDataErrors(ctx context.Context, tenantID, customerID string) (int, []error)
}

// NonTerminalSubscription is one still-billing subscription blocking an
// archive (ADR-067).
type NonTerminalSubscription struct {
	ID     string
	Status string
	// Name is the subscription's display name (its code when it has none):
	// the archive refusal names subscriptions the way the page lists them.
	Name string
}

// SubscriptionCurrency is the currency of one plan a live subscription bills
// on: what the billing-profile currency guard compares against.
type SubscriptionCurrency struct {
	// Code is the subscription's code, which the refusal names.
	Code     string
	Currency string
}

// SubscriptionChecker reads a customer's subscriptions for the archive and
// currency guards. Narrow interface wired via SetSubscriptionChecker
// (precedent: SetStripeSyncer) so the customer package keeps the
// zero-peer-import rule; implemented by an adapter over the subscription
// store in api/router.go.
//
// The service calls it under the customer-rules lock
// (Store.WithCustomerRulesLock). Subscription creates and draft activations
// hold the same lock, so the answer counts every one that committed first.
// A trial that ends does not take the lock; the implementation must still
// count that subscription when it moves from trialing to active between its
// reads.
type SubscriptionChecker interface {
	// NonTerminalForCustomer returns the subscriptions that still bill:
	// active or trialing, including an active one with a scheduled cancel
	// (it bills until the boundary). A draft is left out, so an abandoned
	// draft never blocks an archive.
	NonTerminalForCustomer(ctx context.Context, tenantID, customerID string) ([]NonTerminalSubscription, error)
	// PlanCurrencies returns the plan currencies of every draft, trialing
	// and active subscription of the customer, however many, a pending plan
	// change's plan included. A draft counts: it bills from its activation
	// (ADR-100).
	PlanCurrencies(ctx context.Context, tenantID, customerID string) ([]SubscriptionCurrency, error)
}

// AuditEmitter is the narrow in-tx audit seam (ADR-090). The service builds
// the emission content at the mutation site; the store threads the closure
// onto its transaction so the business write and the audit row share fate.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

type Service struct {
	store         Store
	stripeSyncer  StripeSyncer
	clocks        TestClockChecker
	taxFlusher    TaxFlusher
	resolver      clock.Resolver
	subChecker    SubscriptionChecker
	auditLogger   AuditEmitter
	dunningPolicy DunningPolicyReader
}

// DunningPolicyReader fetches a dunning policy by id on a tenant- and
// mode-scoped transaction. Implemented by *dunning.PostgresStore, wired in
// router.go.
type DunningPolicyReader interface {
	GetPolicyByID(ctx context.Context, tenantID, id string) (domain.DunningPolicy, error)
}

// SetDunningPolicyReader wires the policy read Update runs before assigning a
// customer to a dunning policy. When unwired (narrow unit tests) the read is
// skipped.
func (s *Service) SetDunningPolicyReader(r DunningPolicyReader) { s.dunningPolicy = r }

func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetAuditLogger wires in-tx audit emission for customer creation (ADR-090).
// Nil-safe: unwired services skip emission, which keeps narrow unit tests
// fake-friendly. Distinct from Handler.SetAuditLogger (the post-hoc writer
// still used by update / billing-profile / token-rotate) — the same
// *audit.Logger satisfies both.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// SetResolver wires the unified clock.Resolver. At Update /
// UpsertBillingProfile and other customer-scoped mutations, ctx is
// bound to the customer's effective-now so downstream postgres
// timestamp writes (updated_at on customer row, billing_profile
// updates, etc.) land in simulated time on clock-pinned customers.
//
// Customer.Create can't bind from the customer pin (the customer doesn't
// exist yet), so it binds from the CLOCK directly — see the TestClockID
// branch in Create, which resolves and binds in one read via
// TestClockChecker. The resolver is not involved there and cannot be.
func (s *Service) SetResolver(r clock.Resolver) {
	s.resolver = r
}

// bindForCustomer binds effective-now from a customer pin for any
// post-create mutation (Update, UpsertBillingProfile, ArchiveCustomer).
// Returns ctx unchanged on resolver error or no resolver.
func (s *Service) bindForCustomer(ctx context.Context, tenantID, customerID string) context.Context {
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, CustomerID: customerID})
	return bound
}

// AuditCtx binds the customer's simulated-time context (its test clock + that
// clock's frozen instant) for a HANDLER-level audit emission, so the row lands
// on the sim axis (audit_log.sim_effective_at / test_clock_id) and survives
// ADR-086 teardown, which hard-deletes the customer itself.
//
// The handler needs its own bind because the service's internal bindForCustomer
// binds a ctx the handler never sees. Exported here rather than wiring a second
// clock.Resolver into the handler: one resolver per domain, one pin resolution,
// no chance of the two disagreeing.
//
// A live customer can never be clock-pinned (DB CHECK on
// test_clocks.livemode), so the bind finds no clock and leaves ctx as is;
// no-op when ctx is already bound. created_at stays wall-clock (ADR-030);
// this is the second axis.
func (s *Service) AuditCtx(ctx context.Context, tenantID, customerID string) context.Context {
	if s.resolver == nil || customerID == "" {
		return ctx
	}
	if _, ok := clock.SimOf(ctx); ok {
		return ctx
	}
	return s.bindForCustomer(ctx, tenantID, customerID)
}

// SetStripeSyncer configures Stripe sync (optional, breaks circular dep).
func (s *Service) SetStripeSyncer(syncer StripeSyncer) {
	s.stripeSyncer = syncer
}

// SetSubscriptionChecker wires the archive/currency guard's subscription
// lookup. Production always wires it (api/router.go); the guarded
// transitions FAIL LOUDLY when it's missing rather than silently
// permitting an archive that keeps billing (ADR-067).
func (s *Service) SetSubscriptionChecker(c SubscriptionChecker) {
	s.subChecker = c
}

// SetTestClockChecker wires the test-clock existence check used at
// customer-create time. Optional — if unwired, customer-create
// rejects any test_clock_id (defensive default; production wires
// the real store via api/router.go).
func (s *Service) SetTestClockChecker(checker TestClockChecker) {
	s.clocks = checker
}

// SetTaxFlusher wires the per-customer tax-retry flush fired after a
// successful billing-profile upsert. Optional — if unwired, the
// flush is a no-op and operators fall back to per-invoice Retry.
// Production wires *invoice.Service via api/router.go.
func (s *Service) SetTaxFlusher(flusher TaxFlusher) {
	s.taxFlusher = flusher
}

// MarkEmailBounced records a permanent-delivery-failure signal for the
// given customer (resolved from email by the caller via the blind-index
// lookup). The store queues customer.email_bounced in the write's
// transaction, so partners can wire their own dead-address handling and a
// crash after the commit cannot drop the event. A missing customer returns
// errs.ErrNotFound.
func (s *Service) MarkEmailBounced(ctx context.Context, tenantID, customerID, reason string) error {
	return s.store.MarkEmailBounced(ctx, tenantID, customerID, reason)
}

// MarkEmailComplained records a spam complaint against the customer's
// email (resolved from the webhook's address by the caller via the
// blind-index lookup). The store queues customer.email_complained in the
// write's transaction — per-cause, never folded into email_bounced: a
// complaint is a person saying "stop", which partners handle differently
// from a dead mailbox (and provider-side it is effectively irreversible).
func (s *Service) MarkEmailComplained(ctx context.Context, tenantID, customerID, reason string) error {
	return s.store.MarkEmailComplained(ctx, tenantID, customerID, reason)
}

type CreateInput struct {
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	// AdditionalEmails are CC'd on billing emails (ADR-082) —
	// normalized, deduped, capped at 10, never equal to the primary.
	AdditionalEmails []string `json:"additional_emails,omitempty"`
	// TestClockID pins this customer to a test clock — Stripe parity,
	// ADR-027. Test-mode only; rejected on live-mode customers via
	// the livemode check below. Once attached at create time, every
	// Subscription / Invoice for this customer uses the clock's
	// frozen_time as "now". Cannot be changed post-creation.
	TestClockID string `json:"test_clock_id,omitempty"`
}

func (s *Service) Create(ctx context.Context, tenantID string, input CreateInput) (domain.Customer, error) {
	input.ExternalID = strings.TrimSpace(input.ExternalID)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Email = strings.TrimSpace(input.Email)
	input.TestClockID = strings.TrimSpace(input.TestClockID)

	if input.ExternalID == "" {
		return domain.Customer{}, errs.Required("external_id")
	}
	if err := domain.MaxLen("external_id", input.ExternalID, 255); err != nil {
		return domain.Customer{}, err
	}
	if input.DisplayName == "" {
		return domain.Customer{}, errs.Required("display_name")
	}
	if err := domain.MaxLen("display_name", input.DisplayName, 255); err != nil {
		return domain.Customer{}, err
	}
	if input.Email != "" {
		addr, err := domain.ValidateEmail("email", input.Email)
		if err != nil {
			return domain.Customer{}, err
		}
		input.Email = addr
	}
	// Test-clock attach: validate the clock exists (mode gating happens at
	// the route layer — test_clock_id only reaches us when the caller is on a
	// test-mode key). If the checker isn't wired (defensive default) any
	// test_clock_id is rejected.
	//
	// The same read gives us the clock's instant, and we BIND it: this create
	// is the moment the customer joins the simulation, so its audit row belongs
	// on the sim axis. Nothing else can put it there. Create cannot bind from
	// the customer pin the way every later mutation does (bindForCustomer) —
	// the customer does not exist yet — so without this the origin row of every
	// simulation is the one row missing from ?test_clock_id=. It matters more
	// than the rest: ADR-086 teardown HARD-DELETES the customer, leaving this
	// row as the only surviving evidence the customer was ever in the clock's
	// world.
	if input.TestClockID != "" {
		if s.clocks == nil {
			return domain.Customer{}, errs.Invalid("test_clock_id", "test clocks are not available in this environment")
		}
		sim, ok, err := s.clocks.ResolveSim(ctx, tenantID, input.TestClockID)
		if err != nil {
			return domain.Customer{}, err
		}
		if !ok {
			return domain.Customer{}, errs.Invalid("test_clock_id", "test clock not found")
		}
		ctx = clock.WithSim(ctx, sim)
	}

	cc, err := normalizeAdditionalEmails(input.AdditionalEmails, input.Email)
	if err != nil {
		return domain.Customer{}, err
	}

	return s.store.CreateAudited(ctx, tenantID, domain.Customer{
		ExternalID:       input.ExternalID,
		DisplayName:      input.DisplayName,
		Email:            input.Email,
		AdditionalEmails: cc,
		TestClockID:      input.TestClockID,
	}, s.createEmission(ctx))
}

// createEmission builds the in-tx audit emission for a customer create.
// Wire strings (action "create", resource "customer") are FROZEN — the
// dashboard's filter vocabulary and its /customers/{id} deep-link key on
// them.
//
// PII rule (pinned by TestAudit_NoCustomerPIIInMetadata): audit_log is
// append-only at the DB level, so a customer email copied into metadata
// could never be erased — a GDPR-deletion dead end. Record the FLAG
// (email_set) and the operator's own external_id; the address itself stays
// on the mutable, erasable customer row this audit row links to.
func (s *Service) createEmission(ctx context.Context) func(tx *sql.Tx, out domain.Customer) error {
	if s.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.Customer) error {
		return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionCreate,
			ResourceType:  "customer",
			ResourceID:    out.ID,
			ResourceLabel: out.DisplayName,
			Metadata: map[string]any{
				"external_id": out.ExternalID,
				"email_set":   out.Email != "",
			},
		})
	}
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (domain.Customer, error) {
	return s.store.Get(ctx, tenantID, id)
}

// RotateCostDashboardToken mints a fresh public cost-dashboard token
// and writes it to the customer row, invalidating the old token
// (read-only public surface; no grace window needed). Returns the
// raw token — only chance to capture it; the database stores it
// plaintext so the public route can compare directly (256-bit
// entropy makes brute-force infeasible, same shape as the hosted-
// invoice public_token).
func (s *Service) RotateCostDashboardToken(ctx context.Context, tenantID, customerID string) (string, error) {
	ctx = s.bindForCustomer(ctx, tenantID, customerID)
	if _, err := s.store.Get(ctx, tenantID, customerID); err != nil {
		return "", err
	}
	token, err := NewCostDashboardToken()
	if err != nil {
		return "", err
	}
	if err := s.store.SetCostDashboardToken(ctx, tenantID, customerID, token); err != nil {
		return "", err
	}
	return token, nil
}

// GetByCostDashboardToken resolves the customer behind a public token.
// Used by the unauthenticated public route — the lookup is RLS-bypass
// (token IS the credential), and the returned tenant_id is the scope
// every downstream call uses.
func (s *Service) GetByCostDashboardToken(ctx context.Context, token string) (domain.Customer, error) {
	return s.store.GetByCostDashboardToken(ctx, token)
}

func (s *Service) List(ctx context.Context, filter ListFilter) ([]domain.Customer, int, error) {
	return s.store.List(ctx, filter)
}

// ListByTestClockID returns one page of the customers pinned to the given
// test clock, and their total. Exposed as a service method (not just a
// store passthrough) so the testclock domain can depend on the customer
// service's narrow CustomerReader interface — keeps the per-domain rule
// intact and the decrypt step on the customer read path.
func (s *Service) ListByTestClockID(ctx context.Context, tenantID, clockID string, limit, offset int) ([]domain.Customer, int, error) {
	return s.store.ListByTestClockID(ctx, tenantID, clockID, limit, offset)
}

type UpdateInput struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	Status      string `json:"status,omitempty"`
	// DunningPolicyID assigns this customer to a specific dunning
	// policy (ADR-036). Pointer so the handler can distinguish
	// "unset" (omitted) from "clear" (explicit empty string). nil =
	// leave as-is; *"" = clear assignment (fall back to default);
	// *"hc_dpol_..." = assign to that policy.
	DunningPolicyID *string `json:"dunning_policy_id,omitempty"`
	// AdditionalEmails replaces the CC list (ADR-082). Pointer for the
	// same omitted-vs-clear distinction: nil = leave as-is; *[] =
	// clear; *[...] = validated replacement.
	AdditionalEmails *[]string `json:"additional_emails,omitempty"`
}

// Update applies input to the customer and saves it. It reads the customer,
// checks its rules and writes it under the customer-rules lock
// (Store.WithCustomerRulesLock). Two updates at once run one after the
// other, and the second applies its fields to the row the first saved, so
// neither writes back a field it did not send. An archive and a subscription
// create are ordered the same way. An email change clears the bounce state
// in the same write. The Stripe sync runs after the commit.
func (s *Service) Update(ctx context.Context, tenantID, id string, input UpdateInput) (domain.Customer, error) {
	ctx = s.bindForCustomer(ctx, tenantID, id)

	// Checks that read only the input run before the lock.
	name := strings.TrimSpace(input.DisplayName)
	var email string
	if e := strings.TrimSpace(input.Email); e != "" {
		addr, err := domain.ValidateEmail("email", e)
		if err != nil {
			return domain.Customer{}, err
		}
		email = addr
	}
	status := domain.CustomerStatus(input.Status)
	// Checked here, not left to customers_status_check: the constraint's
	// error is a 500, which the idempotency middleware caches.
	if status != "" && status != domain.CustomerStatusActive && status != domain.CustomerStatusArchived {
		return domain.Customer{}, errs.Invalid("status", "must be active or archived")
	}

	var updated domain.Customer
	err := s.store.WithCustomerRulesLock(ctx, tenantID, id, func(tx *sql.Tx, locked domain.Customer) error {
		next := locked
		if name != "" {
			next.DisplayName = name
		}
		if email != "" {
			next.Email = email
		}
		if status != "" {
			// Archive = 409-BLOCK while any subscription still bills, never
			// auto-cancel (ADR-067). Auto-cancel would run the immediate-cancel
			// billing path — final prorated invoice + auto-charge of the saved
			// card — turning "archive to stop billing" into N surprise charges.
			// Archive only hides the customer + blocks NEW subscriptions;
			// existing unpaid invoices remain collectible and dunning continues
			// (both are invoice-driven). The operator cancels subs first, then
			// archives. The check runs under the lock, so a create or an
			// activation that committed before it is counted, and one that
			// comes later waits and then sees the archive.
			if status == domain.CustomerStatusArchived && locked.Status != domain.CustomerStatusArchived {
				if s.subChecker == nil {
					// Fail closed: an unenforced archive silently keeps charging
					// the customer against explicit operator intent — the exact
					// HIGH this guard exists to prevent.
					return fmt.Errorf("subscription checker not wired: cannot verify archive safety")
				}
				blocking, err := s.subChecker.NonTerminalForCustomer(ctx, tenantID, id)
				if err != nil {
					return fmt.Errorf("check subscriptions before archive: %w", err)
				}
				if len(blocking) > 0 {
					return errs.InvalidState(archiveBlockedMessage(blocking))
				}
			}
			next.Status = status
		}
		if input.DunningPolicyID != nil {
			policyID := strings.TrimSpace(*input.DunningPolicyID)
			// An empty id clears the assignment. A set one must be a policy in
			// the caller's tenant and mode: the foreign key is on the id alone
			// and foreign-key checks do not apply row-level security, so the
			// update would accept another tenant's or the other mode's policy.
			if policyID != "" && s.dunningPolicy != nil {
				if _, err := s.dunningPolicy.GetPolicyByID(ctx, tenantID, policyID); err != nil {
					if errors.Is(err, errs.ErrNotFound) {
						return errs.NotFound("dunning policy")
					}
					return fmt.Errorf("get dunning policy: %w", err)
				}
			}
			next.DunningPolicyID = policyID
		}
		if input.AdditionalEmails != nil {
			cc, err := normalizeAdditionalEmails(*input.AdditionalEmails, next.Email)
			if err != nil {
				return err
			}
			next.AdditionalEmails = cc
		} else if next.Email != locked.Email {
			// Primary changed without touching the CC list: if the new
			// primary is already on the stored list it would be both To and
			// Cc on every send — reject rather than silently rewrite the
			// operator's list.
			for _, cc := range next.AdditionalEmails {
				if strings.EqualFold(cc, next.Email) {
					return errs.Invalid("email",
						"this address is on the additional-emails list — remove it there first")
				}
			}
		}
		// A bounce or complaint belonged to the old address, so an email
		// change clears it in the same write. Left in place, it would make
		// the suppression gate drop every send to the new address.
		var err error
		updated, err = s.store.UpdateTx(ctx, tx, next, next.Email != locked.Email)
		return err
	})
	if err != nil {
		return domain.Customer{}, err
	}

	// Stripe sync (2026-05-29): propagate email + display_name changes
	// to the linked Stripe Customer. Pre-fix only UpsertBillingProfile
	// triggered Stripe sync; customer.Update silently diverged. Best-
	// effort: local save succeeded, Stripe sync failure logs but doesn't
	// fail the operator action (matches Lago/Recurly's "local is source
	// of truth, sync is downstream" model).
	if s.stripeSyncer != nil && updated.StripeCustomerID != "" {
		// Pull the billing profile so the full set of Stripe-relevant
		// fields stay in lockstep. ErrNotFound = no profile yet —
		// pass an empty BP, SyncBillingProfile gracefully skips the
		// fields that aren't set.
		bp, bpErr := s.store.GetBillingProfile(ctx, tenantID, id)
		if bpErr != nil && !errors.Is(bpErr, errs.ErrNotFound) {
			slog.WarnContext(ctx, "fetch billing profile for Stripe sync failed",
				"tenant_id", tenantID, "customer_id", id, "error", bpErr)
		}
		if syncErr := s.stripeSyncer.SyncBillingProfile(ctx, updated.StripeCustomerID, updated.Email, bp); syncErr != nil {
			slog.WarnContext(ctx, "failed to sync customer update to Stripe",
				"tenant_id", tenantID, "customer_id", id,
				"stripe_customer_id", updated.StripeCustomerID, "error", syncErr)
		}
	}

	return updated, nil
}

// GetDunningPolicyID returns the customer's assigned dunning_policy_id
// (empty string = no explicit assignment, dunning service falls back
// to the tenant default). Satisfies dunning.CustomerPolicyReader so
// the dunning service can resolve effective policy without importing
// the customer package.
func (s *Service) GetDunningPolicyID(ctx context.Context, tenantID, customerID string) (string, error) {
	c, err := s.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", err
	}
	return c.DunningPolicyID, nil
}

// deriveProfileStatus answers exactly one question: can the invoice
// Bill-To block be rendered in full from this profile? That block prints
// legal name, address_line1, city, and country (the BILL TO block in
// internal/invoice/pdf.go RenderPDF), so those four are the completeness
// set.
//
// Deliberately NOT required: postal_code and state. Both are legitimately
// absent in real jurisdictions — Hong Kong and the UAE issue no postal
// codes, and most countries have no state/province — so requiring them
// would mark correct profiles incomplete. tax_id is likewise excluded: it
// is only mandatory under reverse_charge, which UpsertBillingProfile
// already hard-rejects above, so a stored profile always satisfies its own
// tax_status requirements.
//
// BillingProfileMissing is not reachable here: it means "no profile row at
// all", which the API surfaces as a 404 from GetBillingProfile, not as a
// status on an existing row.
func deriveProfileStatus(bp domain.CustomerBillingProfile) domain.BillingProfileStatus {
	for _, required := range []string{bp.LegalName, bp.AddressLine1, bp.City, bp.Country} {
		if strings.TrimSpace(required) == "" {
			return domain.BillingProfileIncomplete
		}
	}
	return domain.BillingProfileReady
}

func (s *Service) UpsertBillingProfile(ctx context.Context, tenantID string, bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error) {
	if bp.CustomerID == "" {
		return domain.CustomerBillingProfile{}, errs.Required("customer_id")
	}
	// The customer must be the caller's. The foreign key on customer_id is on
	// the id alone and foreign-key checks do not apply row-level security, so
	// without this read the upsert accepts any existing customer id,
	// including another tenant's or the other mode's.
	if _, err := s.store.Get(ctx, tenantID, bp.CustomerID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.CustomerBillingProfile{}, errs.NotFound("customer")
		}
		return domain.CustomerBillingProfile{}, fmt.Errorf("get customer: %w", err)
	}
	ctx = s.bindForCustomer(ctx, tenantID, bp.CustomerID)
	if phone := strings.TrimSpace(bp.Phone); phone != "" {
		if !phonePattern.MatchString(phone) {
			return domain.CustomerBillingProfile{}, errs.Invalid("phone", "must be 7-20 characters and contain only digits, spaces, +, -, (, )")
		}
	}
	// Normalize + format-validate the billing country to ISO-3166 alpha-2 so it
	// stores canonically ("US", not "us"/" US ") and a bad code fails as a clean
	// 400 here rather than as an opaque Stripe Tax rejection at invoice time. The
	// dashboard already sends a code from a fixed list; this closes the
	// direct-API bypass, like the tax-status enforcement below.
	bp.Country = strings.ToUpper(strings.TrimSpace(bp.Country))
	if bp.Country != "" && !countryPattern.MatchString(bp.Country) {
		return domain.CustomerBillingProfile{}, errs.Invalid("country", "must be an ISO-3166 alpha-2 country code (e.g. 'IN', 'US')")
	}
	// Currency: normalize to canonical UPPERCASE (lowercase broke
	// analytics/dunning filters once), format-validate, and — the money
	// guard — reject a currency that mismatches a still-billing
	// subscription's plan currency. The profile currency OVERRIDES the plan
	// currency at every invoice writer, so a mismatch silently
	// re-denominates plan prices ($100 plan invoiced as €100 or ¥100) with
	// no conversion. ADR-067.
	bp.Currency = strings.ToUpper(strings.TrimSpace(bp.Currency))
	if bp.Currency != "" {
		if !currencyPattern.MatchString(bp.Currency) {
			return domain.CustomerBillingProfile{}, errs.Invalid("currency", "must be an ISO-4217 alpha-3 currency code (e.g. 'USD', 'EUR')")
		}
		// Allowlist too (ADR-100): the profile currency is the HIGHEST-
		// precedence invoice-header source, so a well-formed-but-unsupported
		// code here (JPY, or a fictional "QQQ") would relabel every invoice.
		// Format check stays first for its friendlier message on malformed
		// input.
		if err := domain.ValidateCurrency(bp.Currency); err != nil {
			return domain.CustomerBillingProfile{}, err
		}
		// The check against the customer's subscriptions runs under the
		// customer-rules lock, with the write (below).
	}
	// Normalize + format-validate tax IDs. Unknown kinds pass through untouched
	// so we don't reject jurisdictions we haven't added explicit support for.
	// Type is normalized to its canonical Stripe code so storage stays
	// consistent regardless of which alias the caller supplied.
	bp.TaxIDType = tax.NormalizeTaxIDType(bp.TaxIDType)
	bp.TaxID = tax.NormalizeTaxID(bp.TaxID)
	if err := tax.ValidateTaxID(bp.TaxIDType, bp.TaxID); err != nil {
		return domain.CustomerBillingProfile{}, errs.Invalid("tax_id", err.Error())
	}
	// Default to standard tax status so zero-valued API requests behave
	// predictably. The DB CHECK constraint mirrors this enum; validating
	// here produces a clean 400 instead of a constraint error.
	if bp.TaxStatus == "" {
		bp.TaxStatus = tax.StatusStandard
	}
	switch bp.TaxStatus {
	case tax.StatusStandard, tax.StatusExempt, tax.StatusReverseCharge:
	default:
		return domain.CustomerBillingProfile{}, errs.Invalid("tax_status", "must be 'standard', 'exempt', or 'reverse_charge'")
	}
	bp.TaxExemptReason = strings.TrimSpace(bp.TaxExemptReason)
	// Enforce the data each non-standard status legally requires so the
	// invoice can render a defensible legend:
	//   - exempt needs a reason (reseller certificate, non-profit, government)
	//     for the invoice + audit trail.
	//   - reverse_charge needs the buyer's tax ID — "tax payable by the
	//     recipient" is only valid when the buyer is a registered business, so
	//     a $0 reverse-charge invoice with no buyer VAT number is legally
	//     unsupportable.
	// The dashboard already guards both; enforcing here closes the direct-API
	// bypass the tax-flow audit surfaced.
	switch bp.TaxStatus {
	case tax.StatusExempt:
		if bp.TaxExemptReason == "" {
			return domain.CustomerBillingProfile{}, errs.Invalid("tax_exempt_reason", "a reason is required when tax_status is 'exempt'")
		}
	case tax.StatusReverseCharge:
		if bp.TaxID == "" {
			return domain.CustomerBillingProfile{}, errs.Invalid("tax_id", "a buyer tax ID is required when tax_status is 'reverse_charge'")
		}
	}
	// Derived, never caller-asserted. profile_status names a verdict about
	// the stored data, so accepting it from the request body let a client
	// PUT {profile_status:'ready'} onto a profile with no legal name and no
	// address and have the API echo "ready" back. It was also the one enum
	// on this endpoint with no service-side validation, so an unknown value
	// fell through to the DB CHECK and surfaced as a 500 rather than the
	// clean 400 tax_status gets above.
	bp.ProfileStatus = deriveProfileStatus(bp)

	// The currency check and the write run under the customer-rules lock. A
	// subscription create or activation holds the same lock until it
	// commits, so the check counts one that committed first, and one that
	// comes later waits and then checks against this profile.
	var result domain.CustomerBillingProfile
	err := s.store.WithCustomerRulesLock(ctx, tenantID, bp.CustomerID, func(tx *sql.Tx, _ domain.Customer) error {
		if err := s.checkProfileCurrency(ctx, tenantID, bp); err != nil {
			return err
		}
		var err error
		result, err = s.store.UpsertBillingProfileTx(ctx, tx, tenantID, bp)
		return err
	})
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}

	// Note: billing-profile.email was removed in migration 0100.
	// customers.email is now the single canonical recipient (Phase 1 of
	// the dual-email collapse). The prior BP→customer sync hack is
	// obsolete. Phase 2 (multi-recipient via customer_email_contacts
	// table) will be additive when a DP requests it.

	// Sync to Stripe if a Stripe customer exists. Reads from the
	// canonical mapping on customers.stripe_customer_id (migration
	// 0096); the legacy customer_payment_setups summary was retired
	// in 0097.
	if s.stripeSyncer != nil {
		if cust, getErr := s.store.Get(ctx, tenantID, bp.CustomerID); getErr == nil && cust.StripeCustomerID != "" {
			if syncErr := s.stripeSyncer.SyncBillingProfile(ctx, cust.StripeCustomerID, cust.Email, result); syncErr != nil {
				slog.Warn("failed to sync billing profile to Stripe",
					"customer_id", bp.CustomerID, "stripe_customer_id", cust.StripeCustomerID, "error", syncErr)
				// Non-fatal: local save succeeded, Stripe sync is best-effort
			}
		}
	}

	// Fan out a tax retry across every draft invoice for this customer
	// stuck on `customer_data_invalid`. The operator's edit IS the
	// retry trigger — same shape as ADR-019's Stripe-reconnect flush
	// scoped to one customer. Best-effort: per-invoice failures are
	// logged but don't roll back the profile save.
	if s.taxFlusher != nil {
		processed, retryErrs := s.taxFlusher.RetryCustomerDataErrors(ctx, tenantID, bp.CustomerID)
		if processed > 0 || len(retryErrs) > 0 {
			slog.InfoContext(ctx, "billing profile flush retried tax errors",
				"tenant_id", tenantID,
				"customer_id", bp.CustomerID,
				"processed", processed,
				"errors", len(retryErrs),
			)
		}
		for _, retryErr := range retryErrs {
			slog.WarnContext(ctx, "billing profile flush tax retry failed",
				"tenant_id", tenantID,
				"customer_id", bp.CustomerID,
				"error", retryErr,
			)
		}
	}

	return result, nil
}

// checkProfileCurrency refuses a profile currency that differs from a plan
// currency of the customer's draft, trialing or active subscriptions
// (InvalidState). The profile currency replaces the plan currency on every
// invoice, so a mismatch would relabel plan prices without conversion
// ($100 invoiced as €100). An empty currency checks nothing.
// UpsertBillingProfile calls it under the customer-rules lock.
func (s *Service) checkProfileCurrency(ctx context.Context, tenantID string, bp domain.CustomerBillingProfile) error {
	if bp.Currency == "" {
		return nil
	}
	if s.subChecker == nil {
		// Test-only path — production wires the checker in router.go. The
		// format validation still applies; only the cross-sub mismatch
		// check is skipped.
		slog.Warn("billing profile currency guard skipped: subscription checker not wired",
			"customer_id", bp.CustomerID)
		return nil
	}
	subs, err := s.subChecker.PlanCurrencies(ctx, tenantID, bp.CustomerID)
	if err != nil {
		return fmt.Errorf("check subscriptions for currency guard: %w", err)
	}
	for _, sub := range subs {
		if sub.Currency != "" && !strings.EqualFold(sub.Currency, bp.Currency) {
			return errs.InvalidState(fmt.Sprintf(
				"billing profile currency %s conflicts with subscription %s's plan currency %s — invoices would relabel plan prices in %s without conversion; cancel the subscription or keep the currency",
				bp.Currency, sub.Code, strings.ToUpper(sub.Currency), bp.Currency))
		}
	}
	return nil
}

func (s *Service) GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error) {
	return s.store.GetBillingProfile(ctx, tenantID, customerID)
}

// normalizeAdditionalEmails delegates to the shared canonicalizer so
// the stored list and the per-send overrides (invoice/CN send
// handlers) enforce identical rules from one source.
func normalizeAdditionalEmails(list []string, primaryEmail string) ([]string, error) {
	return domain.NormalizeAdditionalEmails(list, primaryEmail)
}

// archiveBlockedMessage is the refusal to archive a customer whose
// subscriptions still bill (ADR-067). It names them as the customer page
// lists them, by name, not by id: "Pro monthly" means something to the
// operator reading it, "hc_sub_d9o…" does not. A subscription with no name
// falls back to its id so it can still be found.
func archiveBlockedMessage(blocking []NonTerminalSubscription) string {
	names := make([]string, 0, len(blocking))
	for _, b := range blocking {
		name := b.Name
		if name == "" {
			name = b.ID
		}
		names = append(names, name)
	}
	what, them := "1 subscription that still bills", "it"
	if len(blocking) > 1 {
		what, them = fmt.Sprintf("%d subscriptions that still bill", len(blocking)), "them"
	}
	return fmt.Sprintf("This customer has %s (%s). Cancel %s first; archiving doesn't stop billing.",
		what, strings.Join(names, ", "), them)
}
