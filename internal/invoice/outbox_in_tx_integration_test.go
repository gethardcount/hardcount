package invoice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Every invoice.* event is queued in the transaction that writes its change,
// so the change and its event commit together or not at all (ADR-040). Each
// test below stops the work right after the change commits, the way a
// crash, a deploy, a client disconnect or the route timeout does, and checks
// that the event was saved anyway, exactly once, with the row the change
// committed (ADR-131).
//
// Two ways of stopping are used. Where the code after the commit runs on the
// caller's context (finalize, offline payment), the context is cancelled. Where
// it runs on a detached context (a void's AfterVoid and a write-off), a
// cancel cannot stop it, so the step right after the commit panics instead:
// the process dies there.

// eventsWired builds the invoice store and service with webhook events wired
// as the router wires them.
func eventsWired(db *postgres.DB) (*invoice.PostgresStore, *invoice.Service) {
	outbox := webhook.NewOutboxStore(db)
	store := invoice.NewPostgresStore(db)
	store.SetOutboxEnqueuer(outbox)
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))
	svc.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
	return store, svc
}

// seedInvoice creates a customer and an invoice for it through a store with
// no outbox, so seeding queues no event.
func seedInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, inv domain.Invoice) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + inv.InvoiceNumber, DisplayName: "Outbox " + inv.InvoiceNumber,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	inv.CustomerID = cust.ID
	if inv.PaymentStatus == "" {
		inv.PaymentStatus = domain.PaymentPending
	}
	inv.Currency = "USD"
	inv.BillingPeriodStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	inv.BillingPeriodEnd = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	out, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, inv)
	if err != nil {
		t.Fatalf("seed invoice %s: %v", inv.InvoiceNumber, err)
	}
	return out
}

// queuedEvents returns the data of every webhook_outbox row of eventType
// about the invoice, read under the tenant's row-level security.
func queuedEvents(t *testing.T, db *postgres.DB, tenantID, eventType, invoiceID string) []map[string]any {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx,
		`SELECT payload FROM webhook_outbox WHERE event_type = $1 AND payload->'invoice'->>'id' = $2`,
		eventType, invoiceID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// asData is v as a webhook receiver decodes it.
func asData(t *testing.T, v any) map[string]any {
	t.Helper()
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(enc, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// oneEventOfTheRow checks that exactly one eventType row was queued for the
// invoice and that its data is the invoice as the database holds it now,
// plus extra. Callers wire no step that writes the row after the commit, so
// the row now is the row the change committed.
func oneEventOfTheRow(t *testing.T, db *postgres.DB, store *invoice.PostgresStore, tenantID, eventType, invoiceID string, extra map[string]any) map[string]any {
	t.Helper()
	got := queuedEvents(t, db, tenantID, eventType, invoiceID)
	if len(got) != 1 {
		t.Fatalf("%d %s rows for %s, want 1: the change committed, so its event must be saved with it", len(got), eventType, invoiceID)
	}
	if err := domain.CheckWebhookData(eventType, got[0]); err != nil {
		t.Fatal(err)
	}
	row, err := store.Get(postgres.WithLivemode(context.Background(), false), tenantID, invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if want := asData(t, domain.InvoiceWebhookData(row, extra)); !reflect.DeepEqual(got[0], want) {
		t.Errorf("%s data differs from the committed invoice:\n got %v\nwant %v", eventType, got[0], want)
	}
	return got[0]
}

// TestCreateWithLineItemsTx_FinalizedInvoiceQueuesItsEventInTheSameTx: an
// invoice born finalized (cycle close, first invoice, threshold, cancel,
// plan swap, proration) queues invoice.finalized on the transaction that
// inserts it. Rolling that transaction back removes the event with the
// invoice, and a draft queues nothing until it is finalized.
func TestCreateWithLineItemsTx_FinalizedInvoiceQueuesItsEventInTheSameTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Born Finalized")
	store, _ := eventsWired(db)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_obf", DisplayName: "Born Finalized"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	create := func(num string, status domain.InvoiceStatus, commit bool) domain.Invoice {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		inv, err := store.CreateWithLineItemsTx(ctx, tx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: status, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingReason:      domain.BillingReasonSubscriptionCycle,
			BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		}, nil)
		if err != nil {
			t.Fatalf("create %s: %v", num, err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		return inv
	}

	committed := create("HC-OBF-1", domain.InvoiceFinalized, true)
	data := oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoiceFinalized, committed.ID, nil)
	if token, _ := data[domain.WebhookResourceInvoice].(map[string]any)["public_token"].(string); token == "" {
		t.Error("invoice.finalized carries no public_token: a finalized invoice has a hosted page")
	}

	rolledBack := create("HC-OBF-2", domain.InvoiceFinalized, false)
	if got := queuedEvents(t, db, tenantID, domain.EventInvoiceFinalized, rolledBack.ID); len(got) != 0 {
		t.Errorf("%d invoice.finalized rows for an invoice whose transaction rolled back, want 0", len(got))
	}

	draft := create("HC-OBF-3", domain.InvoiceDraft, true)
	if got := queuedEvents(t, db, tenantID, domain.EventInvoiceFinalized, draft.ID); len(got) != 0 {
		t.Errorf("%d invoice.finalized rows for a draft, want 0: it is sent when the draft is finalized", len(got))
	}
}

// endAfterCommit is a held-relief issuer that issues nothing. Its post-commit
// step, the first that runs after the finalize commits, ends the caller's
// context.
type endAfterCommit struct{ end context.CancelFunc }

func (r endAfterCommit) IssueHeldReliefTx(context.Context, *sql.Tx, string, domain.Invoice) (func(context.Context), error) {
	return func(context.Context) { r.end() }, nil
}

// TestFinalize_ContextEndsAfterCommit_InvoiceFinalizedIsQueued: finalizing a
// draft queues invoice.finalized in the finalize transaction, for an
// operator-composed draft (FinalizeWithDates) and a subscription's draft
// (FinalizeDraft) alike. The caller's context ends right after the commit,
// and the event is still saved, with the hosted-page token the finalize gave
// the invoice.
func TestFinalize_ContextEndsAfterCommit_InvoiceFinalizedIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Finalize Ends")
	store, svc := eventsWired(db)

	for _, tc := range []struct {
		name   string
		num    string
		reason domain.InvoiceBillingReason
	}{
		{"operator-composed draft", "HC-OFE-MANUAL", domain.BillingReasonManual},
		{"subscription draft", "HC-OFE-CYCLE", domain.BillingReasonSubscriptionCycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := seedInvoice(t, db, base, tenantID, domain.Invoice{
				InvoiceNumber: tc.num, Status: domain.InvoiceDraft, BillingReason: tc.reason,
				SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			})
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			svc.SetHeldReliefIssuer(endAfterCommit{end: cancel})

			returned, err := svc.Finalize(ctx, tenantID, draft.ID)
			if err != nil {
				t.Fatalf("finalize: %v", err)
			}
			data := oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoiceFinalized, draft.ID, nil)
			inv, _ := data[domain.WebhookResourceInvoice].(map[string]any)
			if inv["status"] != string(domain.InvoiceFinalized) {
				t.Errorf("invoice.finalized status = %v, want finalized", inv["status"])
			}
			if token, _ := inv["public_token"].(string); token == "" {
				t.Error("invoice.finalized carries no public_token: the finalize gives the invoice its hosted page")
			}
			row, err := store.Get(base, tenantID, draft.ID)
			if err != nil {
				t.Fatal(err)
			}
			if returned.PublicToken != row.PublicToken || !returned.UpdatedAt.Equal(row.UpdatedAt) {
				t.Errorf("Finalize returned token %q updated_at %v; the committed row has %q %v",
					returned.PublicToken, returned.UpdatedAt, row.PublicToken, row.UpdatedAt)
			}
		})
	}
}

// TestFinalize_InvoiceFinalizedIsSentOnce: the event is the invoice the
// finalize committed, including credit a draft already carried (#1281), and
// a second finalize, which is refused, queues no second event.
func TestFinalize_InvoiceFinalizedIsSentOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Finalize Once")
	store, svc := eventsWired(db)
	draft := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-OFO-1", Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonManual,
		SubtotalCents: 10000, TotalAmountCents: 10000, CreditsAppliedCents: 2500, AmountDueCents: 7500,
	})

	if _, err := svc.Finalize(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := svc.Finalize(ctx, tenantID, draft.ID); err == nil {
		t.Fatal("second finalize succeeded, want refused")
	}
	data := oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoiceFinalized, draft.ID, nil)
	inv, _ := data[domain.WebhookResourceInvoice].(map[string]any)
	if inv["credits_applied_cents"] != float64(2500) || inv["amount_due_cents"] != float64(7500) || inv["total_amount_cents"] != float64(10000) {
		t.Errorf("amounts: credits_applied %v due %v total %v, want 2500, 7500 and 10000",
			inv["credits_applied_cents"], inv["amount_due_cents"], inv["total_amount_cents"])
	}
}

// crashingStopper is the Stripe leg of a void, the first step after the void
// commits. It panics: the process dies there.
type crashingStopper struct{}

func (crashingStopper) StopCollection(context.Context, string, domain.Invoice) {
	panic("process stopped after the void committed")
}

func (crashingStopper) ExpireCheckoutSessions(context.Context, string, domain.Invoice) {}

// survive runs fn and recovers the panic a crashing test double raises.
func survive(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// TestVoid_ProcessStopsAfterCommit_InvoiceVoidedIsQueued: a void queues
// invoice.voided in its own transaction, so the process dying right after
// the commit cannot lose it. A second void is refused and queues nothing.
func TestVoid_ProcessStopsAfterCommit_InvoiceVoidedIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Void Stops")
	store, svc := eventsWired(db)
	svc.SetCollectionStopper(crashingStopper{})
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-OVS-1", Status: domain.InvoiceFinalized,
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
	})

	survive(func() { _, _ = svc.Void(ctx, tenantID, inv.ID) })
	row, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != domain.InvoiceVoided {
		t.Fatalf("status = %s, want voided", row.Status)
	}
	oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoiceVoided, inv.ID, nil)

	if _, err := svc.Void(ctx, tenantID, inv.ID); err == nil {
		t.Fatal("second void succeeded, want refused")
	}
	if got := queuedEvents(t, db, tenantID, domain.EventInvoiceVoided, inv.ID); len(got) != 1 {
		t.Errorf("%d invoice.voided rows after a refused second void, want 1", len(got))
	}
}

// TestVoidUnchangedTx_EventCommitsWithTheCallerTx: the relief void of a
// cancel or plan swap runs on the caller's transaction, and so does its
// invoice.voided. The caller rolling back removes the event with the void;
// the caller committing saves both, before anything runs after the commit.
func TestVoidUnchangedTx_EventCommitsWithTheCallerTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Relief Void")
	store, svc := eventsWired(db)
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-ORV-1", Status: domain.InvoiceFinalized,
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
	})
	voidOnCallerTx := func(commit bool) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := svc.VoidUnchangedTx(ctx, tx, tenantID, inv); err != nil {
			t.Fatalf("void on the caller's tx: %v", err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
	}

	voidOnCallerTx(false)
	if got := queuedEvents(t, db, tenantID, domain.EventInvoiceVoided, inv.ID); len(got) != 0 {
		t.Errorf("%d invoice.voided rows after the caller rolled back, want 0", len(got))
	}
	voidOnCallerTx(true)
	oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoiceVoided, inv.ID, nil)
}

// crashingAudit is an audit logger whose post-commit Log panics: the process
// dies at the first step after a write-off commits.
type crashingAudit struct{ invoice.AuditLogger }

func (crashingAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	panic("process stopped after the write-off committed")
}

// TestMarkUncollectible_ProcessStopsAfterCommit_EventIsQueued: a write-off
// queues invoice.marked_uncollectible in its own transaction, so the process
// dying right after the commit cannot lose it. A second write-off is refused
// and queues nothing.
func TestMarkUncollectible_ProcessStopsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Write-off Stops")
	store, svc := eventsWired(db)
	svc.SetAuditLogger(crashingAudit{AuditLogger: audit.NewLogger(db)})
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-OWS-1", Status: domain.InvoiceFinalized,
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
	})

	survive(func() { _, _ = svc.MarkUncollectible(ctx, tenantID, inv.ID) })
	row, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != domain.InvoiceUncollectible {
		t.Fatalf("status = %s, want uncollectible", row.Status)
	}
	oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoiceMarkedUncollectible, inv.ID, nil)

	svc.SetAuditLogger(audit.NewLogger(db))
	if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err == nil {
		t.Fatal("second write-off succeeded, want refused")
	}
	if got := queuedEvents(t, db, tenantID, domain.EventInvoiceMarkedUncollectible, inv.ID); len(got) != 1 {
		t.Errorf("%d invoice.marked_uncollectible rows after a refused second write-off, want 1", len(got))
	}
}

// endingAuditLog is an audit logger whose post-commit Log ends the caller's
// context: it is the first step after an offline payment commits.
type endingAuditLog struct {
	invoice.AuditLogger
	end context.CancelFunc
}

func (a endingAuditLog) Log(context.Context, string, string, string, string, string, map[string]any) error {
	a.end()
	return nil
}

// TestRecordOfflinePayment_ContextEndsAfterCommit_EventIsQueued: recording a
// payment made outside Hardcount queues invoice.payment_recorded in the
// transaction that marks the invoice paid, beside invoice.paid. The caller's
// context ends right after the commit, and both events are still saved. The
// event carries the amount booked, not the zeroed amount due. A second
// recording is refused and queues nothing.
func TestRecordOfflinePayment_ContextEndsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Offline Ends")
	store, svc := eventsWired(db)
	inv := seedInvoice(t, db, base, tenantID, domain.Invoice{
		InvoiceNumber: "HC-OOE-1", Status: domain.InvoiceFinalized,
		SubtotalCents: 4200, TotalAmountCents: 4200, AmountDueCents: 4200,
	})
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	svc.SetAuditLogger(endingAuditLog{AuditLogger: audit.NewLogger(db), end: cancel})

	if _, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, "Wire ref 7"); err != nil {
		t.Fatalf("record offline payment: %v", err)
	}
	oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoicePaid, inv.ID, nil)
	data := oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoicePaymentRecorded, inv.ID,
		map[string]any{"recovered_from": string(domain.InvoiceFinalized)})
	paid, _ := data[domain.WebhookResourceInvoice].(map[string]any)
	if paid["amount_paid_cents"] != float64(4200) || paid["amount_due_cents"] != float64(0) {
		t.Errorf("invoice.payment_recorded amounts: paid %v due %v, want 4200 and 0", paid["amount_paid_cents"], paid["amount_due_cents"])
	}

	svc.SetAuditLogger(audit.NewLogger(db))
	if _, err := svc.RecordOfflinePayment(base, tenantID, inv.ID, ""); err == nil {
		t.Fatal("second offline payment succeeded, want refused")
	}
	if got := queuedEvents(t, db, tenantID, domain.EventInvoicePaymentRecorded, inv.ID); len(got) != 1 {
		t.Errorf("%d invoice.payment_recorded rows after a refused second payment, want 1", len(got))
	}
}

// writeOffMidway is a credit-note reader that, when the offline payment asks
// it, writes the invoice off: a write-off committed after the payment read
// the invoice and before it locked the row.
type writeOffMidway struct {
	store    *invoice.PostgresStore
	tenantID string
}

func (w writeOffMidway) IssuedCreditedCents(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (w writeOffMidway) UnreliefedClawbackCents(ctx context.Context, _, invoiceID string) (int64, error) {
	if _, err := w.store.UpdateStatusWithReversalPrior(ctx, w.tenantID, invoiceID, domain.InvoiceUncollectible, nil); err != nil {
		return 0, err
	}
	return 0, nil
}

// TestRecordOfflinePayment_RecoveredFromIsTheStatusUnderTheLock: recovered_from
// is the status the paid flip moved the invoice from, read under its row
// lock. A write-off that commits between the payment's first read and that
// lock makes it uncollectible, not the finalized the first read saw.
func TestRecordOfflinePayment_RecoveredFromIsTheStatusUnderTheLock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Offline Race")
	store, svc := eventsWired(db)
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-OOR-1", Status: domain.InvoiceFinalized,
		SubtotalCents: 3000, TotalAmountCents: 3000, AmountDueCents: 3000,
	})
	svc.SetCreditNoteTotaler(writeOffMidway{store: invoice.NewPostgresStore(db), tenantID: tenantID})

	if _, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, ""); err != nil {
		t.Fatalf("record offline payment: %v", err)
	}
	oneEventOfTheRow(t, db, store, tenantID, domain.EventInvoicePaymentRecorded, inv.ID,
		map[string]any{"recovered_from": string(domain.InvoiceUncollectible)})
}
