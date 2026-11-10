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
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Issuing a credit note, alone or as the issue step of create with
// auto_issue, answers 409 concurrent_change when it loses a database
// deadlock, and the idempotency layer does not store it. The same request
// with the same Idempotency-Key then runs again and issues one note
// (ADR-136). When auto_issue cannot void the draft it made, a resend would
// make a second note, so that answer stays the stored 500.
//
// The credit-note and invoice stores, the audit log and the idempotency
// middleware are real Postgres. deadlockOnceLock makes the first invoice lock
// of an issue fail with SQLSTATE 40P01, as Postgres fails the side it stops
// to break a deadlock with a cancel.
//
// Mutation-verify: drop respond.ConcurrentChangeAsConflict from the issue handler
// (issue) or from create's auto_issue step (auto_issue): the first answer is
// a 500, and the same key replays it. Convert at the auto_issue step even
// when the void failed (auto_issue_void_fails): the answer is a 409.

// deadlockOnceLock is the real invoice store, except that the first
// LockForCreditNoteTx after arming loses a deadlock.
type deadlockOnceLock struct {
	*invoice.PostgresStore
	armed atomic.Bool
}

func (s *deadlockOnceLock) LockForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string) (domain.Invoice, error) {
	if s.armed.CompareAndSwap(true, false) {
		return domain.Invoice{}, fmt.Errorf("lock the invoice row: %w", &pgconn.PgError{Severity: "ERROR", Code: "40P01", Message: "deadlock detected"})
	}
	return s.PostgresStore.LockForCreditNoteTx(ctx, tx, tenantID, id)
}

var errVoidFailed = errors.New("void the draft: connection reset by peer")

// voidFailsStore is the real credit-note store, except that a void fails
// while failVoid is set.
type voidFailsStore struct {
	*creditnote.PostgresStore
	failVoid atomic.Bool
}

func (s *voidFailsStore) TransitionStatusAudited(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus, emit func(tx *sql.Tx) error) (bool, error) {
	if to == domain.CreditNoteVoided && s.failVoid.Load() {
		return false, errVoidFailed
	}
	return s.PostgresStore.TransitionStatusAudited(ctx, tenantID, id, from, to, emit)
}

type deadlockCNRig struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	inv      domain.Invoice
	svc      *creditnote.Service
	invoices *deadlockOnceLock
	notes    *voidFailsStore
	logger   *audit.Logger
	srv      http.Handler
}

// newDeadlockCNRig seeds a finalized invoice of 10000c, due, behind the
// credit-note routes and the idempotency middleware.
func newDeadlockCNRig(t *testing.T, db *postgres.DB, suffix string) *deadlockCNRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Deadlock "+suffix)
	_, inv := seedInvoiceForCN(t, db, tenantID, "deadlock-"+suffix, domain.InvoiceFinalized, domain.PaymentPending, "")
	invoices := &deadlockOnceLock{PostgresStore: invoice.NewPostgresStore(db)}
	notes := &voidFailsStore{PostgresStore: creditnote.NewPostgresStore(db)}
	logger := audit.NewLogger(db)
	svc := creditnote.NewService(notes, invoices, nil)
	svc.SetNumberGenerator(&seqNumbers{prefix: "DEADLOCK" + strings.ToUpper(suffix)})
	svc.SetAuditLogger(logger)
	return &deadlockCNRig{
		t: t, db: db, ctx: ctx, tenantID: tenantID, inv: inv, svc: svc,
		invoices: invoices, notes: notes, logger: logger,
		srv: middleware.Idempotency(db, nil)(creditnote.NewHandler(svc, creditnote.HandlerDeps{}).Routes()),
	}
}

func (r *deadlockCNRig) post(path, key, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rr := httptest.NewRecorder()
	r.srv.ServeHTTP(rr, req.WithContext(auth.WithTenantID(r.ctx, r.tenantID)))
	return rr
}

// createBody is a 3000c credit note on the rig's invoice.
func (r *deadlockCNRig) createBody(autoIssue bool) string {
	return fmt.Sprintf(`{"invoice_id":%q,"reason":"order_change","auto_issue":%t,`+
		`"lines":[{"description":"over-billed seat","quantity":1,"unit_amount_cents":3000}]}`, r.inv.ID, autoIssue)
}

// notesByStatus counts the invoice's credit notes per status.
func (r *deadlockCNRig) notesByStatus() map[domain.CreditNoteStatus]int {
	r.t.Helper()
	cns, err := r.svc.List(r.ctx, creditnote.ListFilter{TenantID: r.tenantID, InvoiceID: r.inv.ID})
	if err != nil {
		r.t.Fatalf("list credit notes: %v", err)
	}
	out := map[domain.CreditNoteStatus]int{}
	for _, cn := range cns {
		out[cn.Status]++
	}
	return out
}

// issuedRows counts the credit_note.issued audit rows of the tenant.
func (r *deadlockCNRig) issuedRows() int {
	r.t.Helper()
	_, total, err := r.logger.Query(r.ctx, r.tenantID, audit.QueryFilter{ResourceType: "credit_note", Action: "credit_note.issued"})
	if err != nil {
		r.t.Fatalf("query audit: %v", err)
	}
	return total
}

func (r *deadlockCNRig) amountDue() int64 {
	r.t.Helper()
	got, err := r.invoices.Get(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return got.AmountDueCents
}

func wantCNConcurrentChange(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if rr.Code != http.StatusConflict || env.Error.Code != "concurrent_change" {
		t.Fatalf("deadlock loser: %d %s, want 409 concurrent_change", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("deadlock loser answered from the idempotency cache")
	}
}

func wantCNRanAgain(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want || rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("resend with the same key: %d (replayed=%q) %s, want %d from the handler",
			rr.Code, rr.Header().Get("Idempotent-Replayed"), rr.Body.String(), want)
	}
}

func TestDeadlockLoss_AnswersConflict_NotStored(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("issue", func(t *testing.T) {
		r := newDeadlockCNRig(t, db, "issue")
		cn, err := r.svc.Create(r.ctx, r.tenantID, creditnote.CreateInput{
			InvoiceID: r.inv.ID, Reason: "order_change",
			Lines: []creditnote.CreditLineInput{{Description: "over-billed seat", Quantity: 1, UnitAmountCents: 3000}},
		})
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		r.invoices.armed.Store(true)
		path := "/" + cn.ID + "/issue"

		wantCNConcurrentChange(t, r.post(path, "idem-deadlock-issue", ""))
		if got := r.notesByStatus(); got[domain.CreditNoteDraft] != 1 || got[domain.CreditNoteIssued] != 0 {
			t.Fatalf("after the lost issue: notes %v, want the one draft", got)
		}
		if due, rows := r.amountDue(), r.issuedRows(); due != 10000 || rows != 0 {
			t.Fatalf("after the lost issue: %dc due and %d issued rows, want 10000c and 0", due, rows)
		}

		wantCNRanAgain(t, r.post(path, "idem-deadlock-issue", ""), http.StatusOK)
		if got := r.notesByStatus(); got[domain.CreditNoteIssued] != 1 || got[domain.CreditNoteDraft] != 0 {
			t.Fatalf("after the resend: notes %v, want the one note issued", got)
		}
		if due, rows := r.amountDue(), r.issuedRows(); due != 7000 || rows != 1 {
			t.Fatalf("after the resend: %dc due and %d issued rows, want 7000c and 1", due, rows)
		}
	})

	t.Run("auto_issue", func(t *testing.T) {
		r := newDeadlockCNRig(t, db, "auto")
		r.invoices.armed.Store(true)

		// Create commits a draft; its issue loses the deadlock, and the
		// handler voids the draft.
		wantCNConcurrentChange(t, r.post("/", "idem-deadlock-auto", r.createBody(true)))
		if got := r.notesByStatus(); got[domain.CreditNoteVoided] != 1 || len(got) != 1 {
			t.Fatalf("after the lost issue: notes %v, want the one draft voided", got)
		}
		if due, rows := r.amountDue(), r.issuedRows(); due != 10000 || rows != 0 {
			t.Fatalf("after the lost issue: %dc due and %d issued rows, want 10000c and 0", due, rows)
		}

		wantCNRanAgain(t, r.post("/", "idem-deadlock-auto", r.createBody(true)), http.StatusCreated)
		if got := r.notesByStatus(); got[domain.CreditNoteVoided] != 1 || got[domain.CreditNoteIssued] != 1 || len(got) != 2 {
			t.Fatalf("after the resend: notes %v, want the first voided and the resend's issued", got)
		}
		if due, rows := r.amountDue(), r.issuedRows(); due != 7000 || rows != 1 {
			t.Fatalf("after the resend: %dc due and %d issued rows, want 7000c and 1", due, rows)
		}
	})

	t.Run("auto_issue_void_fails", func(t *testing.T) {
		// The issue loses the deadlock and the void of its draft fails too.
		// The draft stays, so a resend would make a second note: the answer
		// is the 500 the idempotency layer stores and replays.
		r := newDeadlockCNRig(t, db, "voidfails")
		r.invoices.armed.Store(true)
		r.notes.failVoid.Store(true)

		rr := r.post("/", "idem-deadlock-voidfails", r.createBody(true))
		if rr.Code != http.StatusInternalServerError || rr.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("lost issue, failed void: %d (replayed=%q) %s, want 500 from the handler",
				rr.Code, rr.Header().Get("Idempotent-Replayed"), rr.Body.String())
		}
		if got := r.notesByStatus(); got[domain.CreditNoteDraft] != 1 || len(got) != 1 {
			t.Fatalf("after the failed void: notes %v, want the one draft", got)
		}

		rr = r.post("/", "idem-deadlock-voidfails", r.createBody(true))
		if rr.Code != http.StatusInternalServerError || rr.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("resend with the same key: %d (replayed=%q), want the stored 500", rr.Code, rr.Header().Get("Idempotent-Replayed"))
		}
		if got := r.notesByStatus(); got[domain.CreditNoteDraft] != 1 || len(got) != 1 {
			t.Fatalf("after the resend: notes %v, want still the one draft and no second note", got)
		}
	})
}
