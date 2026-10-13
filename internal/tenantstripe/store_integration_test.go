package tenantstripe

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestStore_TenantAndModeScoping_RealPostgres runs the store's queries against
// a real database. The store opens every transaction with RLS bypassed and
// scopes rows with hand-written tenant_id / livemode predicates, so those
// predicates are the only thing keeping one tenant's (or one mode's) Stripe
// keys apart from another's. Three rows are seeded — tenant A test, tenant A
// live, tenant B test — each with its own key strings, so a read or write
// that reaches the wrong row shows up as the wrong string.
//
// All key strings are obviously fake and carry only the real prefix.
func TestStore_TenantAndModeScoping_RealPostgres(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx, cancel := context.WithTimeout(testutil.TestCtx(), 30*time.Second)
	defer cancel()

	// The real AES-GCM encryptor, not the no-op default: the at-rest
	// assertions below are only meaningful with encryption on.
	enc, err := crypto.NewEncryptor(strings.Repeat("0123456789abcdef", 4))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	store := NewStore(db)
	store.SetEncryptor(enc)

	tenantA := testutil.CreateTestTenant(t, db, "Tenant Stripe A")
	tenantB := testutil.CreateTestTenant(t, db, "Tenant Stripe B")

	const (
		aTestSK = "sk_test_FAKE_tenant_a_test_AT01"
		aTestPK = "pk_test_FAKE_tenant_a_test"
		aTestWH = "whsec_FAKE_tenant_a_test_AW01"
		aLiveSK = "sk_live_FAKE_tenant_a_live_AL01"
		aLivePK = "pk_live_FAKE_tenant_a_live"
		bTestSK = "sk_test_FAKE_tenant_b_test_BT01"
		bTestPK = "pk_test_FAKE_tenant_b_test"
		bTestWH = "whsec_FAKE_tenant_b_test_BW01"
	)

	// --- seed: two modes for tenant A, one row for tenant B ---

	aTest, err := store.Upsert(ctx, UpsertInput{
		TenantID: tenantA, Livemode: false,
		SecretKey: aTestSK, PublishableKey: aTestPK, WebhookSecret: aTestWH,
	})
	if err != nil {
		t.Fatalf("upsert A test: %v", err)
	}
	// No webhook secret: the insert must leave the column NULL.
	aLive, err := store.Upsert(ctx, UpsertInput{
		TenantID: tenantA, Livemode: true,
		SecretKey: aLiveSK, PublishableKey: aLivePK,
	})
	if err != nil {
		t.Fatalf("upsert A live: %v", err)
	}
	bTest, err := store.Upsert(ctx, UpsertInput{
		TenantID: tenantB, Livemode: false,
		SecretKey: bTestSK, PublishableKey: bTestPK, WebhookSecret: bTestWH,
	})
	if err != nil {
		t.Fatalf("upsert B test: %v", err)
	}

	if aTest.ID == aLive.ID || aTest.ID == bTest.ID || aLive.ID == bTest.ID {
		t.Fatalf("want three distinct rows, got ids %q %q %q", aTest.ID, aLive.ID, bTest.ID)
	}
	if aTest.TenantID != tenantA || aTest.Livemode {
		t.Errorf("A test row = (%q, livemode=%v), want (%q, false)", aTest.TenantID, aTest.Livemode, tenantA)
	}
	if aLive.TenantID != tenantA || !aLive.Livemode {
		t.Errorf("A live row = (%q, livemode=%v), want (%q, true)", aLive.TenantID, aLive.Livemode, tenantA)
	}
	// The public view carries display fragments only.
	if aTest.SecretKeyPrefix != "sk_test_FAKE" || aTest.SecretKeyLast4 != "AT01" {
		t.Errorf("A test public view prefix/last4 = %q/%q, want sk_test_FAKE/AT01", aTest.SecretKeyPrefix, aTest.SecretKeyLast4)
	}
	if !aTest.HasWebhookSecret || aTest.WebhookSecretLast4 != "AW01" {
		t.Errorf("A test webhook view = (has=%v, last4=%q), want (true, AW01)", aTest.HasWebhookSecret, aTest.WebhookSecretLast4)
	}
	if aLive.HasWebhookSecret || aLive.WebhookSecretLast4 != "" {
		t.Errorf("A live webhook view = (has=%v, last4=%q), want (false, \"\")", aLive.HasWebhookSecret, aLive.WebhookSecretLast4)
	}

	// --- at rest: the stored value is not the plaintext ---

	rawSK, rawWH := rawSecrets(t, admin, aTest.ID)
	if rawSK == aTestSK || strings.Contains(rawSK, aTestSK) {
		t.Errorf("secret key is stored in plaintext: %q", rawSK)
	}
	if !strings.HasPrefix(rawSK, "enc:") {
		t.Errorf("stored secret key = %q, want an enc: envelope", rawSK)
	}
	if !rawWH.Valid || rawWH.String == aTestWH || strings.Contains(rawWH.String, aTestWH) || !strings.HasPrefix(rawWH.String, "enc:") {
		t.Errorf("stored webhook secret = %+v, want a non-plaintext enc: envelope", rawWH)
	}
	if _, liveWH := rawSecrets(t, admin, aLive.ID); liveWH.Valid {
		t.Errorf("A live webhook secret column = %q, want NULL (none was supplied)", liveWH.String)
	}

	// --- read: each (tenant, mode) returns its own plaintext ---

	wantPlain := func(step, tenantID string, livemode bool, wantSK, wantWH string) {
		t.Helper()
		got, err := store.GetPlaintext(ctx, tenantID, livemode)
		if err != nil {
			t.Fatalf("%s: GetPlaintext(%s, livemode=%v): %v", step, tenantID, livemode, err)
		}
		if got.SecretKey != wantSK {
			t.Errorf("%s: (%s, livemode=%v) secret key = %q, want %q", step, tenantID, livemode, got.SecretKey, wantSK)
		}
		if got.WebhookSecret != wantWH {
			t.Errorf("%s: (%s, livemode=%v) webhook secret = %q, want %q", step, tenantID, livemode, got.WebhookSecret, wantWH)
		}
	}
	wantPlain("seed", tenantA, false, aTestSK, aTestWH)
	wantPlain("seed", tenantA, true, aLiveSK, "")
	wantPlain("seed", tenantB, false, bTestSK, bTestWH)

	// Tenant B has no live row; tenant A's live row must not answer for it.
	if _, err := store.GetPlaintext(ctx, tenantB, true); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetPlaintext(B, live) err = %v, want ErrNotFound", err)
	}

	listA, err := store.ListByTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 2 || listA[0].ID != aLive.ID || listA[1].ID != aTest.ID {
		t.Errorf("list A = %+v, want [A live, A test]", listA)
	}
	listB, err := store.ListByTenant(ctx, tenantB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != bTest.ID {
		t.Errorf("list B = %+v, want [B test]", listB)
	}

	// Endpoint lookup: tenant and mode come from the row, and a row with no
	// webhook secret is reported as not found.
	ep, err := store.GetByID(ctx, aTest.ID)
	if err != nil {
		t.Fatalf("GetByID(A test): %v", err)
	}
	if ep.TenantID != tenantA || ep.Livemode || ep.WebhookSecret != aTestWH {
		t.Errorf("GetByID(A test) = (%q, livemode=%v, %q), want (%q, false, %q)", ep.TenantID, ep.Livemode, ep.WebhookSecret, tenantA, aTestWH)
	}
	if _, err := store.GetByID(ctx, aLive.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetByID(A live, no webhook secret) err = %v, want ErrNotFound", err)
	}

	// --- update, keep branch: rotating API keys without a webhook secret
	// keeps the stored one ---

	const aTestSK2 = "sk_test_FAKE_tenant_a_test_AT02"
	rotated, err := store.Upsert(ctx, UpsertInput{
		TenantID: tenantA, Livemode: false,
		SecretKey: aTestSK2, PublishableKey: aTestPK,
	})
	if err != nil {
		t.Fatalf("rotate A test keys: %v", err)
	}
	if rotated.ID != aTest.ID {
		t.Errorf("rotation created row %q, want the existing row %q updated", rotated.ID, aTest.ID)
	}
	if !rotated.HasWebhookSecret || rotated.WebhookSecretLast4 != "AW01" {
		t.Errorf("after rotation webhook view = (has=%v, last4=%q), want the earlier secret kept (true, AW01)", rotated.HasWebhookSecret, rotated.WebhookSecretLast4)
	}
	wantPlain("keep", tenantA, false, aTestSK2, aTestWH)
	wantPlain("keep", tenantA, true, aLiveSK, "")
	wantPlain("keep", tenantB, false, bTestSK, bTestWH)

	// --- update, null branch: re-saving a row that never had a webhook
	// secret, again without one, leaves the column NULL ---

	const aLiveSK2 = "sk_live_FAKE_tenant_a_live_AL02"
	resaved, err := store.Upsert(ctx, UpsertInput{
		TenantID: tenantA, Livemode: true,
		SecretKey: aLiveSK2, PublishableKey: aLivePK,
	})
	if err != nil {
		t.Fatalf("re-save A live keys: %v", err)
	}
	if resaved.ID != aLive.ID {
		t.Errorf("re-save created row %q, want the existing row %q updated", resaved.ID, aLive.ID)
	}
	if resaved.HasWebhookSecret {
		t.Errorf("A live reports a webhook secret after a re-save that supplied none")
	}
	if _, liveWH := rawSecrets(t, admin, aLive.ID); liveWH.Valid {
		t.Errorf("A live webhook secret column = %q after re-save, want NULL", liveWH.String)
	}
	wantPlain("null", tenantA, true, aLiveSK2, "")
	wantPlain("null", tenantA, false, aTestSK2, aTestWH)
	wantPlain("null", tenantB, false, bTestSK, bTestWH)

	// --- update, replace: a supplied webhook secret overwrites the stored one ---

	const aTestWH2 = "whsec_FAKE_tenant_a_test_AW02"
	if _, err := store.Upsert(ctx, UpsertInput{
		TenantID: tenantA, Livemode: false,
		SecretKey: aTestSK2, PublishableKey: aTestPK, WebhookSecret: aTestWH2,
	}); err != nil {
		t.Fatalf("replace A test webhook secret: %v", err)
	}
	wantPlain("replace", tenantA, false, aTestSK2, aTestWH2)
	wantPlain("replace", tenantB, false, bTestSK, bTestWH)

	// --- SetWebhookSecret touches only its own (tenant, mode) ---

	const aLiveWH = "whsec_FAKE_tenant_a_live_AW03"
	set, err := store.SetWebhookSecret(ctx, tenantA, true, aLiveWH)
	if err != nil {
		t.Fatalf("set A live webhook secret: %v", err)
	}
	if set.ID != aLive.ID || !set.HasWebhookSecret || set.WebhookSecretLast4 != "AW03" {
		t.Errorf("SetWebhookSecret returned (%q, has=%v, last4=%q), want (%q, true, AW03)", set.ID, set.HasWebhookSecret, set.WebhookSecretLast4, aLive.ID)
	}
	// SetWebhookSecret is the second writer of the encrypted column: what
	// it stores must be ciphertext, not the secret it was given.
	if _, rawWH := rawSecrets(t, admin, aLive.ID); !rawWH.Valid || !strings.HasPrefix(rawWH.String, "enc:") || strings.Contains(rawWH.String, aLiveWH) {
		t.Errorf("SetWebhookSecret stored %q: want an enc: value that does not contain the secret", rawWH.String)
	}
	wantPlain("set webhook", tenantA, true, aLiveSK2, aLiveWH)
	wantPlain("set webhook", tenantA, false, aTestSK2, aTestWH2)
	wantPlain("set webhook", tenantB, false, bTestSK, bTestWH)
	if _, err := store.SetWebhookSecret(ctx, tenantB, true, aLiveWH); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("SetWebhookSecret(B, live) with no such row: err = %v, want ErrNotFound", err)
	}

	// --- verify status is recorded on its own (tenant, mode) only ---

	// Both writes target A test: it shares a tenant with A live and a mode
	// with B test, so a write that drops either predicate lands on one of
	// those two rows.
	wantVerify := func(step, wantAccount string, wantVerified bool, wantErr string) {
		t.Helper()
		listA, err := store.ListByTenant(ctx, tenantA)
		if err != nil {
			t.Fatalf("%s: list A: %v", step, err)
		}
		listB, err := store.ListByTenant(ctx, tenantB)
		if err != nil {
			t.Fatalf("%s: list B: %v", step, err)
		}
		if len(listA) != 2 || len(listB) != 1 {
			t.Fatalf("%s: got %d rows for A and %d for B, want 2 and 1", step, len(listA), len(listB))
		}
		live, test, other := listA[0], listA[1], listB[0]
		if test.StripeAccountID != wantAccount || (test.VerifiedAt != nil) != wantVerified || test.LastVerifiedError != wantErr {
			t.Errorf("%s: A test verify state = (%q, verified=%v, err=%q), want (%q, %v, %q)", step,
				test.StripeAccountID, test.VerifiedAt != nil, test.LastVerifiedError, wantAccount, wantVerified, wantErr)
		}
		if live.StripeAccountID != "" || live.VerifiedAt != nil || live.LastVerifiedError != "" {
			t.Errorf("%s: A live verify state changed by a write to A test: (%q, verified=%v, err=%q)", step,
				live.StripeAccountID, live.VerifiedAt != nil, live.LastVerifiedError)
		}
		if other.StripeAccountID != "" || other.VerifiedAt != nil || other.LastVerifiedError != "" {
			t.Errorf("%s: B test verify state changed by a write to A test: (%q, verified=%v, err=%q)", step,
				other.StripeAccountID, other.VerifiedAt != nil, other.LastVerifiedError)
		}
	}
	if err := store.MarkVerifyFailed(ctx, tenantA, false, "fake verify failure"); err != nil {
		t.Fatalf("mark A test verify failed: %v", err)
	}
	wantVerify("verify failed", "", false, "fake verify failure")
	if err := store.MarkVerified(ctx, tenantA, false, "acct_FAKE_a_test", "Tenant A Test"); err != nil {
		t.Fatalf("mark A test verified: %v", err)
	}
	wantVerify("verified", "acct_FAKE_a_test", true, "")

	// --- delete removes only its own row ---

	if err := store.Delete(ctx, tenantA, false); err != nil {
		t.Fatalf("delete A test: %v", err)
	}
	if _, err := store.GetPlaintext(ctx, tenantA, false); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetPlaintext(A, test) after delete: err = %v, want ErrNotFound", err)
	}
	wantPlain("delete", tenantA, true, aLiveSK2, aLiveWH)
	wantPlain("delete", tenantB, false, bTestSK, bTestWH)
	if err := store.Delete(ctx, tenantA, false); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("second delete of A test: err = %v, want ErrNotFound", err)
	}
	var remaining int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM stripe_provider_credentials`).Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 2 {
		t.Errorf("rows left after deleting A test = %d, want 2 (A live, B test)", remaining)
	}
}

// rawSecrets reads the two secret columns exactly as stored, through the
// owner connection, so the at-rest assertions don't go through the store's
// own decrypt path.
func rawSecrets(t *testing.T, admin *sql.DB, id string) (string, sql.NullString) {
	t.Helper()
	var secret string
	var webhook sql.NullString
	err := admin.QueryRowContext(context.Background(), `
		SELECT secret_key_encrypted, webhook_secret_encrypted
		FROM stripe_provider_credentials
		WHERE id = $1
	`, id).Scan(&secret, &webhook)
	if err != nil {
		t.Fatalf("read raw secret columns for %s: %v", id, err)
	}
	return secret, webhook
}
