package api

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// An SMTP bounce (bounceReporterAdapter.ReportBounce) has no caller to
// return an error to, and nothing sends it again. The bounced status and its
// customer.email_bounced event commit together, so a failed event insert
// loses both. The ERROR log line is then the only record of the bounce.

var errBounceOutboxDown = errors.New("webhook outbox insert failed: connection reset")

// downOutbox fails every webhook-event insert, as a dropped connection does.
type downOutbox struct{}

func (downOutbox) Enqueue(context.Context, *sql.Tx, string, string, map[string]any) (string, error) {
	return "", errBounceOutboxDown
}

// deleteBeforeBounce is the real customer store, except that the customer
// row is deleted just before the bounce write. That is a customer deleted
// between the blind-index lookup and the write.
type deleteBeforeBounce struct {
	*customer.PostgresStore
	db *postgres.DB
}

func (s *deleteBeforeBounce) MarkEmailBounced(ctx context.Context, tenantID, customerID, reason string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `DELETE FROM customers WHERE id = $1`, customerID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.PostgresStore.MarkEmailBounced(ctx, tenantID, customerID, reason)
}

func TestReportBounce_EventInsertFails_LogsTheLostBounce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	blinder, err := crypto.NewBlinder(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("blinder: %v", err)
	}
	const addr = "ap@bounce-log.test"

	newRig := func(t *testing.T, name string, wrap func(*customer.PostgresStore) customer.Store) (string, domain.Customer, *customer.PostgresStore, *bounceReporterAdapter) {
		t.Helper()
		tenantID := testutil.CreateTestTenant(t, db, name)
		tctx := auth.WithTenantID(ctx, tenantID)
		store := customer.NewPostgresStore(db)
		store.SetBlinder(blinder)
		store.SetOutboxEnqueuer(downOutbox{})
		svc := customer.NewService(wrap(store))
		c, err := svc.Create(tctx, tenantID, customer.CreateInput{
			ExternalID: "bounce-log", DisplayName: "Bounce Log Co", Email: addr,
		})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		return tenantID, c, store, &bounceReporterAdapter{blinder: blinder, store: store, svc: svc}
	}

	t.Run("event insert fails: the lost bounce is logged at ERROR", func(t *testing.T) {
		tenantID, c, store, a := newRig(t, "Bounce Log Insert Fails", func(s *customer.PostgresStore) customer.Store { return s })
		logs := captureSlog(t)

		a.ReportBounce(ctx, tenantID, addr, "550 5.1.1 mailbox does not exist")

		got, err := store.Get(auth.WithTenantID(ctx, tenantID), tenantID, c.ID)
		if err != nil {
			t.Fatalf("get customer: %v", err)
		}
		if got.EmailStatus != domain.EmailStatusUnknown {
			t.Fatalf("email_status %s, want %s: the event insert failed, so the bounce rolled back with it", got.EmailStatus, domain.EmailStatusUnknown)
		}
		out := logs.String()
		for _, want := range []string{
			"level=ERROR",
			"tenant_id=" + tenantID,
			"customer_id=" + c.ID,
			"event_type=" + domain.EventCustomerEmailBounced,
			errBounceOutboxDown.Error(),
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q: nothing redelivers an SMTP bounce, so this line is its only record\nlog:\n%s", want, out)
			}
		}
	})

	t.Run("customer deleted before the write: nothing is logged", func(t *testing.T) {
		tenantID, _, _, a := newRig(t, "Bounce Log Customer Gone", func(s *customer.PostgresStore) customer.Store {
			return &deleteBeforeBounce{PostgresStore: s, db: db}
		})
		logs := captureSlog(t)

		a.ReportBounce(ctx, tenantID, addr, "550 5.1.1 mailbox does not exist")

		if out := logs.String(); strings.Contains(out, "level=ERROR") {
			t.Errorf("a customer deleted after the lookup is not a lost bounce, yet ERROR was logged:\n%s", out)
		}
	})
}
