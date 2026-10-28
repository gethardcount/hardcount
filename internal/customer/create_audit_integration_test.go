package customer_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// landThenFailEmitter writes the REAL audit row on the caller's tx and then
// errors. It satisfies customer.AuditEmitter and is the fault injection the
// playbook's mutation-verify gate demands — landing the row first proves the
// audit INSERT is rolled back WITH the business write, not merely that a
// never-attempted write left nothing behind.
type landThenFailEmitter struct {
	logger *audit.Logger
	calls  int
}

func (e *landThenFailEmitter) LogInTx(ctx context.Context, tx *sql.Tx, entry audit.Entry) error {
	e.calls++
	if err := e.logger.LogInTx(ctx, tx, entry); err != nil {
		return err
	}
	return errors.New("injected audit failure")
}

// TestCustomerCreateAudit_SharedFate pins the ADR-090 emission on
// PostgresStore.CreateAudited — POST /v1/customers, which until now was
// covered ONLY by the HTTP catch-all middleware. When that middleware is
// deleted, this emission is the customer-create record; if it were wrong or
// absent, customer provenance would silently vanish from an append-only
// compliance log.
//
// Directions pinned:
//  1. success commits the customer AND exactly one create row (right action /
//     resource / label / metadata);
//  2. a failed business write (duplicate external_id) emits NOTHING — the
//     phantom class;
//  3. a failed emission ROLLS THE CUSTOMER BACK — the silent-loss class;
//  4. the PII rule: no email in metadata, ever (email_set flag only), which
//     TestAudit_NoCustomerPIIInMetadata enforces for the sibling writers.
//
// Mutation-verify: drop the emit-error propagation in CreateAudited and (3)
// fails; move the emit call above the INSERT's RETURNING scan and (2) fails.
func TestCustomerCreateAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Customer Create InTx Audit")

	store := customer.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	createRows := func(t *testing.T, custID string) []domain.AuditEntry {
		t.Helper()
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "customer", ResourceID: custID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		var out []domain.AuditEntry
		for _, r := range rows {
			if r.Action == domain.AuditActionCreate {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("create commits the customer and its audit row together", func(t *testing.T) {
		svc := customer.NewService(store)
		svc.SetAuditLogger(logger)

		created, err := svc.Create(ctx, tenantID, customer.CreateInput{
			ExternalID:  "cus_audit_ok",
			DisplayName: "Audited Co",
			Email:       "billing@audited.example",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		rows := createRows(t, created.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one create audit row for %s; got %d: %+v", created.ID, len(rows), rows)
		}
		row := rows[0]
		if row.ResourceType != "customer" || row.Action != domain.AuditActionCreate {
			t.Errorf("vocabulary: got %s/%s, want create/customer", row.Action, row.ResourceType)
		}
		if row.ResourceLabel != "Audited Co" {
			t.Errorf("resource_label = %q, want %q", row.ResourceLabel, "Audited Co")
		}
		if row.Metadata["external_id"] != "cus_audit_ok" {
			t.Errorf("metadata external_id = %v, want cus_audit_ok", row.Metadata["external_id"])
		}
		if row.Metadata["email_set"] != true {
			t.Errorf("metadata email_set = %v, want true", row.Metadata["email_set"])
		}
		// PII rule (append-only log ⇒ un-erasable): the address itself must
		// never land in metadata — only the flag.
		for k, v := range row.Metadata {
			if sv, _ := v.(string); strings.Contains(sv, "@") {
				t.Errorf("metadata[%q] carries an email address (%q) — PII must not enter the append-only log", k, sv)
			}
		}

		// And the customer really is there (shared fate, success side).
		got, err := store.Get(ctx, tenantID, created.ID)
		if err != nil {
			t.Fatalf("get customer: %v", err)
		}
		if got.ExternalID != "cus_audit_ok" {
			t.Errorf("external_id = %q, want cus_audit_ok", got.ExternalID)
		}
	})

	t.Run("emailless create records email_set=false", func(t *testing.T) {
		svc := customer.NewService(store)
		svc.SetAuditLogger(logger)

		created, err := svc.Create(ctx, tenantID, customer.CreateInput{
			ExternalID: "cus_audit_noemail", DisplayName: "No Email Co",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		rows := createRows(t, created.ID)
		if len(rows) != 1 {
			t.Fatalf("want one create row; got %d", len(rows))
		}
		if rows[0].Metadata["email_set"] != false {
			t.Errorf("metadata email_set = %v, want false", rows[0].Metadata["email_set"])
		}
	})

	t.Run("failed create writes no audit row", func(t *testing.T) {
		svc := customer.NewService(store)
		svc.SetAuditLogger(logger)

		// Same external_id as the first sub-test → unique violation inside
		// the tx, BEFORE the emission hook site. Nothing may be recorded:
		// the log must never assert a customer that was never created.
		_, err := svc.Create(ctx, tenantID, customer.CreateInput{
			ExternalID: "cus_audit_ok", DisplayName: "Duplicate Co",
		})
		if !errors.Is(err, errs.ErrAlreadyExists) {
			t.Fatalf("duplicate external_id must fail with ErrAlreadyExists; got %v", err)
		}
		rows, _, qErr := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "customer", Action: domain.AuditActionCreate,
		})
		if qErr != nil {
			t.Fatalf("query audit: %v", qErr)
		}
		for _, r := range rows {
			if r.ResourceLabel == "Duplicate Co" {
				t.Errorf("phantom audit row for a customer that was never created: %+v", r)
			}
		}
	})

	t.Run("audit failure rolls the customer create back", func(t *testing.T) {
		emitter := &landThenFailEmitter{logger: logger}
		svc := customer.NewService(store)
		svc.SetAuditLogger(emitter)

		_, err := svc.Create(ctx, tenantID, customer.CreateInput{
			ExternalID: "cus_audit_rollback", DisplayName: "Rollback Co",
		})
		if err == nil {
			t.Fatal("create must fail when its audit emission fails (shared fate)")
		}
		if emitter.calls != 1 {
			t.Fatalf("emit calls = %d, want 1", emitter.calls)
		}

		// No customer …
		if _, err := store.GetByExternalID(ctx, tenantID, "cus_audit_rollback"); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("customer committed despite a failed audit emission (got err=%v)", err)
		}
		// … and no audit row: the row the emitter DID insert rolled back with it.
		rows, _, qErr := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "customer", Action: domain.AuditActionCreate,
		})
		if qErr != nil {
			t.Fatalf("query audit: %v", qErr)
		}
		for _, r := range rows {
			if r.ResourceLabel == "Rollback Co" {
				t.Errorf("audit row survived the rolled-back create: %+v", r)
			}
		}

		// The external_id is still free — the operator's retry succeeds.
		svc.SetAuditLogger(logger)
		retried, err := svc.Create(ctx, tenantID, customer.CreateInput{
			ExternalID: "cus_audit_rollback", DisplayName: "Rollback Co",
		})
		if err != nil {
			t.Fatalf("retry create: %v", err)
		}
		if len(createRows(t, retried.ID)) != 1 {
			t.Errorf("retry must land exactly one create audit row")
		}
	})
}

// TestCustomerCreateRoute_IsAccountedFor: the in-tx emission's self-mark is what
// tells the root-mounted AuditCoverage detector that this mutation left evidence.
// A create that stops emitting is reported as an uncovered mutation — it is no
// longer papered over by a row guessed from the URL. Mounted route, real service.
func TestCustomerCreateRoute_IsAccountedFor(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Customer Create Accounted")

	svc := customer.NewService(customer.NewPostgresStore(db))
	svc.SetAuditLogger(audit.NewLogger(db))
	router := customer.NewHandler(svc).Routes()

	reqCtx := audit.WithRequestState(auth.WithTenantID(ctx, tenantID))
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"external_id":"cus_markhandled","display_name":"Handled Co"}`)).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !audit.WasHandled(reqCtx) {
		t.Error("create emitted no audit row — the coverage detector will report it as an uncovered mutation")
	}
}
