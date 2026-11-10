package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A refund creates a credit note, issues it (Stripe's refund call is inside
// the issue) and voids it if the issue fails: three transactions. They run on
// a ctx the client cannot cancel, so a client that disconnects, or the route
// timeout, cannot leave a draft half-way. A draft counts toward the invoice's
// refunded total, so a leftover one made the next full refund answer
// "invoice has already been fully refunded".

// cutCommitStore is the credit-note store with the commit-cut seam on the
// transaction that creates a note: after a successful create it cancels the
// caller's request and, if the ctx the create ran on is now done, returns
// context.Canceled. That is pgx committing on the transaction's ctx (stdlib
// Commit uses the BeginTx ctx), emulated at the store.
type cutCommitStore struct {
	*creditnote.PostgresStore
	cancelRequest context.CancelFunc
}

func (s *cutCommitStore) CreateUnderInvoiceLockAudited(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem,
	build func(tx *sql.Tx, existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, cn domain.CreditNote) error,
) (domain.CreditNote, error) {
	cn, err := s.PostgresStore.CreateUnderInvoiceLockAudited(ctx, tenantID, invoiceID, lines, build, emit)
	if err != nil || s.cancelRequest == nil {
		return cn, err
	}
	s.cancelRequest()
	if ctx.Err() != nil {
		return domain.CreditNote{}, context.Canceled
	}
	return cn, nil
}

// deadRequestInvoices is the invoice store whose first read on the issue's
// transaction ends the caller's request and fails: a request cut inside Issue.
type deadRequestInvoices struct {
	*invoice.PostgresStore
	cancelRequest context.CancelFunc
	failed        bool
}

func (s *deadRequestInvoices) LockForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string) (domain.Invoice, error) {
	if !s.failed {
		s.failed = true
		s.cancelRequest()
		return domain.Invoice{}, errors.New("connection reset by peer")
	}
	return s.PostgresStore.LockForCreditNoteTx(ctx, tx, tenantID, id)
}

// liveRefunds is what the invoice's credit notes that are not voided refund.
func liveRefunds(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, invoiceID string) (sum int64, drafts int) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(refund_amount_cents) FILTER (WHERE status <> 'voided'), 0),
		       COUNT(*) FILTER (WHERE status = 'draft')
		FROM credit_notes WHERE invoice_id = $1`, invoiceID).Scan(&sum, &drafts); err != nil {
		t.Fatalf("read refunds: %v", err)
	}
	return sum, drafts
}

// TestCreateRefund_ClientGoneDuringCreateCommit_NoteIsIssued: the request ends
// while the note's create commits. The create runs on a ctx the request
// cannot cancel, so it stands, and the refund goes on to issue the note.
// When it ran on the request's ctx, the create could commit and still return
// context canceled: the refund answered an error and left the draft.
//
// Mutation: take the refund's ctx after Create, and CreateRefund returns
// context canceled with a draft left.
func TestCreateRefund_ClientGoneDuringCreateCommit_NoteIsIssued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN refund client gone at create")
	_, inv := seedInvoiceForCN(t, db, tenantID, "gone-create", domain.InvoicePaid, domain.PaymentSucceeded, "pi_gone_create")
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	store := &cutCommitStore{PostgresStore: creditnote.NewPostgresStore(db), cancelRequest: cancel}
	svc := creditnote.NewService(store, invoice.NewPostgresStore(db), &keyRecordingRefunder{refundID: "re_gone_create"})
	svc.SetNumberGenerator(&seqNumbers{prefix: "GONECREATE"})

	cn, err := svc.CreateRefund(reqCtx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund: %v; want the issued note, the create committed", err)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Errorf("status = %s, want issued", cn.Status)
	}
	if sum, drafts := liveRefunds(t, ctx, db, tenantID, inv.ID); drafts != 0 || sum != inv.TotalAmountCents {
		t.Errorf("drafts = %d, refunded = %dc; want 0 and %dc", drafts, sum, inv.TotalAmountCents)
	}
}

// TestCreateRefund_IssueFailsOnDeadRequest_NoDraftAndFullRefundWorks: the
// request ends inside the issue, which fails. The compensating void runs on
// the refund's own ctx, so it voids the draft; the invoice's refunded total is
// back to 0 and a second full refund succeeds. When the void ran on the dead
// request's ctx it failed silently, the draft stayed, and the second full
// refund was refused. The same through the create endpoint with auto_issue.
//
// Mutation: the void back on the request's ctx, and the draft stays.
func TestCreateRefund_IssueFailsOnDeadRequest_NoDraftAndFullRefundWorks(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN refund client gone at issue")

	cases := []struct {
		name   string
		refund func(t *testing.T, svc *creditnote.Service, reqCtx context.Context, inv domain.Invoice)
	}{
		{"CreateRefund", func(t *testing.T, svc *creditnote.Service, reqCtx context.Context, inv domain.Invoice) {
			if _, err := svc.CreateRefund(reqCtx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"}); err == nil {
				t.Fatal("CreateRefund = nil with a failed issue, want the error")
			}
		}},
		{"create endpoint with auto_issue", func(t *testing.T, svc *creditnote.Service, reqCtx context.Context, inv domain.Invoice) {
			body := `{"invoice_id":"` + inv.ID + `","reason":"requested_by_customer","refund_amount_cents":10000,"auto_issue":true,` +
				`"lines":[{"description":"Refund","quantity":1,"unit_amount_cents":10000}]}`
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			creditnote.NewHandler(svc, creditnote.HandlerDeps{}).Routes().ServeHTTP(rr, req.WithContext(auth.WithTenantID(reqCtx, tenantID)))
			if rr.Code < 400 {
				t.Fatalf("create with auto_issue = %d, want an error (the issue failed). body=%s", rr.Code, rr.Body.String())
			}
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suffix := []string{"gone-issue-svc", "gone-issue-api"}[i]
			_, inv := seedInvoiceForCN(t, db, tenantID, suffix, domain.InvoicePaid, domain.PaymentSucceeded, "pi_"+suffix)
			reqCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			invoices := &deadRequestInvoices{PostgresStore: invoice.NewPostgresStore(db), cancelRequest: cancel}
			store := creditnote.NewPostgresStore(db)
			svc := creditnote.NewService(store, invoices, &keyRecordingRefunder{refundID: "re_" + suffix})
			svc.SetNumberGenerator(&seqNumbers{prefix: strings.ToUpper(strings.ReplaceAll(suffix, "-", ""))})

			c.refund(t, svc, reqCtx, inv)
			if sum, drafts := liveRefunds(t, ctx, db, tenantID, inv.ID); drafts != 0 || sum != 0 {
				t.Fatalf("drafts = %d, refunded = %dc after the failed issue; want 0 and 0: the draft is voided", drafts, sum)
			}

			again, err := svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
			if err != nil {
				t.Fatalf("second full refund: %v; want it issued", err)
			}
			if again.Status != domain.CreditNoteIssued || again.RefundAmountCents != inv.TotalAmountCents {
				t.Errorf("second refund: status %s, %dc; want issued, %dc", again.Status, again.RefundAmountCents, inv.TotalAmountCents)
			}
		})
	}
}
