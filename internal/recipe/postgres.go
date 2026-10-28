package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

const recipeInstanceColumns = `
	id, tenant_id, recipe_key, recipe_version,
	overrides, created_object_ids, created_at, COALESCE(created_by, '')
`

func (s *PostgresStore) GetByKey(ctx context.Context, tenantID, recipeKey string) (domain.RecipeInstance, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	defer postgres.Rollback(tx)
	inst, err := s.GetByKeyTx(ctx, tx, tenantID, recipeKey)
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.RecipeInstance{}, err
	}
	return inst, nil
}

func (s *PostgresStore) List(ctx context.Context, tenantID string) ([]domain.RecipeInstance, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+recipeInstanceColumns+`
		FROM recipe_instances
		WHERE tenant_id = $1
		ORDER BY created_at DESC, id DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.RecipeInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) GetByKeyTx(ctx context.Context, tx *sql.Tx, tenantID, recipeKey string) (domain.RecipeInstance, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+recipeInstanceColumns+`
		FROM recipe_instances WHERE tenant_id = $1 AND recipe_key = $2
	`, tenantID, recipeKey)
	inst, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RecipeInstance{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	return inst, nil
}

func (s *PostgresStore) CreateTx(ctx context.Context, tx *sql.Tx, inst domain.RecipeInstance) (domain.RecipeInstance, error) {
	overridesJSON, err := json.Marshal(safeMap(inst.Overrides))
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	createdJSON, err := json.Marshal(inst.CreatedObjects)
	if err != nil {
		return domain.RecipeInstance{}, err
	}

	row := tx.QueryRowContext(ctx, `
		INSERT INTO recipe_instances (
			tenant_id, recipe_key, recipe_version, overrides, created_object_ids, created_by
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		RETURNING `+recipeInstanceColumns+`
	`, inst.TenantID, inst.RecipeKey, inst.RecipeVersion,
		overridesJSON, createdJSON, inst.CreatedBy)

	out, err := scanInstance(row)
	if err != nil {
		// Another install of this recipe committed after this one read the
		// badge. Answer 409 like the meter and plan-code conflicts; a retry
		// returns that install.
		if postgres.IsUniqueViolation(err) {
			return domain.RecipeInstance{}, errs.AlreadyExists("recipe_key", fmt.Sprintf("recipe %q is already installed", inst.RecipeKey))
		}
		return domain.RecipeInstance{}, err
	}
	return out, nil
}

// rowScanner abstracts QueryRowContext / Rows.Scan into one helper so the
// per-column scan code only lives in scanInstance.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(s rowScanner) (domain.RecipeInstance, error) {
	var inst domain.RecipeInstance
	var overridesJSON, createdJSON []byte
	if err := s.Scan(
		&inst.ID, &inst.TenantID, &inst.RecipeKey, &inst.RecipeVersion,
		&overridesJSON, &createdJSON, &inst.CreatedAt, &inst.CreatedBy,
	); err != nil {
		return domain.RecipeInstance{}, err
	}
	if len(overridesJSON) > 0 {
		if err := json.Unmarshal(overridesJSON, &inst.Overrides); err != nil {
			return domain.RecipeInstance{}, err
		}
	}
	if len(createdJSON) > 0 {
		if err := json.Unmarshal(createdJSON, &inst.CreatedObjects); err != nil {
			return domain.RecipeInstance{}, err
		}
	}
	if inst.Overrides == nil {
		inst.Overrides = map[string]any{}
	}
	return inst, nil
}

func safeMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
