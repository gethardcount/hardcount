package tenant_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// The setup keys are minted by auth, like every other key: each has a fresh
// 16-byte salt, its stored hash is SHA-256(salt || key), and it validates.
func TestBootstrap_KeysAreMintedLikeEveryKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	logger := audit.NewLogger(db)
	keys := auth.NewService(db, logger)
	res, err := tenant.RunBootstrap(ctx, db, tenant.BootstrapDeps{
		HashPassword:  user.HashPassword,
		CreateUserTx:  user.NewPostgresStore(db).CreateInTx,
		CreateKeyTx:   keys.CreateKeyTx,
		SeedDunningTx: dunning.NewService(dunning.NewPostgresStore(db), nil, nil).SeedDefaultPoliciesTx,
		Audit:         logger,
	}, tenant.BootstrapOpts{TenantName: "Setup Keys Co", OwnerEmail: "owner@setup-keys.test"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	for _, raw := range []string{res.TestSecretKey, res.LiveSecretKey} {
		key, err := keys.ValidateKey(ctx, raw)
		if err != nil {
			t.Fatalf("validate %s…: %v", raw[:15], err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		var hash, salt string
		err = tx.QueryRow(`SELECT key_hash, key_salt FROM api_keys WHERE id = $1`, key.ID).Scan(&hash, &salt)
		postgres.Rollback(tx)
		if err != nil {
			t.Fatalf("read key %s: %v", key.ID, err)
		}
		saltBytes, err := hex.DecodeString(salt)
		if err != nil || len(saltBytes) != 16 {
			t.Errorf("%s…: stored salt %q, want 16 random bytes in hex", raw[:15], salt)
			continue
		}
		sum := sha256.Sum256(append(saltBytes, raw...))
		if hash != hex.EncodeToString(sum[:]) {
			t.Errorf("%s…: the stored hash is not SHA-256(salt || key)", raw[:15])
		}
	}
}
