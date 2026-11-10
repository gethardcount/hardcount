package creditnote_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// refundThroughEndpoint posts a full refund of the invoice to the create
// endpoint with auto_issue, on reqCtx.
func refundThroughEndpoint(t *testing.T, svc *creditnote.Service, reqCtx context.Context, tenantID string, inv domain.Invoice) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"invoice_id":%q,"reason":"requested_by_customer","refund_amount_cents":%d,"auto_issue":true,`+
		`"lines":[{"description":"Refund","quantity":1,"unit_amount_cents":%d}]}`, inv.ID, inv.TotalAmountCents, inv.TotalAmountCents)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	creditnote.NewHandler(svc, creditnote.HandlerDeps{}).Routes().ServeHTTP(rr, req.WithContext(auth.WithTenantID(reqCtx, tenantID)))
	return rr
}

// issuedNoteFrom reads the note an endpoint answered 201 with.
func issuedNoteFrom(t *testing.T, rr *httptest.ResponseRecorder) domain.CreditNote {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 with the issued note. body=%s", rr.Code, rr.Body.String())
	}
	var cn domain.CreditNote
	if err := json.Unmarshal(rr.Body.Bytes(), &cn); err != nil {
		t.Fatalf("decode the note: %v", err)
	}
	return cn
}

// TestCreateRefund_ClientGoneDuringCreateCommit_NoteIsIssued: the request ends
// while the note's create commits. The create and the issue run on a ctx the
// request cannot cancel, so the create stands and the note is issued. When
// they ran on the request's ctx, the create could commit and still return
// context canceled: the refund answered an error and left the draft. Through
// CreateRefund and through the create endpoint with auto_issue.
//
// Mutations: take the refund's ctx after Create, or run the endpoint's create
// or issue on the request's ctx, and an error comes back with a draft left.
func TestCreateRefund_ClientGoneDuringCreateCommit_NoteIsIssued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN refund client gone at create")
	cases := []struct {
		name   string
		refund func(t *testing.T, svc *creditnote.Service, reqCtx context.Context, inv domain.Invoice) domain.CreditNote
	}{
		{"CreateRefund", func(t *testing.T, svc *creditnote.Service, reqCtx context.Context, inv domain.Invoice) domain.CreditNote {
			cn, err := svc.CreateRefund(reqCtx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
			if err != nil {
				t.Fatalf("CreateRefund: %v; want the issued note, the create committed", err)
			}
			return cn
		}},
		{"create endpoint with auto_issue", func(t *testing.T, svc *creditnote.Service, reqCtx context.Context, inv domain.Invoice) domain.CreditNote {
			return issuedNoteFrom(t, refundThroughEndpoint(t, svc, reqCtx, tenantID, inv))
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suffix := []string{"gone-create-svc", "gone-create-api"}[i]
			_, inv := seedInvoiceForCN(t, db, tenantID, suffix, domain.InvoicePaid, domain.PaymentSucceeded, "pi_"+suffix)
			reqCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			store := &cutCommitStore{PostgresStore: creditnote.NewPostgresStore(db), cancelRequest: cancel}
			svc := creditnote.NewService(store, invoice.NewPostgresStore(db), &keyRecordingRefunder{refundID: "re_" + suffix})
			svc.SetNumberGenerator(&seqNumbers{prefix: strings.ToUpper(strings.ReplaceAll(suffix, "-", ""))})

			cn := c.refund(t, svc, reqCtx, inv)
			if cn.Status != domain.CreditNoteIssued {
				t.Errorf("status = %s, want issued", cn.Status)
			}
			if sum, drafts := liveRefunds(t, ctx, db, tenantID, inv.ID); drafts != 0 || sum != inv.TotalAmountCents {
				t.Errorf("drafts = %d, refunded = %dc; want 0 and %dc", drafts, sum, inv.TotalAmountCents)
			}
		})
	}
}

// failFirstIssuedGet fails the first read of a note that is already issued:
// Issue's read after its commit and its refund leg, so Issue returns an error
// for a note that is issued and refunded.
type failFirstIssuedGet struct {
	*creditnote.PostgresStore
	failed bool
}

func (s *failFirstIssuedGet) Get(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	cn, err := s.PostgresStore.Get(ctx, tenantID, id)
	if err == nil && cn.Status == domain.CreditNoteIssued && !s.failed {
		s.failed = true
		return domain.CreditNote{}, errors.New("connection reset by peer")
	}
	return cn, err
}

// TestCreateRefund_IssueCommitsThenFails_AnswersTheIssuedNote: Issue commits
// and its refund runs, then Issue returns an error (its read after the commit
// fails). The void refuses an issued note, so the caller gets the issued note
// with its refund status, not an error: told the refund failed, a client could
// send a new partial refund under a new key and pay twice. Through
// CreateRefund and through the create endpoint with auto_issue.
//
// Mutation: answer the issue error after the void refuses, and the caller is
// told the refund failed.
func TestCreateRefund_IssueCommitsThenFails_AnswersTheIssuedNote(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN refund issue commits then fails")
	cases := []struct {
		name   string
		refund func(t *testing.T, svc *creditnote.Service, inv domain.Invoice) domain.CreditNote
	}{
		{"CreateRefund", func(t *testing.T, svc *creditnote.Service, inv domain.Invoice) domain.CreditNote {
			cn, err := svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"})
			if err != nil {
				t.Fatalf("CreateRefund: %v; want the issued note, the refund was made", err)
			}
			return cn
		}},
		{"create endpoint with auto_issue", func(t *testing.T, svc *creditnote.Service, inv domain.Invoice) domain.CreditNote {
			return issuedNoteFrom(t, refundThroughEndpoint(t, svc, ctx, tenantID, inv))
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suffix := []string{"commit-fail-svc", "commit-fail-api"}[i]
			_, inv := seedInvoiceForCN(t, db, tenantID, suffix, domain.InvoicePaid, domain.PaymentSucceeded, "pi_"+suffix)
			store := &failFirstIssuedGet{PostgresStore: creditnote.NewPostgresStore(db)}
			svc := creditnote.NewService(store, invoice.NewPostgresStore(db), &keyRecordingRefunder{refundID: "re_" + suffix})
			svc.SetNumberGenerator(&seqNumbers{prefix: strings.ToUpper(strings.ReplaceAll(suffix, "-", ""))})

			cn := c.refund(t, svc, inv)
			if !store.failed {
				t.Fatal("the seam never failed a read: Issue did not reach its read after the commit")
			}
			if cn.Status != domain.CreditNoteIssued || cn.RefundStatus != domain.RefundSucceeded {
				t.Errorf("note = (%s, %s), want issued with the refund succeeded", cn.Status, cn.RefundStatus)
			}
			if sum, drafts := liveRefunds(t, ctx, db, tenantID, inv.ID); drafts != 0 || sum != inv.TotalAmountCents {
				t.Errorf("drafts = %d, refunded = %dc; want 0 and %dc", drafts, sum, inv.TotalAmountCents)
			}
		})
	}
}

// TestCreateRefund_IssueFailsOnDeadRequest_NoDraftAndFullRefundWorks: the
// request ends inside the issue, which fails. The compensating void runs on
// the refund's own ctx, so it voids the draft; the invoice's refunded total is
// back to 0 and a second full refund succeeds. When the void ran on the dead
// request's ctx it failed (silently on the refund path; the endpoint logged
// it), the draft stayed, and the second full refund was refused. The same
// through the create endpoint with auto_issue.
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
			rr := refundThroughEndpoint(t, svc, reqCtx, tenantID, inv)
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

// stalledIssueInvoices is the invoice store whose read on the issue's
// transaction waits until that transaction's ctx is done: a lock wait or a
// slow database that outlasts the refund's steps budget.
type stalledIssueInvoices struct {
	*invoice.PostgresStore
}

func (s *stalledIssueInvoices) LockForCreditNoteTx(ctx context.Context, _ *sql.Tx, _, _ string) (domain.Invoice, error) {
	<-ctx.Done()
	return domain.Invoice{}, ctx.Err()
}

// TestCreateRefund_IssueOutlastsTheBudget_TheDraftIsStillVoided: the issue
// waits until the refund's steps budget runs out and fails on its deadline.
// The compensating void takes its own ctx, so it still voids the draft:
// nothing is refunded, and nothing holds the invoice's refund headroom.
//
// Mutation: run the void on the spent steps ctx, and the draft stays.
func TestCreateRefund_IssueOutlastsTheBudget_TheDraftIsStillVoided(t *testing.T) {
	creditnote.ShortenStepsBudget(t, 300*time.Millisecond)
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN refund past its budget")
	_, inv := seedInvoiceForCN(t, db, tenantID, "budget-spent", domain.InvoicePaid, domain.PaymentSucceeded, "pi_budget_spent")
	svc := creditnote.NewService(creditnote.NewPostgresStore(db), &stalledIssueInvoices{PostgresStore: invoice.NewPostgresStore(db)}, &keyRecordingRefunder{refundID: "re_budget_spent"})
	svc.SetNumberGenerator(&seqNumbers{prefix: "BUDGETSPENT"})

	if _, err := svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{InvoiceID: inv.ID, Reason: "requested_by_customer"}); err == nil {
		t.Fatal("CreateRefund = nil with an issue past its budget, want the error")
	}
	if sum, drafts := liveRefunds(t, ctx, db, tenantID, inv.ID); drafts != 0 || sum != 0 {
		t.Errorf("drafts = %d, refunded = %dc; want 0 and 0: the void runs on its own ctx", drafts, sum)
	}
}
