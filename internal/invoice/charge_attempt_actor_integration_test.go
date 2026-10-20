package invoice_test

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Who made a charge attempt (ADR-128 §4, PR 6). The charge call's write
// (RecordChargeAttempt) records the request identity the way the audit log
// does (audit.ResolveActor): an operator's Charge now names them, the
// engine's own charges and dunning retries name the system. A settle report
// (upsertChargeAttemptTx: the webhook, the reconciler, the inline settle)
// does not know who paid, so a row it inserts records no actor, and it never
// changes one the charge call recorded.

type attemptActorRig struct {
	t        *testing.T
	ctx      context.Context
	tenantID string
	store    *invoice.PostgresStore
	admin    *sql.DB
	custID   string
	n        int
}

func newAttemptActorRig(t *testing.T) *attemptActorRig {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Attempt Actor")
	c, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "ext_actor", DisplayName: "Actor"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &attemptActorRig{t: t, ctx: ctx, tenantID: tenantID, store: invoice.NewPostgresStore(db), admin: testutil.AdminPool(t), custID: c.ID}
}

func (r *attemptActorRig) invoice() string {
	r.t.Helper()
	r.n++
	inv, err := r.store.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: r.custID, InvoiceNumber: "VLX-ACTOR-" + string(rune('A'+r.n)), Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
	})
	if err != nil {
		r.t.Fatalf("create invoice: %v", err)
	}
	return inv.ID
}

func (r *attemptActorRig) record(ctx context.Context, invID, pi string, trigger domain.ChargeAttemptTrigger, outcome domain.ChargeAttemptOutcome) {
	r.t.Helper()
	if err := r.store.RecordChargeAttempt(ctx, r.tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: invID, StripePaymentIntentID: pi, Trigger: trigger, Outcome: outcome,
		AmountCents: 5000, OccurredAt: time.Now().UTC(),
	}); err != nil {
		r.t.Fatalf("record attempt %q: %v", pi, err)
	}
}

// actor reads the attempt rows' actor columns, newest last.
func (r *attemptActorRig) actors(invID string) [][2]string {
	r.t.Helper()
	rows, err := r.admin.QueryContext(context.Background(),
		`SELECT COALESCE(actor_type, '<NULL>'), COALESCE(actor_id, '<NULL>') FROM invoice_charge_attempts
		 WHERE invoice_id = $1 ORDER BY created_at, id`, invID)
	if err != nil {
		r.t.Fatalf("read actors: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var a [2]string
		if err := rows.Scan(&a[0], &a[1]); err != nil {
			r.t.Fatalf("scan: %v", err)
		}
		out = append(out, a)
	}
	return out
}

func (r *attemptActorRig) wantActors(invID string, want ...[2]string) {
	r.t.Helper()
	got := r.actors(invID)
	if len(got) != len(want) {
		r.t.Fatalf("attempt rows: %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			r.t.Errorf("attempt row %d actor = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestRecordChargeAttempt_StampsActor covers both insert shapes of the charge
// call's write: an attempt with a PaymentIntent (upsert by PI) and one whose
// PaymentIntent create failed (insert only).
func TestRecordChargeAttempt_StampsActor(t *testing.T) {
	r := newAttemptActorRig(t)
	cases := []struct {
		name    string
		ctx     context.Context
		trigger domain.ChargeAttemptTrigger
		want    [2]string
	}{
		{"operator Charge now", auth.WithUserID(r.ctx, "vlx_usr_op"), domain.ChargeTriggerChargeNow, [2]string{"user", "vlx_usr_op"}},
		{"API collect", auth.WithKeyID(r.ctx, "vlx_key_op"), domain.ChargeTriggerChargeNow, [2]string{"api_key", "vlx_key_op"}},
		{"engine auto-charge", r.ctx, domain.ChargeTriggerAutoCharge, [2]string{"system", "system"}},
		{"dunning retry", r.ctx, domain.ChargeTriggerDunningRetry, [2]string{"system", "system"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r.t = t
			inv := r.invoice()
			r.record(tc.ctx, inv, "pi_"+inv, tc.trigger, domain.ChargeAttemptPending)
			r.record(tc.ctx, inv, "", tc.trigger, domain.ChargeAttemptUnknown)
			r.wantActors(inv, tc.want, tc.want)
		})
	}
}

// TestChargeAttemptActor_SettleReports: a settle report inserts with no
// actor, never overwrites the charge call's actor, and the charge call's
// write fills the actor in when its report beat it (the row the report
// inserted as 'external').
func TestChargeAttemptActor_SettleReports(t *testing.T) {
	r := newAttemptActorRig(t)
	operator := auth.WithUserID(r.ctx, "vlx_usr_op")

	t.Run("a hosted-page payment records no actor", func(t *testing.T) {
		r.t = t
		inv := r.invoice()
		// A report reaching the store on a request that carries an identity
		// still does not name who paid.
		if _, _, err := r.store.MarkPaymentFailedReportingTransition(operator, r.tenantID, inv, domain.PaymentFailureReport{
			PaymentIntentID: "pi_hosted", Message: "Your card was declined.", External: true,
		}, nil); err != nil {
			t.Fatalf("report: %v", err)
		}
		r.wantActors(inv, [2]string{"<NULL>", "<NULL>"})
	})

	t.Run("a later report keeps the charge call's actor", func(t *testing.T) {
		r.t = t
		inv := r.invoice()
		r.record(operator, inv, "pi_mine", domain.ChargeTriggerChargeNow, domain.ChargeAttemptPending)
		// The webhook arrives on a background context.
		if _, _, err := r.store.MarkPaymentFailedReportingTransition(r.ctx, r.tenantID, inv, domain.PaymentFailureReport{
			PaymentIntentID: "pi_mine", Message: "Your card was declined.",
		}, nil); err != nil {
			t.Fatalf("report: %v", err)
		}
		r.wantActors(inv, [2]string{"user", "vlx_usr_op"})
	})

	t.Run("the charge call fills in the actor when its report came first", func(t *testing.T) {
		r.t = t
		inv := r.invoice()
		if _, _, err := r.store.MarkPaymentFailedReportingTransition(r.ctx, r.tenantID, inv, domain.PaymentFailureReport{
			PaymentIntentID: "pi_raced", Message: "Your card was declined.",
		}, nil); err != nil {
			t.Fatalf("report: %v", err)
		}
		r.record(operator, inv, "pi_raced", domain.ChargeTriggerChargeNow, domain.ChargeAttemptFailed)
		r.wantActors(inv, [2]string{"user", "vlx_usr_op"})
	})
}

// TestChargeNowAttempt_CountsAsVeloxsOwn: splitting Charge now out of
// auto_charge must not change how a late report is judged. A Charge now
// attempt is one Velox made, so a webhook for an older attempt is superseded
// by it, exactly as by an automatic one (veloxAttemptStandingTx).
func TestChargeNowAttempt_CountsAsVeloxsOwn(t *testing.T) {
	r := newAttemptActorRig(t)
	operator := auth.WithUserID(r.ctx, "vlx_usr_op")
	for _, trigger := range []domain.ChargeAttemptTrigger{domain.ChargeTriggerAutoCharge, domain.ChargeTriggerChargeNow} {
		t.Run(string(trigger), func(t *testing.T) {
			r.t = t
			inv := r.invoice()
			// A PaymentIntent id is unique per tenant, so each subtest's are
			// its own.
			oldPI, newPI := "pi_old_"+string(trigger), "pi_new_"+string(trigger)
			r.record(operator, inv, oldPI, trigger, domain.ChargeAttemptPending)
			r.record(operator, inv, newPI, trigger, domain.ChargeAttemptPending)
			_, res, err := r.store.MarkPaymentFailedReportingTransition(r.ctx, r.tenantID, inv, domain.PaymentFailureReport{
				PaymentIntentID: oldPI, Message: "Your card was declined.",
			}, nil)
			if err != nil {
				t.Fatalf("report: %v", err)
			}
			if res.Recorded {
				t.Errorf("a late decline for %s moved the invoice; the newer %s attempt %s supersedes it", oldPI, trigger, newPI)
			}
		})
	}
}

// TestChargeAttemptTriggers_MatchCheckConstraint keeps
// domain.ChargeAttemptTriggers and invoice_charge_attempts' CHECK equal (the
// enum and CHECK sync rule): a trigger the CHECK does not list fails the
// attempt's insert, and the charge call's write is best effort, so the
// attempt would be lost without a word.
func TestChargeAttemptTriggers_MatchCheckConstraint(t *testing.T) {
	testutil.SetupTestDB(t)
	var def string
	if err := testutil.AdminPool(t).QueryRowContext(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'invoice_charge_attempts_trigger_source_check'`,
	).Scan(&def); err != nil {
		t.Fatalf("read the CHECK: %v", err)
	}
	inCheck := map[string]bool{}
	for _, m := range regexp.MustCompile(`'(\w+)'`).FindAllStringSubmatch(def, -1) {
		inCheck[m[1]] = true
	}
	for _, tr := range domain.ChargeAttemptTriggers() {
		if !inCheck[string(tr)] {
			t.Errorf("trigger %q is not in the CHECK (%s)", tr, def)
		}
		delete(inCheck, string(tr))
	}
	for tr := range inCheck {
		t.Errorf("the CHECK allows %q, which no Go constant names", tr)
	}
}
