package creditnote_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// refundStatusUnknown is the refund status for "Stripe may or may not have
// made the refund" (#965).
const refundStatusUnknown = domain.RefundUnknown

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
	// during runs inside CreateRefund, while the request is "at Stripe". A
	// test passes a context's cancel func to end the caller's context in the
	// middle of the call, as chi's request timeout does.
	during func()
}

func (r *keyRecordingRefunder) CreateRefund(ctx context.Context, _ string, _ int64, key, _ string) (string, domain.RefundStatus, error) {
	r.keys = append(r.keys, key)
	if r.during != nil {
		r.during()
		// stripe-go gives up with no answer from Stripe; the Stripe
		// refunder reads that as "result unknown".
		if ctx.Err() != nil {
			return "", "", outcomeRefundErr{unknown: true}
		}
	}
	if r.err != nil {
		return "", "", r.err
	}
	return r.refundID, domain.RefundSucceeded, nil
}

// GetRefund: the refund this fake made has succeeded; any other is dead.
func (r *keyRecordingRefunder) GetRefund(_ context.Context, id string) (domain.RefundStatus, string, error) {
	if r.refundID != "" && id == r.refundID {
		return domain.RefundSucceeded, "", nil
	}
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
			"hardcount_cn_"+id+"_r_0123456789abcdef", id); err != nil {
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
		want := "hardcount_cn_" + cn.ID
		if len(refunder.keys) != 2 || refunder.keys[0] != want || refunder.keys[1] != want {
			t.Fatalf("request keys = %v, want [%s %s]: the resend must reuse the unclear request's key", refunder.keys, want, want)
		}
		if out.RefundStatus != domain.RefundSucceeded || out.StripeRefundID != "re_unk_resent" {
			t.Fatalf("refund leg = (%q, %q), want (succeeded, re_unk_resent)", out.RefundStatus, out.StripeRefundID)
		}
		// The key goes with the unknown status and only with it: once the
		// result is known, nothing is left to resend.
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var keyCleared bool
		if err := tx.QueryRowContext(ctx,
			`SELECT refund_unknown_key IS NULL FROM credit_notes WHERE id = $1`, cn.ID).Scan(&keyCleared); err != nil {
			t.Fatalf("read refund_unknown_key: %v", err)
		}
		if !keyCleared {
			t.Fatal("refund_unknown_key is still set after the refund's result became known")
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
				t.Fatalf("webhook %s: %v (want nil: the refund id is Hardcount's, the event is just not news)", st, err)
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

	// Two retries of one unknown refund read the same row and resend the same
	// request. Stripe runs it once: one retry gets the refund back, the other
	// gets 409 (the first was still running), which reads as unknown. Whichever
	// write lands last, the refund Stripe made stays recorded.
	t.Run("a late unclear answer never overwrites the refund a concurrent retry recorded", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "unk-race", "", domain.RefundNone)
		key := "hardcount_cn_" + cn.ID
		write := func(w creditnote.RefundStatusWrite) {
			t.Helper()
			if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, w, nil); err != nil {
				t.Fatalf("write %+v: %v", w, err)
			}
		}
		// The issue-time request got no clear answer.
		write(creditnote.RefundStatusWrite{Status: refundStatusUnknown, UnknownKey: key})
		// The winning retry records the refund its resend got back.
		write(creditnote.RefundStatusWrite{Status: domain.RefundSucceeded, StripeRefundID: "re_unk_race", ProviderRead: true})
		// The losing retry's 409 lands last, written against the row it read.
		write(creditnote.RefundStatusWrite{Status: refundStatusUnknown, UnknownKey: key, ProviderRead: true})

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != domain.RefundSucceeded || got.StripeRefundID != "re_unk_race" || got.RefundUnknownKey != "" {
			t.Fatalf("refund leg = (%q, %q, key %q), want (succeeded, re_unk_race, no key)", got.RefundStatus, got.StripeRefundID, got.RefundUnknownKey)
		}

		// The key goes with unknown and only with it.
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{Status: refundStatusUnknown, ExpectedPriorRefundID: "re_unk_race"}, nil); err == nil {
			t.Fatal("store accepted status unknown with no request key")
		}
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{Status: domain.RefundFailed, ExpectedPriorRefundID: "re_unk_race", UnknownKey: key}, nil); err == nil {
			t.Fatal("store accepted a request key with status failed")
		}
	})

	// seedCardRefund is an issued note with a card refund on a paid invoice
	// with a PaymentIntent, its refund leg at status (and refundID, if any).
	seedCardRefund := func(t *testing.T, suffix string, status domain.RefundStatus, refundID string) domain.CreditNote {
		t.Helper()
		cust, inv := seedInvoiceForCN(t, db, tenantID, suffix, domain.InvoicePaid, domain.PaymentSucceeded, "pi_"+suffix)
		cn, err := store.Create(ctx, tenantID, domain.CreditNote{
			InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-" + suffix,
			Status: domain.CreditNoteIssued, Reason: "fraudulent",
			SubtotalCents: 5000, TotalCents: 5000, RefundAmountCents: 5000,
			Currency: "USD", RefundStatus: domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("seed credit note: %v", err)
		}
		if status != domain.RefundNone {
			if err := store.UpdateRefundStatus(ctx, tenantID, cn.ID, status, refundID); err != nil {
				t.Fatalf("stamp refund leg: %v", err)
			}
		}
		return cn
	}

	// A refund asked for through the API runs on the request's context, which
	// chi's middleware.Timeout ends after 30 seconds. When Stripe hangs, that
	// context ends during the Stripe call, and Postgres refuses a transaction
	// begun on it. The write that records the unclear result must still land.
	t.Run("a request that ends during the Stripe call still stores unknown at issue", func(t *testing.T) {
		_, inv := seedInvoiceForCN(t, db, tenantID, "unk-deadline-issue", domain.InvoicePaid, domain.PaymentSucceeded, "pi_unk_deadline_issue")
		refunder := &keyRecordingRefunder{}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "UNKDLISSUE"})
		svc.SetAuditLogger(audit.NewLogger(db)) // its row rides the same write
		reqCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		refunder.during = cancel

		cn, err := svc.CreateRefund(reqCtx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
		if err != nil {
			t.Fatalf("CreateRefund: %v (want the issued note with refund_status unknown)", err)
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != refundStatusUnknown || got.RefundUnknownKey != "hardcount_cn_"+cn.ID {
			t.Fatalf("stored refund leg = (%q, key %q), want (unknown, hardcount_cn_%s)", got.RefundStatus, got.RefundUnknownKey, cn.ID)
		}
	})

	t.Run("a request that ends during the Stripe call still stores unknown on retry", func(t *testing.T) {
		cn := seedCardRefund(t, "unk-deadline-retry", domain.RefundFailed, "")
		refunder := &keyRecordingRefunder{}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetAuditLogger(audit.NewLogger(db)) // its row rides the same write
		reqCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		refunder.during = cancel

		if _, err := svc.RetryRefund(reqCtx, tenantID, cn.ID); err == nil {
			t.Fatal("RetryRefund returned no error for an unclear answer")
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != refundStatusUnknown {
			t.Fatalf("stored refund_status = %q, want unknown: Stripe may have made the refund", got.RefundStatus)
		}
		if len(refunder.keys) != 1 || got.RefundUnknownKey != refunder.keys[0] {
			t.Fatalf("stored key = %q, want the key of the request sent %v", got.RefundUnknownKey, refunder.keys)
		}
	})

	// From "none", Issue's request (hardcount_cn_<id>) may have reached Stripe and
	// its outcome was never recorded, so Retry resends it rather than starting
	// a new request under a new key.
	t.Run("Retry from none resends the issue request", func(t *testing.T) {
		cn := seedCardRefund(t, "unk-none-resend", domain.RefundNone, "")
		refunder := &keyRecordingRefunder{refundID: "re_unk_none_resend"}
		svc := creditnote.NewService(store, invStore, refunder)

		out, err := svc.RetryRefund(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("RetryRefund: %v", err)
		}
		if want := "hardcount_cn_" + cn.ID; len(refunder.keys) != 1 || refunder.keys[0] != want {
			t.Fatalf("request keys = %v, want [%s]", refunder.keys, want)
		}
		if out.RefundStatus != domain.RefundSucceeded || out.StripeRefundID != "re_unk_none_resend" {
			t.Fatalf("refund leg = (%q, %q), want (succeeded, re_unk_none_resend)", out.RefundStatus, out.StripeRefundID)
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
