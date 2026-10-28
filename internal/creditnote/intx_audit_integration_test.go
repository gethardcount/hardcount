package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// errInjectedEmit is the fault injected into the in-tx emission hooks — the
// shared-fate direction (ADR-090): if the audit row cannot be written, the
// money-document mutation it evidences must not commit.
var errInjectedEmit = errors.New("injected audit emission failure")

// seedRefundableCN creates a customer + paid invoice + issued credit note
// carrying stripeRefundID with refund_status = `status`. Column sets mirror
// issue_atomicity_integration_test.go; the refund id is stamped through the
// store's own UpdateRefundStatus (Create doesn't persist it).
func seedRefundableCN(t *testing.T, db *postgres.DB, tenantID, suffix, stripeRefundID string, status domain.RefundStatus) domain.CreditNote {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_refundaudit_" + suffix, DisplayName: "Refund Audit " + suffix,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-REFUNDAUDIT-" + suffix,
		Status:             domain.InvoicePaid,
		PaymentStatus:      domain.PaymentSucceeded,
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     0,
		AmountPaidCents:    10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	store := creditnote.NewPostgresStore(db)
	cn, err := store.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:         inv.ID,
		CustomerID:        cust.ID,
		CreditNoteNumber:  "CN-REFUNDAUDIT-" + suffix,
		Status:            domain.CreditNoteIssued,
		Reason:            "fraudulent",
		SubtotalCents:     5000,
		TotalCents:        5000,
		RefundAmountCents: 5000,
		Currency:          "USD",
		RefundStatus:      domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	// Stamp the Stripe refund id + starting refund status the webhook keys on.
	if err := store.UpdateRefundStatus(ctx, tenantID, cn.ID, status, stripeRefundID); err != nil {
		t.Fatalf("stamp refund id/status: %v", err)
	}
	got, err := store.Get(ctx, tenantID, cn.ID)
	if err != nil {
		t.Fatalf("re-read seeded credit note: %v", err)
	}
	return got
}

// seedDraftCN creates a customer + invoice + DRAFT credit note (the
// TransitionStatusAudited CAS subject).
func seedDraftCN(t *testing.T, db *postgres.DB, tenantID, suffix string) domain.CreditNote {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_casaudit_" + suffix, DisplayName: "CAS Audit " + suffix,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-CASAUDIT-" + suffix,
		Status:             domain.InvoiceVoided,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	cn, err := creditnote.NewPostgresStore(db).Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:         inv.ID,
		CustomerID:        cust.ID,
		CreditNoteNumber:  "CN-CASAUDIT-" + suffix,
		Status:            domain.CreditNoteDraft,
		Reason:            "order_change",
		SubtotalCents:     5000,
		TotalCents:        5000,
		CreditAmountCents: 5000,
		Currency:          "USD",
		RefundStatus:      domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	return cn
}

func cnAuditRows(ctx context.Context, t *testing.T, logger *audit.Logger, tenantID, cnID string) []domain.AuditEntry {
	t.Helper()
	rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: "credit_note", ResourceID: cnID,
	})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

// TestApplyRefundWebhookStatusAudited_SharedFate pins the ADR-090 in-tx
// emission hook on the async refund-webhook writer — a BACKGROUND path that
// shipped with no audit coverage at all. The store fires emit ONLY when the
// monotonic guard actually flipped a row, so a redelivery can never fabricate
// a "refund_status_changed" record for a transition that didn't happen (the
// `refund_status IS DISTINCT FROM $1` clause — a PR4 review finding), and an
// emission failure rolls the flip back.
func TestApplyRefundWebhookStatusAudited_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Refund Webhook Audit")

	store := creditnote.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	// emitter mirrors the Service's real emission (audit.Entry shape from
	// creditnote.Service.ApplyRefundWebhook) and counts its firings.
	emitter := func(calls *int, status domain.RefundStatus, refundID string) func(*sql.Tx, domain.CreditNote) error {
		return func(tx *sql.Tx, cn domain.CreditNote) error {
			*calls++
			return logger.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionUpdate,
				ResourceType:  "credit_note",
				ResourceID:    cn.ID,
				ResourceLabel: cn.CreditNoteNumber,
				Metadata: map[string]any{
					"action":           "refund_status_changed",
					"refund_status":    string(status),
					"stripe_refund_id": refundID,
					"invoice_id":       cn.InvoiceID,
					"customer_id":      cn.CustomerID,
				},
			})
		}
	}

	t.Run("pending to succeeded emits once and commits the flip", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "ok", "re_audit_ok", domain.RefundPending)

		calls := 0
		if err := store.ApplyRefundWebhookStatusAudited(ctx, tenantID, "re_audit_ok",
			domain.RefundSucceeded, emitter(&calls, domain.RefundSucceeded, "re_audit_ok")); err != nil {
			t.Fatalf("ApplyRefundWebhookStatusAudited: %v", err)
		}
		if calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1", calls)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.RefundStatus != domain.RefundSucceeded {
			t.Fatalf("refund_status: got %q, want succeeded", got.RefundStatus)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the flip; got %+v", rows)
		}
		if rows[0].Action != domain.AuditActionUpdate {
			t.Errorf("action: got %q, want %q", rows[0].Action, domain.AuditActionUpdate)
		}
		if rows[0].Metadata["action"] != "refund_status_changed" {
			t.Errorf("metadata action: got %v, want refund_status_changed", rows[0].Metadata["action"])
		}
		if rows[0].Metadata["refund_status"] != string(domain.RefundSucceeded) {
			t.Errorf("metadata refund_status: got %v, want succeeded", rows[0].Metadata["refund_status"])
		}
	})

	t.Run("emit failure rolls the flip back and writes no audit row", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "fail", "re_audit_fail", domain.RefundPending)

		calls := 0
		err := store.ApplyRefundWebhookStatusAudited(ctx, tenantID, "re_audit_fail", domain.RefundSucceeded,
			func(tx *sql.Tx, _ domain.CreditNote) error {
				calls++
				return errInjectedEmit
			})
		if !errors.Is(err, errInjectedEmit) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1 (the test is vacuous otherwise)", calls)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.RefundStatus != domain.RefundPending {
			t.Fatalf("refund_status: got %q, want pending — the flip must roll back with its failed audit emission", got.RefundStatus)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back flip: %+v", rows)
		}
	})

	t.Run("same-value pending redelivery emits nothing and succeeds", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "same", "re_audit_same", domain.RefundPending)

		calls := 0
		if err := store.ApplyRefundWebhookStatusAudited(ctx, tenantID, "re_audit_same",
			domain.RefundPending, emitter(&calls, domain.RefundPending, "re_audit_same")); err != nil {
			t.Fatalf("a same-value redelivery must be a successful no-op; got %v", err)
		}
		if calls != 0 {
			t.Fatalf("emit fired %d times on a pending→pending non-change — the log must not fabricate a 'refund_status_changed' row", calls)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row fabricated for a non-transition: %+v", rows)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.RefundStatus != domain.RefundPending {
			t.Errorf("refund_status: got %q, want pending", got.RefundStatus)
		}
	})

	t.Run("stale pending over succeeded emits nothing and succeeds", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "stale", "re_audit_stale", domain.RefundSucceeded)

		calls := 0
		if err := store.ApplyRefundWebhookStatusAudited(ctx, tenantID, "re_audit_stale",
			domain.RefundPending, emitter(&calls, domain.RefundPending, "re_audit_stale")); err != nil {
			t.Fatalf("a stale redelivery must be a successful no-op; got %v", err)
		}
		if calls != 0 {
			t.Fatalf("emit fired %d times on a monotonic-guard skip (succeeded→pending)", calls)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row emitted for a skipped stale write: %+v", rows)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.RefundStatus != domain.RefundSucceeded {
			t.Errorf("refund_status: got %q, want succeeded (the monotonic guard must hold)", got.RefundStatus)
		}
	})

	t.Run("unknown refund id is ErrNotFound and emits nothing", func(t *testing.T) {
		calls := 0
		err := store.ApplyRefundWebhookStatusAudited(ctx, tenantID, "re_audit_unknown",
			domain.RefundSucceeded, emitter(&calls, domain.RefundSucceeded, "re_audit_unknown"))
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("unknown stripe_refund_id must return ErrNotFound (caller decides ack vs retry); got %v", err)
		}
		if calls != 0 {
			t.Fatalf("emit fired %d times for an unknown refund id", calls)
		}
	})
}

// TestTransitionStatusAudited_SharedFate pins the CAS-scoped emission on the
// credit-note status flip (the orphan-draft void reached from the engine /
// reconciler — a background writer with no audit trail before ADR-090): the
// winner records exactly one row on the CAS tx, an emission failure rolls the
// flip back, and a LOST CAS mutates nothing and records nothing.
func TestTransitionStatusAudited_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Transition Audit")

	store := creditnote.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	voidEmitter := func(calls *int, cn domain.CreditNote) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			*calls++
			return logger.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionVoid,
				ResourceType:  "credit_note",
				ResourceID:    cn.ID,
				ResourceLabel: cn.CreditNoteNumber,
				Metadata: map[string]any{
					"action":      "orphan_draft_voided",
					"invoice_id":  cn.InvoiceID,
					"customer_id": cn.CustomerID,
				},
			})
		}
	}

	t.Run("won CAS emits once and commits the void", func(t *testing.T) {
		cn := seedDraftCN(t, db, tenantID, "won")

		calls := 0
		won, err := store.TransitionStatusAudited(ctx, tenantID, cn.ID,
			domain.CreditNoteDraft, domain.CreditNoteVoided, voidEmitter(&calls, cn))
		if err != nil {
			t.Fatalf("TransitionStatusAudited: %v", err)
		}
		if !won {
			t.Fatal("the draft→voided CAS must win on a draft credit note")
		}
		if calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1", calls)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.Status != domain.CreditNoteVoided {
			t.Fatalf("status: got %q, want voided", got.Status)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 || rows[0].Action != domain.AuditActionVoid {
			t.Fatalf("want exactly one 'void' audit row; got %+v", rows)
		}
		if rows[0].ResourceLabel != cn.CreditNoteNumber {
			t.Errorf("resource_label: got %q, want %q", rows[0].ResourceLabel, cn.CreditNoteNumber)
		}
	})

	t.Run("emit failure rolls the void back", func(t *testing.T) {
		cn := seedDraftCN(t, db, tenantID, "fail")

		calls := 0
		won, err := store.TransitionStatusAudited(ctx, tenantID, cn.ID,
			domain.CreditNoteDraft, domain.CreditNoteVoided, func(tx *sql.Tx) error {
				calls++
				return errInjectedEmit
			})
		if !errors.Is(err, errInjectedEmit) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if won {
			t.Error("a failed emission must not report a won transition")
		}
		if calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1 (the test is vacuous otherwise)", calls)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.Status != domain.CreditNoteDraft {
			t.Fatalf("status: got %q, want draft — the CAS must roll back with its failed audit emission", got.Status)
		}
		if got.VoidedAt != nil {
			t.Errorf("voided_at must be unset after rollback; got %v", got.VoidedAt)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back CAS: %+v", rows)
		}
	})

	t.Run("lost CAS emits nothing", func(t *testing.T) {
		cn := seedDraftCN(t, db, tenantID, "lost")

		// Somebody else already voided it — this call's from-status is stale.
		won, err := store.TransitionStatus(ctx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteVoided)
		if err != nil || !won {
			t.Fatalf("seed transition: won=%v err=%v", won, err)
		}

		calls := 0
		won, err = store.TransitionStatusAudited(ctx, tenantID, cn.ID,
			domain.CreditNoteDraft, domain.CreditNoteVoided, voidEmitter(&calls, cn))
		if err != nil {
			t.Fatalf("a lost CAS must not error; got %v", err)
		}
		if won {
			t.Fatal("the second draft→voided CAS must lose")
		}
		if calls != 0 {
			t.Fatalf("emit fired %d times on a lost CAS — a transition that didn't happen must record nothing", calls)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row emitted for a lost CAS: %+v", rows)
		}
	})
}
