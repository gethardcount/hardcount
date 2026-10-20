package customer_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestAdditionalEmails_StoreRoundTripEncrypted (ADR-082): the CC list
// round-trips through create/get/update, and the raw column holds
// ciphertext when the encryptor is configured — a DB dump must not leak
// the addresses the sibling email column protects.
func TestAdditionalEmails_StoreRoundTripEncrypted(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	store.SetEncryptor(enc)

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "CC RoundTrip")

	cc := []string{"finance@acme.test", "eng-lead@acme.test"}
	c, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cc", DisplayName: "CC Co", Email: "ap@acme.test",
		AdditionalEmails: cc,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, tenantID, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got.AdditionalEmails, cc) {
		t.Errorf("round trip: got %v, want %v", got.AdditionalEmails, cc)
	}

	// Raw column is ciphertext — no plaintext address in a dump.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var raw string
	if err := tx.QueryRowContext(ctx,
		`SELECT additional_emails FROM customers WHERE id = $1`, c.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if strings.Contains(raw, "finance@acme.test") {
		t.Error("additional_emails stored in PLAINTEXT with an encryptor configured — dump-leaks the PII the email column protects")
	}

	// Update replaces; empty list clears.
	got.AdditionalEmails = []string{"only@acme.test"}
	if _, err := store.Update(ctx, tenantID, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := store.Get(ctx, tenantID, c.ID)
	if !reflect.DeepEqual(after.AdditionalEmails, []string{"only@acme.test"}) {
		t.Errorf("after update: got %v", after.AdditionalEmails)
	}
	after.AdditionalEmails = nil
	if _, err := store.Update(ctx, tenantID, after); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, _ := store.Get(ctx, tenantID, c.ID)
	if len(cleared.AdditionalEmails) != 0 {
		t.Errorf("after clear: got %v, want empty", cleared.AdditionalEmails)
	}
}

// TestAdditionalEmails_ServiceValidation is the normalization matrix:
// cap, primary-collision, display-name refusal, CRLF rejection,
// case-dedupe, and the tri-state Update pointer semantics.
func TestAdditionalEmails_ServiceValidation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	svc := customer.NewService(store)

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "CC Validation")

	mk := func(ext string, cc []string) (domain.Customer, error) {
		return svc.Create(ctx, tenantID, customer.CreateInput{
			ExternalID: ext, DisplayName: "V Co", Email: "primary@acme.test",
			AdditionalEmails: cc,
		})
	}

	// 11 entries → 422.
	over := make([]string, 11)
	for i := range over {
		over[i] = strings.ToLower("a" + string(rune('a'+i)) + "@acme.test")
	}
	if _, err := mk("cus_v1", over); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("11 entries: got %v, want validation error", err)
	}

	// Entry equal to primary → 422.
	if _, err := mk("cus_v2", []string{"Primary@Acme.Test"}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("primary collision: got %v, want validation error", err)
	}

	// CRLF payload → 422 (invalid address).
	if _, err := mk("cus_v3", []string{"evil@x.test\r\nBcc: victim@y.test"}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("CRLF payload: got %v, want validation error", err)
	}

	// Display-name form → 422: an entry must be one bare address.
	if _, err := mk("cus_v5", []string{"Bob <bob@acme.test>"}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("display-name entry: got %v, want validation error", err)
	}

	// Bare addresses → trimmed and lowercased; case-duplicates collapse.
	c, err := mk("cus_v4", []string{"Bob@Acme.Test", "bob@acme.test", "  Fin@Acme.Test "})
	if err != nil {
		t.Fatalf("create with mixed-case addresses: %v", err)
	}
	if !reflect.DeepEqual(c.AdditionalEmails, []string{"bob@acme.test", "fin@acme.test"}) {
		t.Errorf("normalization: got %v, want [bob@acme.test fin@acme.test]", c.AdditionalEmails)
	}

	// Update tri-state: nil leaves as-is, empty slice clears.
	if u, err := svc.Update(ctx, tenantID, c.ID, customer.UpdateInput{DisplayName: "V Co 2"}); err != nil {
		t.Fatalf("update nil: %v", err)
	} else if len(u.AdditionalEmails) != 2 {
		t.Errorf("nil pointer must leave the list untouched, got %v", u.AdditionalEmails)
	}
	empty := []string{}
	if u, err := svc.Update(ctx, tenantID, c.ID, customer.UpdateInput{AdditionalEmails: &empty}); err != nil {
		t.Fatalf("update clear: %v", err)
	} else if len(u.AdditionalEmails) != 0 {
		t.Errorf("explicit [] must clear, got %v", u.AdditionalEmails)
	}

	// Changing the primary onto a stored CC address → 422 pointing at
	// the collision, not a silent list rewrite.
	list := []string{"future@acme.test"}
	if _, err := svc.Update(ctx, tenantID, c.ID, customer.UpdateInput{AdditionalEmails: &list}); err != nil {
		t.Fatalf("seed cc: %v", err)
	}
	if _, err := svc.Update(ctx, tenantID, c.ID, customer.UpdateInput{Email: "future@acme.test"}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("primary onto stored CC: got %v, want validation error", err)
	}
}
