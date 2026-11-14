package tenant

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// RunBootstrap is the ONLY code path that provisions a tenant (ADR-073).
// The HTTP handler and cmd/hardcount-bootstrap are thin callers — the two
// writers had already drifted apart once (HTTP minted no owner user, no
// live key, no settings row: a dashboard dead-end), so the shared
// sequence is the fix, not doc'd parity.
//
// Everything runs in ONE bypass transaction under
// pg_advisory_xact_lock(LockKeyBootstrap):
//
//	guards (first-tenant / owner-email uniqueness, authoritative under
//	the lock) → tenant → tenant_settings seed → 3 API keys → owner user
//	→ audit rows → default dunning policy per mode
//
// so a failure anywhere leaves ZERO rows behind. The half-bootstrap
// shape this kills: tenant+keys committed, owner-user create fails on
// password validation, every retry 409s "already bootstrapped", and the
// operator is locked out of the dashboard forever. All inputs validate
// BEFORE the first write for the same reason.

// ErrAlreadyBootstrapped is returned when FirstTenantOnly is set and a
// tenant already exists. HTTP maps it to 409.
var ErrAlreadyBootstrapped = errs.InvalidState("bootstrap already completed — tenants exist").WithCode("already_bootstrapped")

// ErrOwnerEmailExists is returned when the owner email already has an
// account. The CLI prints re-run guidance on it; HTTP maps it to 409.
var ErrOwnerEmailExists = errs.AlreadyExists("owner_email", "an account with this email already exists")

// BootstrapDeps are the cross-domain seams RunBootstrap needs. The
// tenant package must not import its peer package user, so callers
// wire these (router.go and cmd/hardcount-bootstrap both pass
// user.HashPassword + user.PostgresStore.CreateInTx).
type BootstrapDeps struct {
	// HashPassword validates the plaintext (length ≥ user.MinPasswordLength,
	// bcrypt's 72-byte cap, denylist) and returns the bcrypt hash. It is
	// called BEFORE any write so validation failures cannot half-bootstrap.
	HashPassword func(plaintext string) (string, error)
	// CreateUserTx inserts the owner user + tenant attachment inside the
	// bootstrap tx.
	CreateUserTx func(ctx context.Context, tx *sql.Tx, email, passwordHash, tenantID, role string) (domain.User, error)
	// CreateKeyTx mints an API key and writes its audit row inside the
	// bootstrap tx (auth.Service.CreateKeyTx), so the setup keys have the
	// same shape, salted hash and audit row as every other key. Required.
	CreateKeyTx func(ctx context.Context, tx *sql.Tx, tenantID string, input auth.CreateKeyInput) (auth.CreateKeyResult, error)
	// SeedDunningTx writes the account's default dunning policy in each
	// mode inside the bootstrap tx (ADR-123). Required: an account created
	// without it would never retry a failed payment.
	SeedDunningTx DunningSeedFunc
	// Audit records the provisioning inside the bootstrap tx (ADR-090).
	// Optional (nil => no rows), but the composition root always wires it:
	// bootstrap mints a LIVE secret key and an owner account, and "where did
	// this live key come from?" is precisely an audit question.
	Audit AuditEmitter
}

// BootstrapOpts parameterize one bootstrap run.
type BootstrapOpts struct {
	TenantName    string // default "Demo Tenant"
	OwnerEmail    string // default admin@hardcount.local
	OwnerPassword string // empty = generate a 96-bit one, returned once
	// FirstTenantOnly refuses when ANY tenant exists — the HTTP
	// endpoint's one-shot semantics. The CLI passes false: additional
	// tenants in the same deployment are a supported dev workflow,
	// guarded per-run by owner-email uniqueness instead.
	FirstTenantOnly bool
}

// BootstrapResult carries everything the caller prints or returns —
// the raw keys and the (possibly generated) owner password appear here
// exactly once and are not retrievable afterwards.
type BootstrapResult struct {
	Tenant            domain.Tenant
	OwnerUser         domain.User
	OwnerPassword     string
	PasswordGenerated bool
	TestSecretKey     string
	LiveSecretKey     string
}

func RunBootstrap(ctx context.Context, db *postgres.DB, deps BootstrapDeps, opts BootstrapOpts) (BootstrapResult, error) {
	if deps.HashPassword == nil || deps.CreateUserTx == nil || deps.CreateKeyTx == nil || deps.SeedDunningTx == nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap deps not wired")
	}

	tenantName := strings.TrimSpace(opts.TenantName)
	if tenantName == "" {
		tenantName = "Demo Tenant"
	}
	ownerEmail := strings.TrimSpace(opts.OwnerEmail)
	if ownerEmail == "" {
		ownerEmail = "admin@hardcount.local"
	}
	// The owner signs in with this address and password-reset emails go
	// to it, so it gets the same check as every other address.
	ownerEmail, err := domain.ValidateEmail("owner_email", ownerEmail)
	if err != nil {
		return BootstrapResult{}, err
	}

	ownerPassword := opts.OwnerPassword
	generated := false
	if ownerPassword == "" {
		ownerPassword = generatePassword()
		generated = true
	}
	// Validate + hash BEFORE any write. user.HashPassword rejects
	// passwords under MinPasswordLength (12) and over bcrypt's 72-byte
	// cap — those failures must surface as a clean 422/CLI error with
	// zero rows written, never after a tenant has committed.
	passwordHash, err := deps.HashPassword(ownerPassword)
	if err != nil {
		return BootstrapResult{}, err
	}

	tenantID := postgres.NewID("hc_ten")

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return BootstrapResult{}, err
	}
	defer postgres.Rollback(tx)

	// Serialize concurrent bootstraps. The old INSERT ... WHERE NOT
	// EXISTS was NOT race-safe (READ COMMITTED: two statement snapshots
	// each miss the other's uncommitted row — two tenants, two owner
	// credential sets). The xact lock releases at commit/rollback and
	// makes both guards below authoritative.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, postgres.LockKeyBootstrap); err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap lock: %w", err)
	}

	if opts.FirstTenantOnly {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tenants)`).Scan(&exists); err != nil {
			return BootstrapResult{}, fmt.Errorf("check tenants: %w", err)
		}
		if exists {
			return BootstrapResult{}, ErrAlreadyBootstrapped
		}
	}

	var emailTaken bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, ownerEmail).Scan(&emailTaken); err != nil {
		return BootstrapResult{}, fmt.Errorf("check owner email: %w", err)
	}
	if emailTaken {
		return BootstrapResult{}, ErrOwnerEmailExists
	}

	var createdTenant domain.Tenant
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO tenants (id, name, status)
		VALUES ($1, $2, 'active')
		RETURNING id, name, status, created_at, updated_at
	`, tenantID, tenantName).Scan(&createdTenant.ID, &createdTenant.Name, &createdTenant.Status, &createdTenant.CreatedAt, &createdTenant.UpdatedAt); err != nil {
		return BootstrapResult{}, fmt.Errorf("create tenant: %w", err)
	}

	// Every tenant has a tenant_settings row — bootstrap seeds an
	// explicit one so operators can edit settings immediately without
	// an implicit upsert side-effect on first invoice. Values match
	// tenant.DefaultSettings.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_settings (
			tenant_id, default_currency, timezone, invoice_prefix,
			net_payment_terms, tax_provider, tax_on_failure
		)
		VALUES ($1, 'USD', 'UTC', 'HC', 30, 'manual', 'block')
		ON CONFLICT (tenant_id) DO NOTHING
	`, tenantID); err != nil {
		return BootstrapResult{}, fmt.Errorf("seed tenant settings: %w", err)
	}

	// The tx runs under TxBypass (no tenant exists at BEGIN), so it stamps
	// the settings the writes below read: app.tenant_id for every audit row,
	// and app.livemode, which the set_livemode trigger copies to each row.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return BootstrapResult{}, fmt.Errorf("stamp tenant: %w", err)
	}

	// A test and a live secret key. A key mints keys only in its own mode, so
	// the account's first live key comes from here. Each key and its audit
	// row are written in the key's mode.
	mintSecret := func(live bool, name string) (string, error) {
		mode := "off"
		if live {
			mode = "on"
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
			return "", fmt.Errorf("set livemode: %w", err)
		}
		created, err := deps.CreateKeyTx(postgres.WithLivemode(ctx, live), tx, tenantID, auth.CreateKeyInput{Name: name, KeyType: auth.KeyTypeSecret})
		if err != nil {
			return "", fmt.Errorf("create %s: %w", name, err)
		}
		return created.RawKey, nil
	}
	testSecretKey, err := mintSecret(false, "Bootstrap Key (Test)")
	if err != nil {
		return BootstrapResult{}, err
	}
	liveSecretKey, err := mintSecret(true, "Bootstrap Key (Live)")
	if err != nil {
		return BootstrapResult{}, err
	}

	ownerUser, err := deps.CreateUserTx(ctx, tx, ownerEmail, passwordHash, tenantID, "owner")
	if err != nil {
		return BootstrapResult{}, err
	}

	// ADR-090: record the provisioning in the NEW tenant's own log, on this
	// same transaction (panel Q6's rule for the platform plane). Bootstrap
	// mints a LIVE secret key and an owner account with no prior actor to
	// attribute them to — "who created this live key, and when?" is exactly
	// the question an auditor asks, and until now the answer was nowhere.
	// actor = system: there is genuinely no authenticated identity here (the
	// route runs only while zero tenants exist, gated by a bootstrap token).
	if deps.Audit != nil {
		// Account-plane facts are recorded live.
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
			return BootstrapResult{}, fmt.Errorf("stamp livemode for audit: %w", err)
		}

		if err := deps.Audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionCreate,
			ResourceType:  "tenant",
			ResourceID:    tenantID,
			ResourceLabel: tenantName,
			// owner_user_id POINTS AT the account; the address is deliberately NOT
			// stored beside it. audit_log is append-only (0150 revoked DELETE from
			// the runtime role), so an email written here could never be erased —
			// and the reader resolves it from the users row, which can be.
			Metadata: map[string]any{
				"action":        "bootstrap_provisioned",
				"owner_user_id": ownerUser.ID,
			},
		}); err != nil {
			return BootstrapResult{}, fmt.Errorf("audit bootstrap tenant: %w", err)
		}

		if err := deps.Audit.LogInTx(ctx, tx, audit.Entry{
			Action:       domain.AuditActionCreate,
			ResourceType: "user",
			ResourceID:   ownerUser.ID,
			Metadata: map[string]any{
				"action": "bootstrap_provisioned",
				"role":   "owner",
			},
		}); err != nil {
			return BootstrapResult{}, fmt.Errorf("audit bootstrap owner: %w", err)
		}
	}

	// The account's default dunning policy, one per mode (ADR-123): without
	// it a failed payment is never retried until someone thinks to create a
	// policy. Seeded last so app.tenant_id is already stamped for its audit
	// rows. The seed only holds the default until the operator's first
	// enabled policy takes it over.
	if err := seedDunningPolicies(ctx, tx, deps.SeedDunningTx, deps.Audit, tenantID, "bootstrap_provisioned"); err != nil {
		return BootstrapResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return BootstrapResult{}, err
	}

	return BootstrapResult{
		Tenant:            createdTenant,
		OwnerUser:         ownerUser,
		OwnerPassword:     ownerPassword,
		PasswordGenerated: generated,
		TestSecretKey:     testSecretKey,
		LiveSecretKey:     liveSecretKey,
	}, nil
}

// generatePassword returns 24 random hex chars (96 bits) — clears the
// 12-char minimum from internal/user.MinPasswordLength with margin.
// Returned once in the bootstrap result; not retrievable afterwards.
func generatePassword() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
