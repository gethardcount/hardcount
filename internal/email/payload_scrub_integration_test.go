package email_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestProcessBatch_TerminalMarksScrubPayload locks the at-rest rule for
// email_outbox.payload: a row that will never be sent again (dispatched,
// skipped) drops its link tokens, URLs and PDF bytes in the mark itself,
// and keeps the keys the timeline, the Sent-emails panel and the Postmark
// webhook read. A row that can still be sent (retrying, or failed and
// awaiting a manual re-send) keeps its whole payload.
func TestProcessBatch_TerminalMarksScrubPayload(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Scrub")
	store := email.NewOutboxStore(db)

	// Every key the producers can write (outboxMessage's JSON tags), so the
	// test also shows which keys survive.
	fullPayload := func() map[string]any {
		return map[string]any{
			"to":                 "customer@example.com",
			"cc":                 []string{"ap@example.com"},
			"customer_name":      "Ada Lovelace",
			"invoice_number":     "VLX-000042",
			"credit_note_number": "CN-000007",
			"amount_cents":       12345,
			"currency":           "USD",
			"trigger":            "operator_resend",
			"attempt_number":     2,
			"max_attempts":       4,
			"setup_url":          "https://checkout.example/c/cs_test_secret",
			"update_url":         "https://app.example/update-payment?token=raw-update-token",
			"password_reset_url": "https://app.example/reset?token=raw-reset-token",
			"invite_url":         "https://app.example/invite?token=raw-invite-token",
			"public_token":       "vlx_pinv_rawpublictoken",
			"pdf":                "JVBERi0xLjQK",
		}
	}
	scrubbed := []string{"setup_url", "update_url", "password_reset_url", "invite_url", "public_token", "pdf"}
	kept := []string{"to", "cc", "customer_name", "invoice_number", "credit_note_number",
		"amount_cents", "currency", "trigger", "attempt_number", "max_attempts"}

	enqueue := func(emailType string) string {
		t.Helper()
		id, err := store.EnqueueStandalone(ctx, tenantID, emailType, fullPayload())
		if err != nil {
			t.Fatalf("enqueue %s: %v", emailType, err)
		}
		return id
	}
	dispatchedID := enqueue(email.TypeInvoice)
	skippedID := enqueue(email.TypePaymentSetupRequest)
	failedID := enqueue(email.TypePasswordReset)
	retryingID := enqueue(email.TypeMemberInvite)

	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, row email.OutboxRow) error {
		switch row.ID {
		case dispatchedID:
			return nil
		case skippedID:
			return email.ErrEmailObsolete
		case failedID:
			return email.ErrPayloadDecode // permanent: straight to 'failed'
		default:
			return errors.New("smtp temp failure")
		}
	})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if n != 4 {
		t.Fatalf("attempted: got %d, want 4", n)
	}

	for _, tc := range []struct {
		name       string
		id         string
		wantStatus string
		wantScrub  bool
	}{
		{"dispatched", dispatchedID, email.OutboxDispatched, true},
		{"skipped", skippedID, "skipped", true},
		{"failed", failedID, email.OutboxFailed, false},
		{"retrying", retryingID, email.OutboxPending, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, payload := readEmailOutbox(t, db, tc.id)
			if status != tc.wantStatus {
				t.Fatalf("status: got %q, want %q", status, tc.wantStatus)
			}
			for _, k := range kept {
				if _, ok := payload[k]; !ok {
					t.Errorf("key %q must survive the mark (payload: %v)", k, payload)
				}
			}
			for _, k := range scrubbed {
				_, present := payload[k]
				if tc.wantScrub && present {
					t.Errorf("key %q still stored on a %s row", k, tc.name)
				}
				if !tc.wantScrub && !present {
					t.Errorf("key %q removed from a %s row — it is needed to send again", k, tc.name)
				}
			}
		})
	}

	// The readers of sent rows still work on a scrubbed payload.
	rows, err := store.ListByInvoice(ctx, tenantID, "VLX-000042")
	if err != nil {
		t.Fatalf("list by invoice: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID != dispatchedID {
			continue
		}
		found = true
		if to, _ := r.Payload["to"].(string); to != "customer@example.com" {
			t.Errorf("timeline recipient: got %q", to)
		}
	}
	if !found {
		t.Error("dispatched row no longer found by invoice_number")
	}
}
