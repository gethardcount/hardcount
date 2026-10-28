package invoice_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestPublicToken_HashLookupAndNoPlaintextAtRest covers the low [security]
// audit finding: the hosted-invoice public_token was stored in plaintext, so a
// DB snapshot leak yielded directly-replayable URLs. The token is now stored as
// an AES-GCM ciphertext + a SHA-256 blind index; lookups go through the hash and
// the raw token never hits the DB. Decryption on read still rebuilds the token
// for re-send URLs.
func TestPublicToken_HashLookupAndNoPlaintextAtRest(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32)) // 64 hex chars
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	store.SetEncryptor(enc)

	tenantID := testutil.CreateTestTenant(t, db, "Public Token Hash")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_pt", DisplayName: "PT",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	issued := now
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-PT-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 100, TotalAmountCents: 100, AmountDueCents: 100,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	rawToken, err := invoice.GeneratePublicToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := store.SetPublicToken(ctx, tenantID, inv.ID, rawToken); err != nil {
		t.Fatalf("set public token: %v", err)
	}

	// Resolve by the raw URL token — goes through the hash blind index. The
	// second return is the invoice's livemode (false here); not-found is an err.
	got, livemode, err := store.GetByPublicToken(ctx, rawToken)
	if err != nil {
		t.Fatalf("GetByPublicToken: %v", err)
	}
	if got.ID != inv.ID {
		t.Errorf("resolved invoice: got %q, want %q", got.ID, inv.ID)
	}
	if livemode {
		t.Errorf("livemode: got true, want false (test-mode invoice)")
	}
	// Decryption on read rebuilds the raw token for re-send URLs.
	if got.PublicToken != rawToken {
		t.Errorf("decrypted PublicToken: got %q, want the raw token", got.PublicToken)
	}

	// The DB must hold NO plaintext token: the encrypted column is ciphertext
	// (enc: prefix, != raw) and the hash column is the SHA-256 of the raw.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var encrypted, hash string
	if err := tx.QueryRow(`
		SELECT COALESCE(public_token_encrypted,''), COALESCE(public_token_hash,'')
		FROM invoices WHERE id = $1
	`, inv.ID).Scan(&encrypted, &hash); err != nil {
		t.Fatalf("read token columns: %v", err)
	}
	if encrypted == "" || encrypted == rawToken {
		t.Errorf("public_token_encrypted must be ciphertext, not the raw token (got %q)", encrypted)
	}
	if !strings.HasPrefix(encrypted, "enc:") {
		t.Errorf("expected AES-GCM ciphertext (enc: prefix), got %q", encrypted)
	}
	if hash != invoice.HashPublicToken(rawToken) {
		t.Errorf("public_token_hash mismatch: got %q, want %q", hash, invoice.HashPublicToken(rawToken))
	}
	if strings.Contains(hash, rawToken) {
		t.Error("hash must not contain the raw token")
	}
}
