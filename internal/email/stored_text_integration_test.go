package email_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestEmailOutbox_ErrorCutInsideACharacter_IsSaved: a send fails with an
// error whose 500-byte cut lands inside a character. Postgres refuses text
// that is not valid UTF-8 (SQLSTATE 22021), so when the cut split the
// character, every mark that saves last_error failed: the row stayed pending
// and was claimed again after each lease, with no end. Each of the three
// marks now saves the error, cut before the split character.
func TestEmailOutbox_ErrorCutInsideACharacter_IsSaved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox), 30*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Long Error")
	store := email.NewOutboxStore(db)

	// over returns text that, after prefix, puts "é" on bytes 499 and 500,
	// and the part a cut between two characters keeps.
	over := func(prefix string) (text, kept string) {
		pad := strings.Repeat("a", 499-len(prefix))
		return pad + "é and more", prefix + pad
	}
	transient, transientKept := over("")
	permanent, permanentKept := over(email.ErrInvalidRecipient.Error() + ": ")
	obsolete, obsoleteKept := over(email.ErrEmailObsolete.Error() + ": invoice ")

	for _, tc := range []struct {
		name       string
		err        error
		wantStatus string
		wantErr    string
	}{
		{"retry mark", errors.New(transient), email.OutboxPending, transientKept},
		{"failed mark", fmt.Errorf("%w: %s", email.ErrInvalidRecipient, permanent), email.OutboxFailed, permanentKept},
		{"skipped mark", &email.ObsoleteError{InvoiceNumber: obsolete, State: email.SkipReasonPaid}, "skipped", obsoleteKept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := store.EnqueueStandalone(ctx, tenantID, email.TypeInvoice, map[string]any{"to": "x@example.com"})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if _, err := store.ProcessBatch(ctx, 10, func(_ context.Context, row email.OutboxRow) error {
				if row.ID != id {
					return fmt.Errorf("row %s was claimed again", row.ID)
				}
				return tc.err
			}); err != nil {
				t.Fatalf("process batch: %v", err)
			}

			status, _, _ := readEmailOutbox(t, db, id)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q: the mark was not saved", status, tc.wantStatus)
			}
			if lastErr, _ := readEmailOutboxRetry(t, db, id); lastErr != tc.wantErr {
				t.Errorf("last_error = %q, want the first 499 bytes, which end before the split character", lastErr)
			}
		})
	}
}
