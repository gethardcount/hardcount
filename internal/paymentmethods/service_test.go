package paymentmethods

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// memStore is a tight in-memory stand-in for PostgresStore. It mirrors the
// invariants enforced by the schema — unique (tenant,livemode,stripe_pm),
// at most one active default per customer — so Service-level tests see
// the same semantics as production.
type memStore struct {
	mu   sync.Mutex
	next int
	rows map[string]PaymentMethod // id → pm
}

func newMemStore() *memStore { return &memStore{rows: map[string]PaymentMethod{}} }

func (m *memStore) List(_ context.Context, tenantID, customerID string) ([]PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PaymentMethod
	for _, pm := range m.rows {
		if pm.TenantID == tenantID && pm.CustomerID == customerID && pm.DetachedAt == nil {
			out = append(out, pm)
		}
	}
	// defaults first, then newest first — mirrors store.go ordering
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if lessPM(out[j], out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func lessPM(a, b PaymentMethod) bool {
	if a.IsDefault != b.IsDefault {
		return a.IsDefault
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func (m *memStore) Get(_ context.Context, tenantID, pmID string) (PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm, ok := m.rows[pmID]
	if !ok || pm.TenantID != tenantID {
		return PaymentMethod{}, errs.ErrNotFound
	}
	return pm, nil
}

func (m *memStore) Upsert(_ context.Context, tenantID string, in PaymentMethod, makeDefault bool) (PaymentMethod, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A card already removed stays removed, untouched (as the Postgres
	// store returns it before any write).
	for _, existing := range m.rows {
		if existing.TenantID == tenantID && existing.StripePaymentMethodID == in.StripePaymentMethodID &&
			existing.DetachedAt != nil {
			return existing, false, nil
		}
	}

	// makeDefault: this card takes the default, so every other active
	// default of the customer is cleared first, on the insert and the
	// conflict path alike (as the Postgres store does in its tx).
	replaced := false
	if makeDefault {
		for id, row := range m.rows {
			if row.TenantID == tenantID && row.CustomerID == in.CustomerID && row.IsDefault &&
				row.DetachedAt == nil && row.StripePaymentMethodID != in.StripePaymentMethodID {
				row.IsDefault = false
				row.UpdatedAt = time.Now().UTC()
				m.rows[id] = row
				replaced = true
			}
		}
	}

	for id, existing := range m.rows {
		if existing.TenantID == tenantID && existing.StripePaymentMethodID == in.StripePaymentMethodID {
			existing.CardBrand, existing.CardLast4 = in.CardBrand, in.CardLast4
			existing.CardExpMonth, existing.CardExpYear = in.CardExpMonth, in.CardExpYear
			existing.IsDefault = existing.IsDefault || makeDefault
			existing.UpdatedAt = time.Now().UTC()
			m.rows[id] = existing
			return existing, replaced, nil
		}
	}

	hasDefault := false
	for _, existing := range m.rows {
		if existing.TenantID == tenantID && existing.CustomerID == in.CustomerID &&
			existing.IsDefault && existing.DetachedAt == nil {
			hasDefault = true
			break
		}
	}

	m.next++
	id := "vlx_pm_mem_" + in.StripePaymentMethodID
	now := time.Now().UTC()
	row := PaymentMethod{
		ID:                    id,
		TenantID:              tenantID,
		CustomerID:            in.CustomerID,
		StripePaymentMethodID: in.StripePaymentMethodID,
		Type:                  orDefault(in.Type, "card"),
		CardBrand:             in.CardBrand,
		CardLast4:             in.CardLast4,
		CardExpMonth:          in.CardExpMonth,
		CardExpYear:           in.CardExpYear,
		IsDefault:             !hasDefault || makeDefault,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	m.rows[id] = row
	return row, replaced, nil
}

func (m *memStore) SetDefault(_ context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.rows[pmID]
	if !ok || target.TenantID != tenantID || target.CustomerID != customerID || target.DetachedAt != nil {
		return PaymentMethod{}, errs.ErrNotFound
	}
	for id, row := range m.rows {
		if row.TenantID == tenantID && row.CustomerID == customerID && row.IsDefault && id != pmID {
			row.IsDefault = false
			row.UpdatedAt = time.Now().UTC()
			m.rows[id] = row
		}
	}
	target.IsDefault = true
	target.UpdatedAt = time.Now().UTC()
	m.rows[pmID] = target
	return target, nil
}

func (m *memStore) Detach(_ context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[pmID]
	if !ok || row.TenantID != tenantID || row.CustomerID != customerID {
		return PaymentMethod{}, errs.ErrNotFound
	}
	if row.DetachedAt == nil {
		now := time.Now().UTC()
		row.DetachedAt = &now
	}
	row.IsDefault = false
	row.UpdatedAt = time.Now().UTC()
	m.rows[pmID] = row
	return row, nil
}

func (m *memStore) CustomersWithDefault(_ context.Context, tenantID string, customerIDs []string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := make(map[string]bool, len(customerIDs))
	for _, id := range customerIDs {
		want[id] = true
	}
	out := map[string]bool{}
	for _, row := range m.rows {
		if row.TenantID == tenantID && want[row.CustomerID] && row.IsDefault && row.DetachedAt == nil {
			out[row.CustomerID] = true
		}
	}
	return out, nil
}

func (m *memStore) DetachAndRebalance(_ context.Context, tenantID, customerID, pmID string) (PaymentMethod, *PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[pmID]
	if !ok || row.TenantID != tenantID || row.CustomerID != customerID {
		return PaymentMethod{}, nil, errs.ErrNotFound
	}
	wasDefault := row.IsDefault && row.DetachedAt == nil
	if row.DetachedAt == nil {
		now := time.Now().UTC()
		row.DetachedAt = &now
	}
	row.IsDefault = false
	row.UpdatedAt = time.Now().UTC()
	m.rows[pmID] = row

	var newDefault *PaymentMethod
	if wasDefault {
		// Promote the newest remaining active card (created_at DESC, id DESC).
		var best *PaymentMethod
		for id, r := range m.rows {
			if r.TenantID != tenantID || r.CustomerID != customerID || r.DetachedAt != nil {
				continue
			}
			cand := r
			if best == nil || cand.CreatedAt.After(best.CreatedAt) ||
				(cand.CreatedAt.Equal(best.CreatedAt) && id > best.ID) {
				best = &cand
			}
		}
		if best != nil {
			best.IsDefault = true
			best.UpdatedAt = time.Now().UTC()
			m.rows[best.ID] = *best
			promoted := m.rows[best.ID]
			newDefault = &promoted
		}
	}
	return row, newDefault, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---------------------------------------------------------------------------
// fakeStripe — only what Service touches.
// ---------------------------------------------------------------------------
type fakeStripe struct {
	setupCalls      int
	detachCalls     int
	detachErr       error
	setDefaultCalls int
	setDefaultErr   error
	setDefaultPMID  string
}

func (f *fakeStripe) CreateSetupIntent(_ context.Context, _ string, _ map[string]string) (string, string, error) {
	f.setupCalls++
	return "seti_secret_fake", "seti_fake", nil
}
func (f *fakeStripe) CreateSetupCheckoutSession(_ context.Context, _, _, _ string, _ map[string]string) (string, string, error) {
	return "https://checkout.stripe.com/fake", "cs_fake", nil
}
func (f *fakeStripe) EnsureStripeCustomer(_ context.Context, _, _ string) (string, error) {
	return "cus_stripe_fake", nil
}
func (f *fakeStripe) DetachPaymentMethod(_ context.Context, _ string) error {
	f.detachCalls++
	return f.detachErr
}
func (f *fakeStripe) FetchPaymentMethodCard(_ context.Context, _ string) (CardMetadata, error) {
	return CardMetadata{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030, Fingerprint: "fp_fake_4242"}, nil
}
func (f *fakeStripe) SetDefaultPaymentMethod(_ context.Context, _, _, stripePMID string) error {
	f.setDefaultCalls++
	f.setDefaultPMID = stripePMID
	return f.setDefaultErr
}

// ---------------------------------------------------------------------------
// fakeSummary — captures the CustomerPaymentSetup row so tests can verify
// that Service keeps the 1:1 denorm in sync.
// ---------------------------------------------------------------------------
type fakeSummary struct {
	mu    sync.Mutex
	setup domain.CustomerPaymentSetup
}

func (f *fakeSummary) UpsertPaymentSetup(_ context.Context, _ string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setup = ps
	return ps, nil
}
func (f *fakeSummary) GetPaymentSetup(_ context.Context, _, _ string) (domain.CustomerPaymentSetup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setup, nil
}

// fakeAudit — captures audit rows so tests can assert that
// SetDefault records stripe_sync_error on Stripe-side failures.
type fakeAudit struct {
	logs []fakeAuditRow
}

type fakeAuditRow struct {
	action       string
	resourceType string
	resourceID   string
	label        string
	meta         map[string]any
}

func (f *fakeAudit) Log(_ context.Context, _, action, resourceType, resourceID, label string, meta map[string]any) error {
	f.logs = append(f.logs, fakeAuditRow{action: action, resourceType: resourceType, resourceID: resourceID, label: label, meta: meta})
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestAttach_FirstBecomesDefault — the first PM attached to a customer
// is auto-promoted to default. Post-migration-0097 the summary table
// is gone; canonical state lives on payment_methods rows.
func TestAttach_FirstBecomesDefault(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	svc := NewService(store, stripe, &fakeSummary{})

	pm, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_stripe_1", false)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !pm.IsDefault {
		t.Fatalf("first PM should be default")
	}
	if pm.CardLast4 != "4242" {
		t.Fatalf("card details not set: %+v", pm)
	}
}

// TestAttach_SecondDoesNotStealDefault — a second card attached without
// makeDefault (a hosted-page payment, a SetupIntent Velox did not create)
// while one is already default keeps the existing default. Switching takes
// SetDefault, or a setup flow (makeDefault).
func TestAttach_SecondDoesNotStealDefault(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &fakeStripe{}, &fakeSummary{})

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	pm2, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_2", false)
	if err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	if pm2.IsDefault {
		t.Fatalf("second PM must not auto-default")
	}
}

// A card saved through a Velox setup flow (makeDefault) becomes the default
// beside a working one: exactly one default, the new card, and Stripe's copy
// is pointed at it. The audit log records the attach and the default change.
func TestAttach_MakeDefaultTakesTheDefaultAndSyncsStripe(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	audit := &fakeAudit{}
	svc := NewService(store, stripe, &fakeSummary{})
	svc.SetAuditLogger(audit)

	if _, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_old", false); err != nil {
		t.Fatalf("attach old: %v", err)
	}
	stripe.setDefaultCalls, stripe.setDefaultPMID = 0, ""

	pm, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_new", true)
	if err != nil {
		t.Fatalf("attach new: %v", err)
	}
	if !pm.IsDefault {
		t.Fatal("a card saved through a setup flow must become the default")
	}
	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	var defaults []string
	for _, p := range list {
		if p.IsDefault {
			defaults = append(defaults, p.StripePaymentMethodID)
		}
	}
	if len(defaults) != 1 || defaults[0] != "pm_new" {
		t.Fatalf("defaults = %v, want exactly [pm_new]", defaults)
	}
	if stripe.setDefaultCalls != 1 || stripe.setDefaultPMID != "pm_new" {
		t.Fatalf("Stripe default sync: calls=%d pm=%q, want once with pm_new", stripe.setDefaultCalls, stripe.setDefaultPMID)
	}
	// The attach writes the attached row, then a default_changed row: the
	// card Velox charges changed.
	if len(audit.logs) < 2 {
		t.Fatalf("audit rows = %v, want attached then default_changed", audit.logs)
	}
	attached, changed := audit.logs[len(audit.logs)-2], audit.logs[len(audit.logs)-1]
	if attached.meta["action"] != "attached" || attached.meta["is_default"] != true {
		t.Fatalf("attached row meta = %v, want action=attached is_default=true", attached.meta)
	}
	if changed.action != "update" || changed.meta["action"] != "default_changed" || changed.meta["trigger"] != "setup_link" {
		t.Fatalf("default_changed row = %+v, want update/default_changed with trigger setup_link", changed)
	}
	for _, row := range []fakeAuditRow{attached, changed} {
		if _, ok := row.meta["stripe_sync_error"]; ok {
			t.Fatalf("audit row must not carry stripe_sync_error on the happy path: %v", row.meta)
		}
	}
}

// The conflict path: a card already saved beside the default (C2's
// checkout.session.completed got there first) is attached again by its
// setup_intent.succeeded with makeDefault, and takes the default.
func TestAttach_MakeDefaultOnACardAlreadyBeside(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	svc := NewService(store, stripe, &fakeSummary{})

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_old", false)
	if beside, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_new", false); beside.IsDefault {
		t.Fatal("the card attached beside became the default")
	}
	stripe.setDefaultCalls = 0
	pm, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_new", true)
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if !pm.IsDefault {
		t.Fatal("re-attaching with makeDefault must make the card the default")
	}
	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	n := 0
	for _, p := range list {
		if p.IsDefault {
			n++
		}
	}
	if n != 1 || list[0].StripePaymentMethodID != "pm_new" {
		t.Fatalf("want exactly one default, pm_new; got %+v", list)
	}
	if stripe.setDefaultCalls != 1 || stripe.setDefaultPMID != "pm_new" {
		t.Fatalf("Stripe default sync: calls=%d pm=%q, want once with pm_new", stripe.setDefaultCalls, stripe.setDefaultPMID)
	}
}

// The Stripe sync after a default-taking attach is best-effort: a Stripe
// failure still returns the saved default card, and the audit row carries
// stripe_sync_error so the divergence is visible.
func TestAttach_MakeDefaultStripeFailureStillReturnsTheCard(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	audit := &fakeAudit{}
	svc := NewService(store, stripe, &fakeSummary{})
	svc.SetAuditLogger(audit)

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_old", false)
	stripe.setDefaultErr = errors.New("stripe 503")

	pm, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_new", true)
	if err != nil {
		t.Fatalf("attach must not fail on a Stripe sync error: %v", err)
	}
	if !pm.IsDefault || pm.StripePaymentMethodID != "pm_new" {
		t.Fatalf("attach returned %+v, want pm_new as the default", pm)
	}
	row := audit.logs[len(audit.logs)-1]
	if got, _ := row.meta["stripe_sync_error"].(string); got != "stripe 503" {
		t.Fatalf("audit stripe_sync_error = %q, want %q", got, "stripe 503")
	}
}

// A card added beside the working default makes no Stripe call: Stripe's
// copy already names the default, which did not change.
func TestAttach_BesideMakesNoStripeCall(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	audit := &fakeAudit{}
	svc := NewService(store, stripe, &fakeSummary{})
	svc.SetAuditLogger(audit)

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_old", false)
	stripe.setDefaultCalls = 0

	pm, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_beside", false)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if pm.IsDefault {
		t.Fatal("a card attached without makeDefault beside a working default must not take it")
	}
	if stripe.setDefaultCalls != 0 {
		t.Fatalf("Stripe SetDefaultPaymentMethod calls = %d, want 0", stripe.setDefaultCalls)
	}
	if row := audit.logs[len(audit.logs)-1]; row.meta["action"] != "attached" || row.meta["is_default"] != false {
		t.Fatalf("audit row meta = %v, want the attached row with is_default=false and no default_changed after it", row.meta)
	}
}

// The first card becomes the default by the store's rule, and its attach
// points Stripe's copy at it (Stripe's default follows Velox's).
func TestAttach_FirstCardSyncsStripeDefault(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	svc := NewService(store, stripe, &fakeSummary{})

	if _, err := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_first", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if stripe.setDefaultCalls != 1 || stripe.setDefaultPMID != "pm_first" {
		t.Fatalf("Stripe default sync: calls=%d pm=%q, want once with pm_first", stripe.setDefaultCalls, stripe.setDefaultPMID)
	}
}

// TestSetDefault_AtomicSwap — flipping default clears the old one.
// Canonical state lives entirely on payment_methods rows since
// migration 0097 retired customer_payment_setups.
func TestSetDefault_AtomicSwap(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &fakeStripe{}, &fakeSummary{})

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	pm2, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_2", false)

	promoted, err := svc.SetDefault(context.Background(), "tnt_x", "cus_y", pm2.ID)
	if err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if !promoted.IsDefault {
		t.Fatalf("promoted PM must be default")
	}

	// Confirm only one row has is_default = true across the list.
	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	defaults := 0
	for _, pm := range list {
		if pm.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("expected exactly one default, got %d", defaults)
	}
}

// TestSetDefault_PushesToStripe — promoting a PM pushes the change to
// Stripe's invoice_settings.default_payment_method so off-session
// auto-charges use the operator's chosen card. Pre-2026-05-29 this
// stayed local-only; Stripe quietly used whatever PM Stripe had as
// default. Stripe sync failure is best-effort: SetDefault still
// returns the promoted PM but the audit row records stripe_sync_error.
func TestSetDefault_PushesToStripe(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	audit := &fakeAudit{}
	svc := NewService(store, stripe, &fakeSummary{})
	svc.SetAuditLogger(audit)

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_first", false)
	pm2, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_second", false)
	stripe.setDefaultCalls = 0 // the first card's attach synced its own default

	if _, err := svc.SetDefault(context.Background(), "tnt_x", "cus_y", pm2.ID); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	if stripe.setDefaultCalls != 1 {
		t.Fatalf("expected 1 Stripe SetDefaultPaymentMethod call, got %d", stripe.setDefaultCalls)
	}
	if stripe.setDefaultPMID != pm2.StripePaymentMethodID {
		t.Fatalf("expected Stripe call with PM %q, got %q", pm2.StripePaymentMethodID, stripe.setDefaultPMID)
	}
	row := audit.logs[len(audit.logs)-1]
	if row.meta["action"] != "default_changed" {
		t.Fatalf("expected last audit row action=default_changed, got %v", row.meta["action"])
	}
	if _, ok := row.meta["stripe_sync_error"]; ok {
		t.Fatalf("audit row must not carry stripe_sync_error on happy path")
	}
}

// TestSetDefault_StripeFailureIsBestEffort — Stripe failure must NOT
// fail the operator action; the local default change still wins.
// Audit row carries stripe_sync_error so ops can reconcile.
func TestSetDefault_StripeFailureIsBestEffort(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{setDefaultErr: errors.New("stripe 503")}
	audit := &fakeAudit{}
	svc := NewService(store, stripe, &fakeSummary{})
	svc.SetAuditLogger(audit)

	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_first", false)
	pm2, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_second", false)

	promoted, err := svc.SetDefault(context.Background(), "tnt_x", "cus_y", pm2.ID)
	if err != nil {
		t.Fatalf("SetDefault must not fail on Stripe error: %v", err)
	}
	if !promoted.IsDefault {
		t.Fatalf("promoted PM must still be default locally")
	}
	row := audit.logs[len(audit.logs)-1]
	got, _ := row.meta["stripe_sync_error"].(string)
	if got != "stripe 503" {
		t.Fatalf("audit row must carry stripe_sync_error=%q, got %q", "stripe 503", got)
	}
}

// TestDetach_PromotesReplacement — detaching the default promotes the
// next-most-recent PM. List filters out detached rows.
func TestDetach_PromotesReplacement(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &fakeStripe{}, &fakeSummary{})

	pm1, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_2", false)

	if _, err := svc.Detach(context.Background(), "tnt_x", "cus_y", pm1.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	if len(list) != 1 || !list[0].IsDefault {
		t.Fatalf("expected 1 active PM flagged default, got %+v", list)
	}
}

// TestDetach_LastPM — detaching the only PM leaves the customer with
// zero active rows. billing.PaymentReadiness.ResolveForCharge will
// return hasDefaultPM=false on the next read; no ghost-card auto-
// charge is possible.
func TestDetach_LastPM(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &fakeStripe{}, &fakeSummary{})

	pm, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	if _, err := svc.Detach(context.Background(), "tnt_x", "cus_y", pm.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	if len(list) != 0 {
		t.Fatalf("expected zero active PMs, got %d", len(list))
	}
}

// TestDetach_StripeAlreadyGone — Stripe returning 404 on Detach (because
// the PM was already removed upstream) must still mark the local row
// detached; we don't leave orphans just because Stripe is ahead of us.
func TestDetach_StripeAlreadyGone(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{detachErr: errs.ErrNotFound}
	svc := NewService(store, stripe, &fakeSummary{})

	pm, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	if _, err := svc.Detach(context.Background(), "tnt_x", "cus_y", pm.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got, _ := store.Get(context.Background(), "tnt_x", pm.ID)
	if got.DetachedAt == nil {
		t.Fatalf("local row should be detached even when Stripe 404s")
	}
}

// TestDetach_CrossCustomer — detaching a PM that belongs to another
// customer within the same tenant must 404, not silently succeed.
func TestDetach_CrossCustomer(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &fakeStripe{}, &fakeSummary{})

	pm, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_a", "pm_1", false)
	if _, err := svc.Detach(context.Background(), "tnt_x", "cus_b", pm.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestDetach_PromotesNewestAndSyncsStripe — detaching the default promotes
// the NEWEST remaining active card (not just "a" card), leaves exactly one
// default, and best-effort syncs that promotion to Stripe's
// invoice_settings.default_payment_method (the cosmetic mirror).
func TestDetach_PromotesNewestAndSyncsStripe(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{}
	svc := NewService(store, stripe, &fakeSummary{})

	// pm_1 is default (first attached); pm_2 then pm_3 are newer.
	pm1, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_2", false)
	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_3", false)
	stripe.setDefaultCalls = 0 // the first card's attach synced its own default

	if _, err := svc.Detach(context.Background(), "tnt_x", "cus_y", pm1.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}

	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	defaults := 0
	var def PaymentMethod
	for _, p := range list {
		if p.IsDefault {
			defaults++
			def = p
		}
	}
	if defaults != 1 {
		t.Fatalf("expected exactly one default, got %d: %+v", defaults, list)
	}
	if def.StripePaymentMethodID != "pm_3" {
		t.Fatalf("expected newest card pm_3 promoted, got %q", def.StripePaymentMethodID)
	}
	// #1: the promotion synced to Stripe, with the promoted card's PM id.
	if stripe.setDefaultCalls != 1 || stripe.setDefaultPMID != "pm_3" {
		t.Fatalf("expected Stripe default synced to pm_3 once, got calls=%d pmid=%q",
			stripe.setDefaultCalls, stripe.setDefaultPMID)
	}
}

// TestDetach_PromotionStripeSyncFailureStillSucceeds — the Stripe sync of
// the auto-promoted default is best-effort (cosmetic per ADR-053). A Stripe
// failure must NOT fail the detach, and the local promotion stands.
func TestDetach_PromotionStripeSyncFailureStillSucceeds(t *testing.T) {
	store := newMemStore()
	stripe := &fakeStripe{setDefaultErr: errors.New("stripe 503")}
	svc := NewService(store, stripe, &fakeSummary{})

	pm1, _ := svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_1", false)
	_, _ = svc.AttachFromSetupIntent(context.Background(), "tnt_x", "cus_y", "pm_2", false)

	if _, err := svc.Detach(context.Background(), "tnt_x", "cus_y", pm1.ID); err != nil {
		t.Fatalf("detach must succeed despite Stripe sync failure: %v", err)
	}
	list, _ := svc.List(context.Background(), "tnt_x", "cus_y")
	if len(list) != 1 || !list[0].IsDefault || list[0].StripePaymentMethodID != "pm_2" {
		t.Fatalf("expected pm_2 promoted locally despite Stripe failure, got %+v", list)
	}
}
