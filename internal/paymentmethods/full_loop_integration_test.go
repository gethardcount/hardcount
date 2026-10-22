package paymentmethods_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestAttach_DedupesByFingerprint exercises the migration-0099
// dedupe-by-fingerprint behavior. Stripe mints a fresh PaymentMethod
// every time a customer completes Checkout, even for the same card
// number — without dedupe the portal renders Visa-4242 twice. The
// store collapses these on attach using card.fingerprint as the
// dedupe key.
//
// Scenario:
//  1. Attach pm_first (fingerprint=fp_A) → becomes default.
//  2. Re-attach pm_second with SAME fingerprint fp_A → old row is
//     detached, new row inherits is_default=true. List returns 1.
//  3. Attach pm_third with DIFFERENT fingerprint fp_B → 2 rows, the
//     existing one stays default, the new one is non-default.
func TestAttach_DedupesByFingerprint(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 20*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "dedupe-test")
	custStore := customer.NewPostgresStore(db)
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_dedupe", DisplayName: "Dedupe Co", Email: "x@dedupe.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	pmStore := paymentmethods.NewPostgresStore(db)
	stripe := &dedupeFakeStripe{}
	svc := paymentmethods.NewService(pmStore, stripe, custStore)
	if err := custStore.SetStripeCustomerID(ctx, tenantID, cust.ID, "cus_stripe_dedupe"); err != nil {
		t.Fatalf("seed stripe customer: %v", err)
	}

	// 1. First attach — pm_first, fingerprint fp_A.
	stripe.next = paymentmethods.CardMetadata{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030, Fingerprint: "fp_A"}
	pm1, err := svc.AttachFromSetupIntent(ctx, tenantID, cust.ID, "pm_first", false)
	if err != nil {
		t.Fatalf("attach first: %v", err)
	}
	if !pm1.IsDefault {
		t.Fatalf("first PM must be default")
	}

	// 2. Re-attach same card under a NEW Stripe PM id but SAME
	// fingerprint — Stripe-realistic for "customer clicked Add again
	// with the same card". Should dedupe: pm_first detached, pm_second
	// inherits is_default=true. List size stays at 1.
	stripe.next = paymentmethods.CardMetadata{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030, Fingerprint: "fp_A"}
	pm2, err := svc.AttachFromSetupIntent(ctx, tenantID, cust.ID, "pm_second", false)
	if err != nil {
		t.Fatalf("attach second (dedupe): %v", err)
	}
	if !pm2.IsDefault {
		t.Fatalf("re-attached PM must inherit is_default=true from the detached duplicate; got false")
	}
	pms, err := svc.List(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("list after dedupe: %v", err)
	}
	if len(pms) != 1 {
		t.Fatalf("dedupe should leave exactly 1 active PM; got %d", len(pms))
	}
	if pms[0].StripePaymentMethodID != "pm_second" {
		t.Fatalf("active PM should be pm_second; got %q", pms[0].StripePaymentMethodID)
	}

	// 3. Attach a genuinely different card — different fingerprint.
	// No dedupe should fire. Existing pm_second stays default;
	// pm_third is the second non-default row.
	stripe.next = paymentmethods.CardMetadata{Brand: "mastercard", Last4: "5555", ExpMonth: 6, ExpYear: 2031, Fingerprint: "fp_B"}
	pm3, err := svc.AttachFromSetupIntent(ctx, tenantID, cust.ID, "pm_third", false)
	if err != nil {
		t.Fatalf("attach third (different card): %v", err)
	}
	if pm3.IsDefault {
		t.Fatalf("third PM (different card) must NOT steal default; existing default wins")
	}
	pms, err = svc.List(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("list after third attach: %v", err)
	}
	if len(pms) != 2 {
		t.Fatalf("different-card attach should add a row; got %d active", len(pms))
	}
}

// dedupeFakeStripe lets the test inject different CardMetadata per
// AttachFromSetupIntent call by mutating .next between calls.
type dedupeFakeStripe struct {
	next paymentmethods.CardMetadata
}

func (f *dedupeFakeStripe) EnsureStripeCustomer(_ context.Context, _, _ string) (string, error) {
	return "cus_stripe_dedupe", nil
}
func (f *dedupeFakeStripe) CreateSetupIntent(_ context.Context, _ string, _ map[string]string) (string, string, error) {
	return "seti_secret_dedupe", "seti_dedupe", nil
}
func (f *dedupeFakeStripe) CreateSetupCheckoutSession(_ context.Context, _, _, _ string, _ map[string]string) (string, string, error) {
	return "https://checkout.stripe.com/dedupe", "cs_dedupe", nil
}
func (f *dedupeFakeStripe) DetachPaymentMethod(_ context.Context, _ string) error { return nil }
func (f *dedupeFakeStripe) FetchPaymentMethodCard(_ context.Context, _ string) (paymentmethods.CardMetadata, error) {
	return f.next, nil
}
func (f *dedupeFakeStripe) SetDefaultPaymentMethod(_ context.Context, _, _, _ string) error {
	return nil
}

// TestDetachAndRebalance_AtomicPromote verifies the real-Postgres path:
// detaching the default promotes the newest remaining active card and
// exactly one default survives — all in one transaction (no committed
// "active cards but no default" window). Exercises the FOR UPDATE +
// promote-subquery + partial-unique-index interaction the in-memory store
// can't model.
func TestDetachAndRebalance_AtomicPromote(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 20*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "detach-rebalance-test")
	custStore := customer.NewPostgresStore(db)
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_detach", DisplayName: "Detach Co", Email: "x@detach.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if err := custStore.SetStripeCustomerID(ctx, tenantID, cust.ID, "cus_stripe_detach"); err != nil {
		t.Fatalf("seed stripe customer: %v", err)
	}

	pmStore := paymentmethods.NewPostgresStore(db)
	stripe := &dedupeFakeStripe{}
	svc := paymentmethods.NewService(pmStore, stripe, custStore)

	// pm_old (fp_A) attaches first → becomes default. pm_new (fp_B, distinct
	// fingerprint so it isn't deduped) attaches second → newer created_at.
	stripe.next = paymentmethods.CardMetadata{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030, Fingerprint: "fp_A"}
	pmOld, err := svc.AttachFromSetupIntent(ctx, tenantID, cust.ID, "pm_old", false)
	if err != nil {
		t.Fatalf("attach old: %v", err)
	}
	stripe.next = paymentmethods.CardMetadata{Brand: "mastercard", Last4: "5555", ExpMonth: 6, ExpYear: 2031, Fingerprint: "fp_B"}
	if _, err := svc.AttachFromSetupIntent(ctx, tenantID, cust.ID, "pm_new", false); err != nil {
		t.Fatalf("attach new: %v", err)
	}

	// Detach the default (pm_old) → the newest active card (pm_new) must be
	// promoted in the same transaction.
	if _, err := svc.Detach(ctx, tenantID, cust.ID, pmOld.ID); err != nil {
		t.Fatalf("detach: %v", err)
	}

	list, err := pmStore.List(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 active PM after detach, got %d", len(list))
	}
	if !list[0].IsDefault || list[0].StripePaymentMethodID != "pm_new" {
		t.Fatalf("expected pm_new promoted to default, got %+v", list[0])
	}
}

// setupDefaultRig drives the store directly (real Postgres): the rule a
// card saved through a setup flow takes the default (makeDefault) lives in
// PostgresStore.Upsert's transaction.
type setupDefaultRig struct {
	t        *testing.T
	ctx      context.Context
	tenantID string
	custs    *customer.PostgresStore
	store    *paymentmethods.PostgresStore
}

func newSetupDefaultRig(t *testing.T, name string) *setupDefaultRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	t.Cleanup(cancel)
	return &setupDefaultRig{
		t: t, ctx: ctx, tenantID: testutil.CreateTestTenant(t, db, name),
		custs: customer.NewPostgresStore(db), store: paymentmethods.NewPostgresStore(db),
	}
}

func (r *setupDefaultRig) customer(key string) string {
	r.t.Helper()
	c, err := r.custs.Create(r.ctx, r.tenantID, domain.Customer{ExternalID: "ext_" + key, DisplayName: key})
	if err != nil {
		r.t.Fatalf("create customer: %v", err)
	}
	return c.ID
}

func (r *setupDefaultRig) upsert(custID, pm, fingerprint string, makeDefault bool) paymentmethods.PaymentMethod {
	r.t.Helper()
	got, _, err := r.store.Upsert(r.ctx, r.tenantID, paymentmethods.PaymentMethod{
		CustomerID: custID, StripePaymentMethodID: pm, Type: "card",
		CardBrand: "visa", CardLast4: "4242", CardExpMonth: 12, CardExpYear: 2034, CardFingerprint: fingerprint,
	}, makeDefault)
	if err != nil {
		r.t.Fatalf("upsert %s (makeDefault=%v): %v", pm, makeDefault, err)
	}
	return got
}

// defaults lists the customer's active default cards, read from the table.
func (r *setupDefaultRig) defaults(custID string) []string {
	r.t.Helper()
	pms, err := r.store.List(r.ctx, r.tenantID, custID)
	if err != nil {
		r.t.Fatalf("list: %v", err)
	}
	var out []string
	for _, pm := range pms {
		if pm.IsDefault && pm.DetachedAt == nil {
			out = append(out, pm.StripePaymentMethodID)
		}
	}
	return out
}

func (r *setupDefaultRig) assertOnlyDefault(custID, want string) {
	r.t.Helper()
	if got := r.defaults(custID); len(got) != 1 || got[0] != want {
		r.t.Fatalf("default cards = %v, want exactly [%s]", got, want)
	}
}

// A card saved through a setup flow (makeDefault) takes the default from a
// working one, on the store's insert path and its conflict path; without
// makeDefault a card beside a working default stays beside (control).
func TestUpsert_MakeDefault_RealStore(t *testing.T) {
	r := newSetupDefaultRig(t, "Setup Card Default")

	t.Run("insert path: a new card takes the default", func(t *testing.T) {
		c := r.customer("ins")
		r.upsert(c, "pm_ins_a", "fp_ins_a", false)
		if b := r.upsert(c, "pm_ins_b", "fp_ins_b", true); !b.IsDefault {
			t.Fatal("the returned row is not the default")
		}
		r.assertOnlyDefault(c, "pm_ins_b")
	})

	t.Run("conflict path: a card already saved beside takes the default", func(t *testing.T) {
		c := r.customer("conf")
		r.upsert(c, "pm_conf_a", "fp_conf_a", false)
		if b := r.upsert(c, "pm_conf_b", "fp_conf_b", false); b.IsDefault {
			t.Fatal("the card saved beside became the default")
		}
		if b := r.upsert(c, "pm_conf_b", "fp_conf_b", true); !b.IsDefault {
			t.Fatal("the returned row is not the default")
		}
		r.assertOnlyDefault(c, "pm_conf_b")
	})

	t.Run("control: without makeDefault a working default stays", func(t *testing.T) {
		c := r.customer("ctl")
		r.upsert(c, "pm_ctl_a", "fp_ctl_a", false)
		if b := r.upsert(c, "pm_ctl_b", "fp_ctl_b", false); b.IsDefault {
			t.Fatal("a card beside a working default took it without makeDefault")
		}
		r.assertOnlyDefault(c, "pm_ctl_a")
	})

	t.Run("fingerprint dedupe of a card beside, with makeDefault", func(t *testing.T) {
		c := r.customer("dd1")
		r.upsert(c, "pm_dd1_a", "fp_dd1_a", false)
		r.upsert(c, "pm_dd1_b", "fp_dd1_b", false)
		// The same physical card as b, saved again through a setup flow.
		r.upsert(c, "pm_dd1_c", "fp_dd1_b", true)
		r.assertOnlyDefault(c, "pm_dd1_c")
		if pms, _ := r.store.List(r.ctx, r.tenantID, c); len(pms) != 2 {
			t.Fatalf("active cards = %d, want 2 (a, and c replacing its duplicate b)", len(pms))
		}
	})

	t.Run("fingerprint dedupe of the default, with makeDefault", func(t *testing.T) {
		c := r.customer("dd2")
		r.upsert(c, "pm_dd2_a", "fp_dd2_a", false)
		r.upsert(c, "pm_dd2_b", "fp_dd2_b", false)
		// The same physical card as the default a, saved again.
		r.upsert(c, "pm_dd2_c", "fp_dd2_a", true)
		r.assertOnlyDefault(c, "pm_dd2_c")
	})

	// One setup session sends checkout.session.completed (C2, makeDefault
	// false) and setup_intent.succeeded (C1, true) for the same card, in
	// either order. The setup flow's card is the default both ways.
	t.Run("order: beside first, then makeDefault", func(t *testing.T) {
		c := r.customer("ord1")
		r.upsert(c, "pm_ord1_a", "fp_ord1_a", false)
		r.upsert(c, "pm_ord1_b", "fp_ord1_b", false)
		r.upsert(c, "pm_ord1_b", "fp_ord1_b", true)
		r.assertOnlyDefault(c, "pm_ord1_b")
	})
	t.Run("order: makeDefault first, then beside", func(t *testing.T) {
		c := r.customer("ord2")
		r.upsert(c, "pm_ord2_a", "fp_ord2_a", false)
		r.upsert(c, "pm_ord2_b", "fp_ord2_b", true)
		if b := r.upsert(c, "pm_ord2_b", "fp_ord2_b", false); !b.IsDefault {
			t.Fatal("a later attach without makeDefault took the default back off the card")
		}
		r.assertOnlyDefault(c, "pm_ord2_b")
	})
}

// The two webhooks of one setup session, concurrently: C2's attach (beside)
// and C1's (makeDefault) of the same card race on the same customer. Every
// run must end with exactly one default, the setup flow's card, and neither
// attach may fail on the one-default-per-customer index.
func TestUpsert_MakeDefault_ConcurrentWithBeside_RealStore(t *testing.T) {
	r := newSetupDefaultRig(t, "Setup Card Default Race")
	const runs = 50
	for i := 0; i < runs; i++ {
		c := r.customer(fmt.Sprintf("race%d", i))
		r.upsert(c, fmt.Sprintf("pm_race%d_a", i), fmt.Sprintf("fp_race%d_a", i), false)
		pmB, fpB := fmt.Sprintf("pm_race%d_b", i), fmt.Sprintf("fp_race%d_b", i)

		start := make(chan struct{})
		errsCh := make(chan error, 2)
		var wg sync.WaitGroup
		for _, makeDefault := range []bool{false, true} {
			wg.Add(1)
			go func(makeDefault bool) {
				defer wg.Done()
				<-start
				_, _, err := r.store.Upsert(r.ctx, r.tenantID, paymentmethods.PaymentMethod{
					CustomerID: c, StripePaymentMethodID: pmB, Type: "card",
					CardBrand: "visa", CardLast4: "4242", CardExpMonth: 12, CardExpYear: 2034, CardFingerprint: fpB,
				}, makeDefault)
				if err != nil {
					errsCh <- fmt.Errorf("upsert makeDefault=%v: %w", makeDefault, err)
				}
			}(makeDefault)
		}
		close(start)
		wg.Wait()
		close(errsCh)
		for err := range errsCh {
			t.Fatalf("run %d: %v", i, err)
		}
		if got := r.defaults(c); len(got) != 1 || got[0] != pmB {
			t.Fatalf("run %d: default cards = %v, want exactly [%s]", i, got, pmB)
		}
	}
}

// in reports through the subtest's t, so a failure names its subtest.
func (r *setupDefaultRig) in(t *testing.T) *setupDefaultRig {
	c := *r
	c.t = t
	return &c
}

// detach removes a card the way the operator's Remove does (store half:
// detach, and promote the newest remaining card when it was the default).
func (r *setupDefaultRig) detach(custID, rowID string) {
	r.t.Helper()
	if _, _, err := r.store.DetachAndRebalance(r.ctx, r.tenantID, custID, rowID); err != nil {
		r.t.Fatalf("detach %s: %v", rowID, err)
	}
}

func (r *setupDefaultRig) assertStillRemoved(rowID string) {
	r.t.Helper()
	got, err := r.store.Get(r.ctx, r.tenantID, rowID)
	if err != nil {
		r.t.Fatalf("get %s: %v", rowID, err)
	}
	if got.DetachedAt == nil || got.IsDefault {
		r.t.Fatalf("removed card %s came back: detached_at=%v is_default=%v", got.StripePaymentMethodID, got.DetachedAt, got.IsDefault)
	}
}

// A webhook for a card that was removed after it was saved (Stripe
// redelivers setup_intent.succeeded hours after a failed first delivery)
// leaves it removed. Stripe does not let a detached PaymentMethod be used
// or attached again, so bringing the row back, let alone as the default,
// would point every retry at a card Stripe refuses while the working card
// sits unused. Velox's own dedupe-by-fingerprint replaces an older copy of
// the same card; a late webhook for that copy leaves the newer one in place.
func TestUpsert_LateAttachOfARemovedCard_RealStore(t *testing.T) {
	r := newSetupDefaultRig(t, "Late Attach Removed Card")

	t.Run("a card removed beside the default, then a late setup attach", func(t *testing.T) {
		r := r.in(t)
		c := r.customer("rm1")
		r.upsert(c, "pm_rm1_a", "fp_rm1_a", false)
		b := r.upsert(c, "pm_rm1_b", "fp_rm1_b", false) // checkout.session.completed got there first
		r.detach(c, b.ID)
		got := r.upsert(c, "pm_rm1_b", "fp_rm1_b", true) // setup_intent.succeeded, redelivered
		if got.DetachedAt == nil || got.IsDefault {
			t.Fatalf("returned row = detached_at %v is_default %v, want the removed row unchanged", got.DetachedAt, got.IsDefault)
		}
		r.assertStillRemoved(b.ID)
		r.assertOnlyDefault(c, "pm_rm1_a")
	})

	t.Run("a setup card removed as the default, then a late attach", func(t *testing.T) {
		r := r.in(t)
		c := r.customer("rm2")
		r.upsert(c, "pm_rm2_a", "fp_rm2_a", false)
		b := r.upsert(c, "pm_rm2_b", "fp_rm2_b", true)
		r.detach(c, b.ID) // a is promoted back
		r.assertOnlyDefault(c, "pm_rm2_a")
		r.upsert(c, "pm_rm2_b", "fp_rm2_b", true)
		r.upsert(c, "pm_rm2_b", "fp_rm2_b", false)
		r.assertStillRemoved(b.ID)
		r.assertOnlyDefault(c, "pm_rm2_a")
	})

	t.Run("control: a card never removed still takes the default", func(t *testing.T) {
		r := r.in(t)
		c := r.customer("rm3")
		r.upsert(c, "pm_rm3_a", "fp_rm3_a", false)
		r.upsert(c, "pm_rm3_b", "fp_rm3_b", false)
		r.upsert(c, "pm_rm3_b", "fp_rm3_b", true)
		r.assertOnlyDefault(c, "pm_rm3_b")
	})

	t.Run("an older copy of the same card replaced by dedupe, then a late attach", func(t *testing.T) {
		r := r.in(t)
		c := r.customer("rm4")
		old := r.upsert(c, "pm_rm4_old", "fp_rm4", false)
		r.upsert(c, "pm_rm4_new", "fp_rm4", false) // same physical card, saved again
		r.assertOnlyDefault(c, "pm_rm4_new")
		r.upsert(c, "pm_rm4_old", "fp_rm4", false)
		r.upsert(c, "pm_rm4_old", "fp_rm4", true)
		r.assertStillRemoved(old.ID)
		r.assertOnlyDefault(c, "pm_rm4_new")
		if pms, _ := r.store.List(r.ctx, r.tenantID, c); len(pms) != 1 {
			t.Fatalf("active cards = %d, want 1 (the newer copy)", len(pms))
		}
	})
}

// recordingAudit keeps the audit rows the service writes.
type recordingAudit struct {
	mu   sync.Mutex
	rows []map[string]any
}

func (a *recordingAudit) Log(_ context.Context, _, action, resourceType, _, label string, meta map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	row := map[string]any{"_action": action, "_resource": resourceType, "_label": label}
	for k, v := range meta {
		row[k] = v
	}
	a.rows = append(a.rows, row)
	return nil
}

func (a *recordingAudit) take() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.rows
	a.rows = nil
	return out
}

func defaultChangedRows(rows []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["action"] == "default_changed" {
			out = append(out, r)
		}
	}
	return out
}

// When an attach moves the default to the saved card, the audit log says
// so, as Set as default and Remove's auto-promote do: a default_changed row
// beside the attached row. The attached row records whether the card is
// the default after the attach. A card added beside, the first card, and a
// redelivery of a card already the default write no default_changed row.
func TestAttach_AuditsADefaultChange_RealStore(t *testing.T) {
	r := newSetupDefaultRig(t, "Attach Audits Default Change")
	stripe := &dedupeFakeStripe{}
	svc := paymentmethods.NewService(r.store, stripe, r.custs)
	audit := &recordingAudit{}
	svc.SetAuditLogger(audit)

	attach := func(custID, pm, fp string, makeDefault bool) []map[string]any {
		t.Helper()
		stripe.next = paymentmethods.CardMetadata{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2034, Fingerprint: fp}
		if _, err := svc.AttachFromSetupIntent(r.ctx, r.tenantID, custID, pm, makeDefault); err != nil {
			t.Fatalf("attach %s: %v", pm, err)
		}
		return audit.take()
	}
	attachedRow := func(t *testing.T, rows []map[string]any) map[string]any {
		t.Helper()
		for _, row := range rows {
			if row["action"] == "attached" {
				return row
			}
		}
		t.Fatalf("no attached audit row in %v", rows)
		return nil
	}

	c := r.customer("aud")

	rows := attach(c, "pm_aud_a", "fp_aud_a", false)
	if got := attachedRow(t, rows)["is_default"]; got != true {
		t.Fatalf("first card: attached row is_default = %v, want true", got)
	}
	if dc := defaultChangedRows(rows); len(dc) != 0 {
		t.Fatalf("first card: default_changed rows = %v, want none (no card was replaced)", dc)
	}

	rows = attach(c, "pm_aud_b", "fp_aud_b", false)
	if got := attachedRow(t, rows)["is_default"]; got != false {
		t.Fatalf("card beside: attached row is_default = %v, want false", got)
	}
	if dc := defaultChangedRows(rows); len(dc) != 0 {
		t.Fatalf("card beside: default_changed rows = %v, want none", dc)
	}

	rows = attach(c, "pm_aud_b", "fp_aud_b", true) // the setup flow's own webhook
	if got := attachedRow(t, rows)["is_default"]; got != true {
		t.Fatalf("setup card: attached row is_default = %v, want true", got)
	}
	dc := defaultChangedRows(rows)
	if len(dc) != 1 || dc[0]["_action"] != "update" || dc[0]["_resource"] != "payment_method" ||
		dc[0]["card_last4"] != "4242" || dc[0]["trigger"] != "setup_link" {
		t.Fatalf("setup card: default_changed rows = %v, want one update/payment_method row with trigger setup_link", dc)
	}

	rows = attach(c, "pm_aud_b", "fp_aud_b", true) // redelivery, already the default
	if dc := defaultChangedRows(rows); len(dc) != 0 {
		t.Fatalf("redelivery: default_changed rows = %v, want none (the default did not move)", dc)
	}
}
