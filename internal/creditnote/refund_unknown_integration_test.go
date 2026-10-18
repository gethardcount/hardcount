package creditnote_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// refundStatusUnknown is the refund status for "Stripe may or may not have
// made the refund" (#965).
const refundStatusUnknown = domain.RefundStatus("unknown")

// outcomeRefundErr is a refund error that says whether Stripe may have made
// the refund, the way the Stripe refunder's errors do.
type outcomeRefundErr struct{ unknown bool }

func (e outcomeRefundErr) Error() string {
	if e.unknown {
		return "stripe refund: no answer"
	}
	return "stripe refund: refused"
}
func (e outcomeRefundErr) OutcomeUnknown() bool { return e.unknown }

// refusedRefundErr is a refund Stripe refused, carrying Stripe's message.
type refusedRefundErr struct{ msg string }

func (e refusedRefundErr) Error() string      { return e.msg }
func (refusedRefundErr) OutcomeUnknown() bool { return false }

// keyRecordingRefunder records the idempotency key of every refund request.
// Its Stripe holds no refund for any note, so a retry always reaches the
// create.
type keyRecordingRefunder struct {
	err      error
	refundID string
	keys     []string
}

func (r *keyRecordingRefunder) CreateRefund(_ context.Context, _ string, _ int64, key, _ string) (string, domain.RefundStatus, error) {
	r.keys = append(r.keys, key)
	if r.err != nil {
		return "", "", r.err
	}
	return r.refundID, domain.RefundSucceeded, nil
}

func (r *keyRecordingRefunder) GetRefund(context.Context, string) (domain.RefundStatus, string, error) {
	return domain.RefundFailed, "", nil
}

func (r *keyRecordingRefunder) FindRefundForCreditNote(context.Context, string, string) (string, domain.RefundStatus, error) {
	return "", "", nil
}

// TestRefundUnknown_RealStore runs #965 against Postgres: the CHECK constraint
// must accept the new status, the row must keep the unclear request's key, the
// webhook writer must leave the status alone, and the needs-attention list
// must show the note.
func TestRefundUnknown_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN refund unknown")
	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	// markUnknown puts a note in the state an unclear re-drive leaves: the
	// stored id (if any) is a refund Stripe reported failed, and the status is
	// unknown with the unclear request's key.
	markUnknown := func(t *testing.T, id string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx,
			`UPDATE credit_notes SET refund_status = 'unknown', refund_unknown_key = $1 WHERE id = $2`,
			"velox_cn_"+id+"_r_0123456789abcdef", id); err != nil {
			t.Fatalf("mark unknown: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	t.Run("an unclear answer is stored as unknown, and Retry resends the same key", func(t *testing.T) {
		_, inv := seedInvoiceForCN(t, db, tenantID, "unk-resend", domain.InvoicePaid, domain.PaymentSucceeded, "pi_unk_resend")
		refunder := &keyRecordingRefunder{err: outcomeRefundErr{unknown: true}, refundID: "re_unk_resent"}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "UNKRESEND"})

		cn, err := svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
		if err != nil {
			t.Fatalf("CreateRefund: %v", err)
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != refundStatusUnknown {
			t.Fatalf("stored refund_status = %q, want unknown", got.RefundStatus)
		}

		refunder.err = nil
		out, err := svc.RetryRefund(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("RetryRefund: %v", err)
		}
		want := "velox_cn_" + cn.ID
		if len(refunder.keys) != 2 || refunder.keys[0] != want || refunder.keys[1] != want {
			t.Fatalf("request keys = %v, want [%s %s]: the resend must reuse the unclear request's key", refunder.keys, want, want)
		}
		if out.RefundStatus != domain.RefundSucceeded || out.StripeRefundID != "re_unk_resent" {
			t.Fatalf("refund leg = (%q, %q), want (succeeded, re_unk_resent)", out.RefundStatus, out.StripeRefundID)
		}
	})

	t.Run("a refusal is stored as failed", func(t *testing.T) {
		_, inv := seedInvoiceForCN(t, db, tenantID, "unk-refused", domain.InvoicePaid, domain.PaymentSucceeded, "pi_unk_refused")
		refunder := &keyRecordingRefunder{err: outcomeRefundErr{unknown: false}}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "UNKREFUSED"})

		cn, err := svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
		if err != nil {
			t.Fatalf("CreateRefund: %v", err)
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("stored refund_status = %q, want failed", got.RefundStatus)
		}
	})

	t.Run("a webhook about the dead refund leaves unknown alone", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "unk-webhook", "re_unk_dead", domain.RefundFailed)
		markUnknown(t, cn.ID)

		// Each of these is a redelivery or a late event about re_unk_dead.
		// None of them is news about the attempt whose result is unknown.
		for _, st := range []domain.RefundStatus{domain.RefundFailed, domain.RefundSucceeded, domain.RefundPending} {
			if err := store.ApplyRefundWebhookStatus(ctx, tenantID, "re_unk_dead", st); err != nil {
				t.Fatalf("webhook %s: %v (want nil: the refund id is Velox's, the event is just not news)", st, err)
			}
			got, err := store.Get(ctx, tenantID, cn.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.RefundStatus != refundStatusUnknown {
				t.Fatalf("after a %s webhook for re_unk_dead, refund_status = %q, want unknown", st, got.RefundStatus)
			}
		}
	})

	t.Run("an unknown refund needs attention at once", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "unk-attention", "re_unk_attention_dead", domain.RefundFailed)
		markUnknown(t, cn.ID)

		list, err := store.List(ctx, creditnote.ListFilter{TenantID: tenantID, RefundStatus: "needs_attention"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, c := range list {
			if c.ID == cn.ID {
				return
			}
		}
		t.Fatalf("needs-attention list has %d notes, none of them %s (refund_status unknown)", len(list), cn.CreditNoteNumber)
	})
}
