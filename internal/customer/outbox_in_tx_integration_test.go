package customer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// customer.email_bounced and customer.email_complained are queued in the
// transaction of the suppression write they report (ADR-040), so the write
// and its event commit together. Each test here stops the work right after
// that commit, the way a deploy or a dropped request does, or fails the
// event's insert, and checks that the write and its event were saved
// together or not at all. The event carries the customer as
// GET /v1/customers/{id} returns it after the commit (ADR-131).

// stopAfterWrite is the real customer store, except that it ends the
// caller's context as soon as a suppression write returns: the first step
// after its commit.
type stopAfterWrite struct {
	*customer.PostgresStore
	stop context.CancelFunc
}

func (s *stopAfterWrite) MarkEmailBounced(ctx context.Context, tenantID, customerID, reason string) error {
	err := s.PostgresStore.MarkEmailBounced(ctx, tenantID, customerID, reason)
	s.stop()
	return err
}

func (s *stopAfterWrite) MarkEmailComplained(ctx context.Context, tenantID, customerID, reason string) error {
	err := s.PostgresStore.MarkEmailComplained(ctx, tenantID, customerID, reason)
	s.stop()
	return err
}

var errOutboxDown = errors.New("webhook outbox insert failed: connection reset")

// faultyOutbox is the real webhook outbox, except that the first insert of
// failType fails, as a dropped connection does at that moment. An empty
// failType never fails.
type faultyOutbox struct {
	outbox   *webhook.OutboxStore
	failType string
	failed   atomic.Bool
}

func (o *faultyOutbox) fails(eventType string) bool {
	return o.failType != "" && eventType == o.failType && o.failed.CompareAndSwap(false, true)
}

func (o *faultyOutbox) Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error) {
	if o.fails(eventType) {
		return "", errOutboxDown
	}
	return o.outbox.Enqueue(ctx, tx, tenantID, eventType, payload)
}

// Dispatch is the post-commit path as router.go wires it on main
// (webhook.OutboxDispatcher: an insert in a transaction of its own).
func (o *faultyOutbox) Dispatch(ctx context.Context, tenantID, eventType string, payload map[string]any) error {
	if o.fails(eventType) {
		return errOutboxDown
	}
	_, err := o.outbox.EnqueueStandalone(ctx, tenantID, eventType, payload)
	return err
}

// suppressionRig is one tenant's customer, with the customer store and
// service wired as router.go wires them.
type suppressionRig struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	store    *customer.PostgresStore
	stopper  *stopAfterWrite
	svc      *customer.Service
	customer domain.Customer
}

func newSuppressionRig(t *testing.T, db *postgres.DB, name, failType string) *suppressionRig {
	t.Helper()
	tenantID := testutil.CreateTestTenant(t, db, name)
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), false), tenantID)
	store := customer.NewPostgresStore(db)
	stopper := &stopAfterWrite{PostgresStore: store, stop: func() {}}
	svc := customer.NewService(stopper)
	svc.SetEventDispatcher(&faultyOutbox{outbox: webhook.NewOutboxStore(db), failType: failType})
	c, err := svc.Create(ctx, tenantID, customer.CreateInput{
		ExternalID: "outbox-suppression", DisplayName: "Suppression Co", Email: "ap@suppression.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &suppressionRig{db: db, ctx: ctx, tenantID: tenantID, store: store, stopper: stopper, svc: svc, customer: c}
}

// events returns the data of the tenant's eventType outbox rows, newest
// first, read under its row-level security.
func (r *suppressionRig) events(t *testing.T, eventType string) []map[string]any {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx,
		`SELECT payload FROM webhook_outbox WHERE event_type = $1 ORDER BY created_at DESC, id DESC`, eventType)
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

// checkEvents checks that the customer has want eventType rows, and that the
// newest carries the customer as GET returns it now, with reason beside it.
func (r *suppressionRig) checkEvents(t *testing.T, eventType string, want int, reason string) {
	t.Helper()
	got := r.events(t, eventType)
	if len(got) != want {
		t.Errorf("%d %s rows, want %d: each suppression write that commits saves its event with it", len(got), eventType, want)
		return
	}
	if want == 0 {
		return
	}
	if err := domain.CheckWebhookData(eventType, got[0]); err != nil {
		t.Error(err)
	}
	c, err := r.store.Get(r.ctx, r.tenantID, r.customer.ID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}
	enc, err := json.Marshal(domain.CustomerWebhookData(c, map[string]any{"reason": reason}))
	if err != nil {
		t.Fatal(err)
	}
	var wantData map[string]any
	if err := json.Unmarshal(enc, &wantData); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0], wantData) {
		t.Errorf("%s data differs from the customer the write committed:\n got %v\nwant %v", eventType, got[0], wantData)
	}
}

// emailStatus reads the customer's email_status.
func (r *suppressionRig) emailStatus(t *testing.T) domain.CustomerEmailStatus {
	t.Helper()
	c, err := r.store.Get(r.ctx, r.tenantID, r.customer.ID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}
	return c.EmailStatus
}

// TestEmailSuppression_ContextEndsAfterCommit_EventIsQueued: each
// suppression write's context ends right after it commits. Its event was
// saved with it, once per signal. A bounce after a complaint changes nothing
// on the row (a complaint outranks a bounce), and its event is still sent.
func TestEmailSuppression_ContextEndsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newSuppressionRig(t, db, "Outbox Customer Suppression", "")

	steps := []struct {
		name, eventType, reason string
		mark                    func(ctx context.Context, tenantID, customerID, reason string) error
		wantStatus              domain.CustomerEmailStatus
		wantRows                int
	}{
		{"bounced", domain.EventCustomerEmailBounced, "mailbox does not exist", r.svc.MarkEmailBounced, domain.EmailStatusBounced, 1},
		{"complained", domain.EventCustomerEmailComplained, "marked as spam", r.svc.MarkEmailComplained, domain.EmailStatusComplained, 1},
		{"bounced after the complaint", domain.EventCustomerEmailBounced, "mailbox full for good", r.svc.MarkEmailBounced, domain.EmailStatusComplained, 2},
	}
	for _, s := range steps {
		ctx, cancel := context.WithCancel(r.ctx)
		r.stopper.stop = cancel
		err := s.mark(ctx, r.tenantID, r.customer.ID, s.reason)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := r.emailStatus(t); got != s.wantStatus {
			t.Fatalf("%s: email_status %s, want %s", s.name, got, s.wantStatus)
		}
		r.checkEvents(t, s.eventType, s.wantRows, s.reason)
	}
}

// TestEmailSuppression_EventInsertFails_NothingIsSaved: the insert of
// customer.email_bounced fails. The bounce write fails with it, so the
// caller gets the error (the Postmark webhook answers 5xx and Postmark sends
// it again) and the customer is unchanged. The second delivery saves both.
func TestEmailSuppression_EventInsertFails_NothingIsSaved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newSuppressionRig(t, db, "Outbox Customer Suppression Fails", domain.EventCustomerEmailBounced)

	err := r.svc.MarkEmailBounced(r.ctx, r.tenantID, r.customer.ID, "mailbox does not exist")
	if !errors.Is(err, errOutboxDown) {
		t.Errorf("bounce: err=%v, want the outbox error: the write and its event commit together or not at all", err)
	}
	if got := r.emailStatus(t); got != domain.EmailStatusUnknown {
		t.Errorf("email_status %s after the failed insert, want %s: the event was not saved, so neither is the bounce", got, domain.EmailStatusUnknown)
	}
	r.checkEvents(t, domain.EventCustomerEmailBounced, 0, "")

	if err := r.svc.MarkEmailBounced(r.ctx, r.tenantID, r.customer.ID, "mailbox does not exist"); err != nil {
		t.Fatalf("second bounce: %v", err)
	}
	if got := r.emailStatus(t); got != domain.EmailStatusBounced {
		t.Errorf("email_status %s after the second bounce, want bounced", got)
	}
	r.checkEvents(t, domain.EventCustomerEmailBounced, 1, "mailbox does not exist")
}
