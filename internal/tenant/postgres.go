package tenant

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Create(ctx context.Context, t domain.Tenant) (domain.Tenant, error) {
	return s.CreateAudited(ctx, t, nil)
}

// CreateAudited creates the tenant and runs the caller-supplied audit
// emission in ONE transaction (ADR-090 shared fate). The tenant's id is
// generated up front so the tx can be opened AS the new tenant: audit_log
// is FORCE-RLS'd, and a platform-plane action lands in the NEW tenant's own
// log (design-panel Q6 — no separate platform log), which requires the
// app.tenant_id GUC to be the new id before the audit INSERT runs. The
// tenants table itself carries no RLS, so the INSERT is unaffected by the
// tenant-scoped GUC.
func (s *PostgresStore) CreateAudited(ctx context.Context, t domain.Tenant, emit func(tx *sql.Tx, out domain.Tenant) error) (domain.Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, s.db.QueryTimeout)
	defer cancel()

	id := postgres.NewID("vlx_ten")
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, id)
	if err != nil {
		return domain.Tenant{}, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO tenants (id, name, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
		RETURNING id, name, status, created_at, updated_at
	`, id, t.Name, domain.TenantStatusActive, now).Scan(
		&t.ID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return domain.Tenant{}, err
	}
	if emit != nil {
		if err := emit(tx, t); err != nil {
			return domain.Tenant{}, fmt.Errorf("provision in tx: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (domain.Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, s.db.QueryTimeout)
	defer cancel()

	var t domain.Tenant
	err := s.db.Pool.QueryRowContext(ctx, `
		SELECT id, name, status, created_at, updated_at
		FROM tenants WHERE id = $1
	`, id).Scan(&t.ID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Tenant{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, s.db.QueryTimeout)
	defer cancel()

	// Default 50, clamp to 100 — no-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	query := `SELECT id, name, status, created_at, updated_at FROM tenants`
	args := []any{}
	argIdx := 1

	if filter.Status != "" {
		query += ` WHERE status = $1`
		args = append(args, filter.Status)
		argIdx++
	}

	query += ` ORDER BY created_at DESC`
	query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, argIdx, argIdx+1)
	args = append(args, limit, filter.Offset)

	rows, err := s.db.Pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tenants []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

func (s *PostgresStore) UpdateStatus(ctx context.Context, id string, status domain.TenantStatus) (domain.Tenant, error) {
	ctx, cancel := context.WithTimeout(ctx, s.db.QueryTimeout)
	defer cancel()

	var t domain.Tenant
	err := s.db.Pool.QueryRowContext(ctx, `
		UPDATE tenants SET status = $1, updated_at = $2
		WHERE id = $3
		RETURNING id, name, status, created_at, updated_at
	`, status, time.Now().UTC(), id).Scan(&t.ID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Tenant{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}
