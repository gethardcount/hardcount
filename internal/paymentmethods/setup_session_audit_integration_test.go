package paymentmethods

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// sessionStripe is a StripeAPI stand-in for the setup-session path only, with
// injectable failure on the one call that mints the capability.
type sessionStripe struct {
	sessions   int
	sessionErr error
}

func (s *sessionStripe) CreateSetupCheckoutSession(_ context.Context, _, _, _ string, _ map[string]string) (string, string, error) {
	if s.sessionErr != nil {
		return "", "", s.sessionErr
	}
	s.sessions++
	return "https://checkout.stripe.com/c/pay/cs_test", "cs_test_session", nil
}

func (s *sessionStripe) EnsureStripeCustomer(context.Context, string, string) (string, error) {
	return "cus_stripe_session", nil
}
func (s *sessionStripe) DetachPaymentMethod(context.Context, string) error { return nil }
func (s *sessionStripe) SetDefaultPaymentMethod(context.Context, string, string, string) error {
	return nil
}
func (s *sessionStripe) FetchPaymentMethodCard(context.Context, string) (CardMetadata, error) {
	return CardMetadata{}, errors.New("not used")
}

// erroringAudit fails every write — the residual-own-tx analogue of an emit
// failure. There is no tx to roll back here (the mutation is EXTERNAL: the
// Stripe session), so the fail-closed direction is that the ERROR REACHES THE
// CALLER and the capability URL is never handed out.
type erroringAudit struct{ calls int }

func (e *erroringAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	e.calls++
	return errors.New("injected audit failure")
}

// TestCreateSetupSessionAudit pins the ADR-090 emission on
// POST /v1/customers/{id}/payment-methods/setup-session — the operator minting
// a card-capture capability link. Its only audit trace used to be the deleted
// middleware catch-all's guessed row; this emission is now the record.
//
// Invariants:
//
//  1. a minted session emits exactly one setup_session_created row
//     (update/customer), carrying the Stripe session id;
//  2. the SECOND link for the SAME customer emits again — the row records the
//     capability grant, NOT the incidental customers.stripe_customer_id write
//     inside EnsureStripeCustomer, which only ever happens on the first call.
//     Gating on that write would silently un-audit every subsequent link;
//  3. an audit-write failure PROPAGATES: no URL is returned, so a capability
//     the compliance log could not record is never handed to the operator;
//  4. a FAILED Stripe mint emits nothing — no evidence of a link that does not
//     exist.
//
// Mutation-verify: gate the emission on the mapping write and (2) fails;
// swallow the Log error (`_ =`) and (3) fails; move the emission above the
// Stripe call and (4) fails.
func TestCreateSetupSessionAudit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Setup Session Audit")
	logger := audit.NewLogger(db)

	sessionRows := func(t *testing.T, custID string) []domain.AuditEntry {
		t.Helper()
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "customer", ResourceID: custID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		var out []domain.AuditEntry
		for _, r := range rows {
			if r.Metadata["action"] == "setup_session_created" {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("minted session emits one row", func(t *testing.T) {
		custID := postgres.NewID("vlx_cus")
		svc := NewService(newMemStore(), &sessionStripe{}, nil)
		svc.SetAuditLogger(logger)

		url, sessionID, err := svc.CreateSetupSession(ctx, tenantID, custID, "")
		if err != nil {
			t.Fatalf("create setup session: %v", err)
		}
		if url == "" || sessionID != "cs_test_session" {
			t.Fatalf("session not minted: url=%q id=%q", url, sessionID)
		}

		rows := sessionRows(t, custID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one setup_session_created row; got %d: %+v", len(rows), rows)
		}
		r := rows[0]
		if r.Action != domain.AuditActionUpdate || r.ResourceType != "customer" || r.ResourceID != custID {
			t.Errorf("row vocabulary = %s/%s/%s, want update/customer/%s", r.Action, r.ResourceType, r.ResourceID, custID)
		}
		if r.Metadata["session_id"] != "cs_test_session" {
			t.Errorf("session_id in metadata = %v, want cs_test_session", r.Metadata["session_id"])
		}
	})

	t.Run("second link for the same customer emits again", func(t *testing.T) {
		custID := postgres.NewID("vlx_cus")
		svc := NewService(newMemStore(), &sessionStripe{}, nil)
		svc.SetAuditLogger(logger)

		for i := 0; i < 2; i++ {
			if _, _, err := svc.CreateSetupSession(ctx, tenantID, custID, ""); err != nil {
				t.Fatalf("create setup session %d: %v", i, err)
			}
		}
		if rows := sessionRows(t, custID); len(rows) != 2 {
			t.Fatalf("want 2 rows (one per capability grant); got %d — the emission is gated on the first-call-only mapping write", len(rows))
		}
	})

	t.Run("audit failure propagates and returns no URL", func(t *testing.T) {
		custID := postgres.NewID("vlx_cus")
		failing := &erroringAudit{}
		svc := NewService(newMemStore(), &sessionStripe{}, nil)
		svc.SetAuditLogger(failing)

		url, _, err := svc.CreateSetupSession(ctx, tenantID, custID, "")
		if err == nil {
			t.Fatal("an unrecordable capability grant must fail, not be handed to the operator")
		}
		if url != "" {
			t.Errorf("url returned despite a failed audit write: %q", url)
		}
		if failing.calls != 1 {
			t.Errorf("audit calls = %d, want 1", failing.calls)
		}
		if rows := sessionRows(t, custID); len(rows) != 0 {
			t.Errorf("audit rows exist despite the failed write: %+v", rows)
		}
	})

	t.Run("failed Stripe mint emits nothing", func(t *testing.T) {
		custID := postgres.NewID("vlx_cus")
		stripe := &sessionStripe{sessionErr: errors.New("stripe down")}
		svc := NewService(newMemStore(), stripe, nil)
		svc.SetAuditLogger(logger)

		if _, _, err := svc.CreateSetupSession(ctx, tenantID, custID, ""); err == nil {
			t.Fatal("a failed Stripe mint must return an error")
		}
		if rows := sessionRows(t, custID); len(rows) != 0 {
			t.Errorf("audit row recorded for a capability link that was never minted: %+v", rows)
		}
	})
}
