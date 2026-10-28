package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// PricingWriter is the narrow tx-aware surface recipe.Service needs from
// pricing.Service. Defined here so the recipe package owns no cross-domain
// state and can be exercised with fakes in unit tests.
type PricingWriter interface {
	CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error)
	// GetMeterByEventNameTx reads a meter the install would adopt on the
	// install's own transaction, FOR KEY SHARE, so an aggregation PATCH
	// cannot land between the conformance check and the prices that use the
	// meter (ADR-130 §15).
	GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error)
	// CreatePlanWithPricesTx creates a plan and its prices on tx, with the
	// checks POST /v1/plans runs.
	CreatePlanWithPricesTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan, prices []domain.PlanPrice) (domain.Plan, []domain.PlanPrice, error)
	ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error)
}

// AuditEmitter is the narrow in-tx audit seam (ADR-090). Instantiate owns the
// coordinator transaction that spans every cross-domain write, so it emits the
// audit row on that same tx: the installed object graph and the record of what
// was installed commit or roll back together.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// Service is the orchestrator for the recipes feature: it answers
// "list recipes", "preview", and "instantiate" (idempotent apply). The
// entities a recipe creates (meters, plans and their prices) live in the
// pricing domain's store; this service threads a single transaction across
// those writes and the install record, so a recipe is either fully applied
// or not applied at all. A recipe touches only the catalog (ADR-130 §15).
type Service struct {
	db          *postgres.DB
	store       Store
	registry    *Registry
	pricing     PricingWriter
	auditLogger AuditEmitter
}

// NewService wires the recipe service. registry must already be loaded
// via Load(); the constructor does no I/O.
func NewService(
	db *postgres.DB,
	store Store,
	registry *Registry,
	pricing PricingWriter,
) *Service {
	return &Service{
		db:       db,
		store:    store,
		registry: registry,
		pricing:  pricing,
	}
}

// SetAuditLogger wires in-tx audit emission for recipe apply (ADR-090). A nil
// emitter skips emission so unit tests can drive the service with fakes; the
// composition root's audit.MustWired check turns a forgotten wiring line into
// a boot-time panic rather than a silently un-audited install.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// RecipeListItem is one entry in the GET /v1/recipes response — the
// canonical recipe metadata plus per-tenant installation state and a
// creates summary so the picker UI can render "1 meter · 9 prices ·
// monthly billing" without an extra preview round-trip. The
// dashboard uses Instantiated to render an "Installed" badge instead of
// the "Install" CTA — re-apply is an idempotent no-op and there is no
// uninstall (ADR-085).
type RecipeListItem struct {
	domain.Recipe
	Creates      RecipeCreates          `json:"creates"`
	Instantiated *domain.RecipeInstance `json:"instantiated,omitempty"`
}

// RecipeCreates is the per-role count of objects a recipe will produce
// when instantiated. Surfaced on list + detail responses so the
// dashboard can render summary chips without round-tripping preview.
// Counts mirror the shape of domain.CreatedObjects (which is per-ID),
// scaled down to integers for display.
type RecipeCreates struct {
	Meters int `json:"meters"`
	Plans  int `json:"plans"`
	Prices int `json:"prices"`
}

// countCreates derives a RecipeCreates summary from a parsed recipe.
func countCreates(r domain.Recipe) RecipeCreates {
	c := RecipeCreates{Meters: len(r.Meters), Plans: len(r.Plans)}
	for _, p := range r.Plans {
		c.Prices += len(p.Prices)
	}
	return c
}

// ListRecipes returns every recipe in the registry — rendered with its
// declared defaults so no raw template syntax ("{{ .currency }}") leaks
// into the response — tagged with this tenant's installation state. One
// indexed read per recipe — fine at the v1 catalog size (3 recipes);
// revisit if the catalog grows past ~50.
func (s *Service) ListRecipes(ctx context.Context, tenantID string) ([]RecipeListItem, error) {
	recipes := s.registry.List()
	out := make([]RecipeListItem, 0, len(recipes))
	for _, r := range recipes {
		display, err := displayRecipe(r)
		if err != nil {
			return nil, fmt.Errorf("recipe %q: render defaults: %w", r.Key, err)
		}
		item := RecipeListItem{Recipe: display, Creates: countCreates(r)}
		inst, err := s.store.GetByKey(ctx, tenantID, r.Key)
		switch {
		case err == nil:
			instCopy := inst
			item.Instantiated = &instCopy
		case errors.Is(err, errs.ErrNotFound):
			// not installed for this tenant — leave Instantiated nil
		default:
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// RecipeDetail is the GET /v1/recipes/{key} response. Same fields as a
// list item plus the long-form description and full overridable_schema
// (already on the embedded domain.Recipe). Wrapping rather than
// returning bare domain.Recipe lets us co-emit the creates summary so
// the dashboard's picker drawer renders chips without a preview call.
type RecipeDetail struct {
	domain.Recipe
	Creates RecipeCreates `json:"creates"`
}

// GetRecipe returns the rendered-with-defaults form of a recipe so the
// dashboard can populate the override form. No DB I/O; pure registry.
// (This comment was true before the code was: until 2026-07-26 the RAW
// registry recipe was returned, leaking "{{ .currency }}" template syntax
// into the detail response.)
func (s *Service) GetRecipe(key string) (RecipeDetail, error) {
	r, ok := s.registry.Get(key)
	if !ok {
		return RecipeDetail{}, errs.ErrNotFound
	}
	display, err := displayRecipe(r)
	if err != nil {
		return RecipeDetail{}, fmt.Errorf("recipe %q: render defaults: %w", key, err)
	}
	return RecipeDetail{Recipe: display, Creates: countCreates(r)}, nil
}

// displayRecipe renders a recipe with its declared defaults for read-only
// display surfaces (list/detail), keeping the overridable schema — which
// renderRecipe strips because overrides are an input — so the dashboard's
// override form still has it. Instantiate/Preview never use this: they
// render with the caller's actual overrides.
func displayRecipe(r domain.Recipe) (domain.Recipe, error) {
	rendered, err := renderRecipe(r, nil)
	if err != nil {
		return domain.Recipe{}, err
	}
	rendered.Overridable = r.Overridable
	return rendered, nil
}

// ListInstances returns the recipe_instances rows for the tenant.
func (s *Service) ListInstances(ctx context.Context, tenantID string) ([]domain.RecipeInstance, error) {
	return s.store.List(ctx, tenantID)
}

// PreviewResult is the wire shape of POST /v1/recipes/{key}/preview.
// `objects` lists what an install creates: the meters, and each plan with
// its prices. `warnings` is a slot for non-fatal conditions; it is always
// empty today, and stays so the contract is stable when one lands.
type PreviewResult struct {
	Key      string         `json:"key"`
	Version  string         `json:"version"`
	Objects  PreviewObjects `json:"objects"`
	Warnings []string       `json:"warnings"`
}

// PreviewObjects is what an install creates. Empty arrays are emitted (no
// `omitempty`) so the picker UI iterates without null guards. An install
// reuses a meter that already exists with the same event_name and
// aggregation; the preview lists it either way.
type PreviewObjects struct {
	Meters []domain.RecipeMeter `json:"meters"`
	Plans  []domain.RecipePlan  `json:"plans"`
}

// Preview renders a recipe with caller-supplied overrides and returns
// the would-be-created object graph. Pure in-memory; no DB writes, no
// transactions. Cheap enough to call on every override-form keystroke.
func (s *Service) Preview(_ context.Context, recipeKey string, overrides map[string]any) (PreviewResult, error) {
	r, ok := s.registry.Get(recipeKey)
	if !ok {
		return PreviewResult{}, errs.ErrNotFound
	}
	rendered, err := renderRecipe(r, overrides)
	if err != nil {
		return PreviewResult{}, errs.Invalid("overrides", err.Error())
	}
	return previewResultFrom(rendered), nil
}

// previewResultFrom converts a rendered domain.Recipe into the public
// preview wire shape. Slices default to non-nil so the JSON encoder
// emits `[]` rather than `null` — picker UI maps without guards.
func previewResultFrom(r domain.Recipe) PreviewResult {
	out := PreviewResult{
		Key:      r.Key,
		Version:  r.Version,
		Objects:  PreviewObjects{Meters: r.Meters, Plans: make([]domain.RecipePlan, 0, len(r.Plans))},
		Warnings: []string{},
	}
	if out.Objects.Meters == nil {
		out.Objects.Meters = []domain.RecipeMeter{}
	}
	for _, p := range r.Plans {
		if p.Prices == nil {
			p.Prices = []domain.RecipePrice{}
		}
		out.Objects.Plans = append(out.Objects.Plans, p)
	}
	return out
}

// InstantiateOptions controls one-off knobs for Instantiate. CreatedBy is
// the actor (operator email or API key ID) recorded on
// recipe_instances.created_by for audit.
type InstantiateOptions struct {
	CreatedBy string
}

// Instantiate applies a recipe for tenantID under one transaction. Order:
// meters → each plan with its prices → instance row → audit row. Any
// failure rolls back the whole graph; partial state never reaches the
// tenant. It writes only the catalog (ADR-130 §15). Apply is an idempotent EVENT
// (ADR-085): a second call on an already-installed recipe is a no-op that
// returns the existing instance — never a 409, never a duplicate plan.
func (s *Service) Instantiate(
	ctx context.Context,
	tenantID, recipeKey string,
	overrides map[string]any,
	opts InstantiateOptions,
) (domain.RecipeInstance, error) {
	r, ok := s.registry.Get(recipeKey)
	if !ok {
		return domain.RecipeInstance{}, errs.ErrNotFound
	}

	rendered, err := renderRecipe(r, overrides)
	if err != nil {
		return domain.RecipeInstance{}, errs.Invalid("overrides", err.Error())
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	defer postgres.Rollback(tx)

	// Idempotency — a recipe is an instantiation EVENT, applied once. If the
	// badge already exists, apply is a no-op: return the existing instance
	// unchanged — and, deliberately, emit NO audit row. Nothing was installed
	// on this call; a row here would be evidence of a mutation that never
	// happened (the fabricated-record class ADR-090 exists to kill). The
	// original apply's row is the truthful record. Everything the badge
	// recorded is still present (plans are
	// ON DELETE RESTRICT with no hard-delete; meters are shared reference
	// data), so there is nothing to re-create and never a second plan to mint
	// on a double-submit — the badge IS the idempotency gate. The badge is
	// mode-scoped like the objects it records (0157): RLS partitions this read
	// by app.livemode, so a test-mode install never gates a live-mode one.
	// Additive re-apply
	// against a NEWER template version is deferred (ADR-085); v1 ships a single
	// version per recipe, so a re-apply at the same version is a pure no-op.
	if existing, err := s.store.GetByKeyTx(ctx, tx, tenantID, recipeKey); err == nil {
		// Declare the no-op to the audit-coverage detector. This request is a
		// mutating POST that will answer 201 having changed NOTHING, so without
		// the declaration it is indistinguishable — to an observer at the
		// transport — from an install that forgot its audit row, and every
		// re-apply would be reported as an uncovered mutation.
		audit.MarkSkip(ctx)
		return existing, nil
	} else if !errors.Is(err, errs.ErrNotFound) {
		return domain.RecipeInstance{}, err
	}

	objs := domain.CreatedObjects{}

	// Meters — adopt an existing event name only if its AGGREGATION matches
	// (the sole billing-consulted field; sum vs max/count/last silently
	// mis-rolls-up usage), else refuse LOUD. The read runs on the install's
	// transaction FOR KEY SHARE, so an aggregation PATCH waits for the
	// install. Adoption never changes the meter: a meter only counts, and
	// the prices go on the new plan (ADR-130 §15), so adopting a meter other
	// plans price changes none of their prices.
	meterIDByKey := make(map[string]string, len(rendered.Meters))
	for _, m := range rendered.Meters {
		if existing, err := s.pricing.GetMeterByEventNameTx(ctx, tx, m.EventName); err == nil {
			if diffs := meterConformanceDiff(existing, m); len(diffs) > 0 {
				return domain.RecipeInstance{}, errs.AlreadyExists("meter", meterConflictMessage(m.EventName, diffs))
			}
			meterIDByKey[m.EventName] = existing.ID
			objs.MeterIDs = append(objs.MeterIDs, existing.ID)
			continue
		} else if !errors.Is(err, errs.ErrNotFound) {
			return domain.RecipeInstance{}, fmt.Errorf("meter %q: adoption check: %w", m.EventName, err)
		}
		created, err := s.pricing.CreateMeterTx(ctx, tx, tenantID, domain.Meter{
			EventName:   m.EventName,
			Name:        m.Name,
			Unit:        m.Unit,
			Aggregation: m.Aggregation,
		})
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("meter %q: %w", m.EventName, err)
		}
		meterIDByKey[m.EventName] = created.ID
		objs.MeterIDs = append(objs.MeterIDs, created.ID)
	}

	// Plan — GENERATE a fresh plan; NEVER adopt an existing plan by code
	// (ADR-085). The plan is created with its prices, so silent-$0 from
	// under-wiring is impossible, and there is no "does this existing plan
	// match?" question. The operator owns the plan once created; the recipe
	// never mutates or reuses one. On a code collision (a prior recipe's
	// plan, or a foreign plan) the NEW plan is uniquified (`_2`, `_3`, …) —
	// the incumbent is never renamed. The plan's currency is the recipe's,
	// and its prices take it.
	existingPlans, err := s.pricing.ListPlans(ctx, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, fmt.Errorf("list plans: %w", err)
	}
	takenCodes := make(map[string]bool, len(existingPlans))
	for _, ep := range existingPlans {
		takenCodes[ep.Code] = true
	}
	for _, p := range rendered.Plans {
		planPrices := make([]domain.PlanPrice, 0, len(p.Prices))
		for _, rp := range p.Prices {
			planPrices = append(planPrices, domain.PlanPrice{
				MeterID:        meterIDByKey[rp.Meter],
				DimensionMatch: rp.DimensionMatch,
				PriceShape:     rp.PriceShape,
			})
		}
		code := freePlanCode(p.Code, takenCodes)
		takenCodes[code] = true
		created, createdPrices, err := s.pricing.CreatePlanWithPricesTx(ctx, tx, tenantID, domain.Plan{
			Code:            code,
			Name:            p.Name,
			Currency:        p.Currency,
			BillingInterval: p.BillingInterval,
			Status:          domain.PlanActive,
			BaseAmountCents: p.BaseAmountCents,
			BaseBillTiming:  p.BaseBillTiming,
		}, planPrices)
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("plan %q: %w", code, err)
		}
		objs.PlanIDs = append(objs.PlanIDs, created.ID)
		for _, cp := range createdPrices {
			objs.PriceIDs = append(objs.PriceIDs, cp.ID)
		}
	}

	inst, err := s.store.CreateTx(ctx, tx, domain.RecipeInstance{
		TenantID:       tenantID,
		RecipeKey:      recipeKey,
		RecipeVersion:  rendered.Version,
		Overrides:      copyOverrides(overrides),
		CreatedObjects: objs,
		CreatedBy:      opts.CreatedBy,
	})
	if err != nil {
		return domain.RecipeInstance{}, err
	}

	// Audit rides the coordinator tx (ADR-090): the whole installed graph and
	// the record of what was installed share fate. Emitted only on the path
	// that actually installed something — the idempotent no-op above returns
	// before here.
	if err := s.emitInstantiated(ctx, tx, rendered, inst); err != nil {
		return domain.RecipeInstance{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.RecipeInstance{}, err
	}
	return inst, nil
}

// emitInstantiated writes the recipe-apply audit row on the coordinator tx.
//
// Wire strings are FROZEN vocabulary: action "create" (an apply is the
// creation of an install), resource_type "recipe". resource_id is the recipe
// KEY, not the instance id — the key is what the operator surface addresses
// (GET /v1/recipes/{key}, the dashboard's install badge), so it is the id that
// resolves to something a human can open; the instance id rides in metadata.
//
// Metadata is the answer to "what did this recipe install?", which nothing
// else can answer once the operator starts editing the objects: it mirrors the
// instance row's created-object ids. Per ADR-085 those ids include ADOPTED
// meters (apply reconnects to an existing meter rather than duplicating it)
// — i.e. the graph this apply wired, which is exactly the question an
// auditor asks.
func (s *Service) emitInstantiated(ctx context.Context, tx *sql.Tx, rendered domain.Recipe, inst domain.RecipeInstance) error {
	if s.auditLogger == nil {
		return nil
	}
	objs := inst.CreatedObjects
	meta := map[string]any{
		"instance_id":    inst.ID,
		"recipe_key":     inst.RecipeKey,
		"recipe_version": inst.RecipeVersion,
	}
	if len(objs.PlanIDs) > 0 {
		meta["plan_ids"] = objs.PlanIDs
	}
	if len(objs.MeterIDs) > 0 {
		meta["meter_ids"] = objs.MeterIDs
	}
	if len(objs.PriceIDs) > 0 {
		meta["price_ids"] = objs.PriceIDs
	}
	if err := s.auditLogger.LogInTx(ctx, tx, audit.Entry{
		Action:        domain.AuditActionCreate,
		ResourceType:  "recipe",
		ResourceID:    inst.RecipeKey,
		ResourceLabel: rendered.Name,
		Metadata:      meta,
	}); err != nil {
		return fmt.Errorf("audit emission: %w", err)
	}
	return nil
}

// copyOverrides returns a defensive shallow copy of the caller-provided
// overrides map so the recipe_instances row can't be mutated through the
// caller's reference after persistence.
func copyOverrides(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// freePlanCode returns `desired` if no plan holds it, else the first free
// `<desired>_N` (N≥2). A recipe never adopts an existing plan by code, so a
// collision (a prior recipe's plan, or a foreign plan) is side-stepped by
// uniquifying the NEW plan — never by renaming the incumbent (k8s generateName
// pattern). The plan's immutable id is its identity; the code is a display slug
// with no functional readers (subscriptions bind by plan id). A concurrent
// writer that steals the chosen code between the ListPlans read and the INSERT
// trips the UNIQUE(tenant_id, code) constraint and rolls the whole tx back — a
// loud retry, never a silent duplicate.
func freePlanCode(desired string, taken map[string]bool) string {
	if !taken[desired] {
		return desired
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s_%d", desired, n)
		if !taken[c] {
			return c
		}
	}
}
