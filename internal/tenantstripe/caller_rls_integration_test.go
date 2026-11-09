package tenantstripe

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	hardcountauth "github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// asCaller is the ctx of a request from tenantID in one mode: the tenant and
// the mode the auth middleware puts on it.
func asCaller(ctx context.Context, tenantID string, live bool) context.Context {
	return hardcountauth.WithTenantID(postgres.WithLivemode(ctx, live), tenantID)
}

// TestStore_RefusesOtherTenantAndMode_RealPostgres: the five methods a
// Settings → Payments request reaches (Upsert, MarkVerified, ListByTenant,
// SetWebhookSecret, Delete) run as the caller in ctx, under row-level
// security. The database then refuses a row of another tenant, or of the
// caller's other mode, even when the arguments name that row. The caller is
// tenant A in test mode. Its targets are tenant B's test row and its own live
// row. Afterwards every row must be exactly as seeded.
//
// A ctx without the tenant or the mode is refused before anything opens, even
// when the arguments name the caller's own row: the store takes the caller
// from ctx and never from its arguments.
func TestStore_RefusesOtherTenantAndMode_RealPostgres(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := NewStore(db)

	tenantA := testutil.CreateTestTenant(t, db, "Stripe RLS A")
	tenantB := testutil.CreateTestTenant(t, db, "Stripe RLS B")
	seed := func(tenantID string, live bool, tag string) domain.StripeProviderCredentials {
		t.Helper()
		mode := modeLabel(live)
		c, err := store.Upsert(asCaller(ctx, tenantID, live), UpsertInput{
			TenantID: tenantID, Livemode: live,
			SecretKey: "sk_" + mode + "_FAKE_" + tag, PublishableKey: "pk_" + mode + "_FAKE_" + tag,
			WebhookSecret: "whsec_FAKE_" + tag,
		})
		if err != nil {
			t.Fatalf("seed %s: %v", tag, err)
		}
		return c
	}
	seed(tenantA, false, "a_test")
	aLive := seed(tenantA, true, "a_live")
	bTest := seed(tenantB, false, "b_test")
	before := credentialRows(t, admin)
	if len(before) != 3 {
		t.Fatalf("seeded %d rows, want 3", len(before))
	}

	caller := asCaller(ctx, tenantA, false)
	for _, tc := range []struct {
		name     string
		tenantID string
		live     bool
		id       string
	}{
		{"another tenant's row", tenantB, false, bTest.ID},
		{"the caller's other mode's row", tenantA, true, aLive.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// leftAlone fails the test when the target row is no longer
			// was, the row as read just before the write.
			leftAlone := func(write string, was credentialRow) {
				t.Helper()
				if now, ok := credentialRows(t, admin)[tc.id]; !ok || !reflect.DeepEqual(was, now) {
					t.Errorf("%s reached the target row: before %+v, after %+v (still there: %v)", write, was, now, ok)
				}
			}
			mode := modeLabel(tc.live)
			was := credentialRows(t, admin)[tc.id]
			_, err := store.Upsert(caller, UpsertInput{
				TenantID: tc.tenantID, Livemode: tc.live,
				SecretKey: "sk_" + mode + "_FAKE_intruder", PublishableKey: "pk_" + mode + "_FAKE_intruder",
				WebhookSecret: "whsec_FAKE_intruder",
			})
			if !isRLSRefusal(err) {
				t.Errorf("Upsert: err = %v, want the row-level security refusal (SQLSTATE 42501)", err)
			}
			leftAlone("Upsert", was)

			was = credentialRows(t, admin)[tc.id]
			if _, err := store.SetWebhookSecret(caller, tc.tenantID, tc.live, "whsec_FAKE_intruder_2"); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("SetWebhookSecret: err = %v, want ErrNotFound (RLS hides the row)", err)
			}
			leftAlone("SetWebhookSecret", was)

			// An UPDATE that RLS hides the row from changes nothing, and
			// MarkVerified does not count rows, so only the row tells.
			was = credentialRows(t, admin)[tc.id]
			if err := store.MarkVerified(caller, tc.tenantID, tc.live, "acct_FAKE_intruder", "Intruder"); err != nil {
				t.Errorf("MarkVerified: %v", err)
			}
			leftAlone("MarkVerified", was)

			rows, err := store.ListByTenant(caller, tc.tenantID)
			if err != nil {
				t.Errorf("ListByTenant: %v", err)
			}
			for _, r := range rows {
				if r.ID == tc.id {
					t.Errorf("ListByTenant(%s) returned the target row %s (livemode=%v)", tc.tenantID, r.ID, r.Livemode)
				}
			}
			was = credentialRows(t, admin)[tc.id]
			if err := store.Delete(caller, tc.tenantID, tc.live); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("Delete: err = %v, want ErrNotFound (RLS hides the row)", err)
			}
			leftAlone("Delete", was)
		})
	}
	if after := credentialRows(t, admin); !reflect.DeepEqual(before, after) {
		t.Errorf("rows changed by refused writes:\nbefore %+v\nafter  %+v", before, after)
	}

	// The caller's own row, named in the arguments, with a ctx that lacks the
	// tenant or the mode.
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"no tenant on ctx", postgres.WithLivemode(ctx, false), postgres.ErrTenantUnset},
		{"no mode on ctx", hardcountauth.WithTenantID(ctx, tenantA), postgres.ErrLivemodeUnset},
	} {
		for _, call := range []struct {
			method string
			run    func() error
		}{
			{"Upsert", func() error {
				_, err := store.Upsert(tc.ctx, UpsertInput{TenantID: tenantA, SecretKey: "sk_test_FAKE_noctx", PublishableKey: "pk_test_FAKE_noctx"})
				return err
			}},
			{"MarkVerified", func() error { return store.MarkVerified(tc.ctx, tenantA, false, "acct_FAKE_noctx", "No ctx") }},
			{"ListByTenant", func() error { _, err := store.ListByTenant(tc.ctx, tenantA); return err }},
			{"SetWebhookSecret", func() error { _, err := store.SetWebhookSecret(tc.ctx, tenantA, false, "whsec_FAKE_noctx"); return err }},
			{"Delete", func() error { return store.Delete(tc.ctx, tenantA, false) }},
		} {
			if err := call.run(); !errors.Is(err, tc.want) {
				t.Errorf("%s with %s: err = %v, want %v", call.method, tc.name, err, tc.want)
			}
		}
	}
	if after := credentialRows(t, admin); !reflect.DeepEqual(before, after) {
		t.Errorf("rows changed by calls without a caller:\nbefore %+v\nafter  %+v", before, after)
	}
}

// credentialRow is one stripe_provider_credentials row as stored.
type credentialRow struct {
	TenantID      string
	Livemode      bool
	SecretKey     string
	WebhookSecret sql.NullString
	AccountID     sql.NullString
	Verified      bool
	UpdatedAt     time.Time
}

// credentialRows reads every row through the owner connection, which RLS does
// not filter, keyed by id.
func credentialRows(t *testing.T, admin *sql.DB) map[string]credentialRow {
	t.Helper()
	rows, err := admin.QueryContext(context.Background(), `
		SELECT id, tenant_id, livemode, secret_key_encrypted, webhook_secret_encrypted,
		       stripe_account_id, verified_at IS NOT NULL, updated_at
		FROM stripe_provider_credentials
	`)
	if err != nil {
		t.Fatalf("read stripe_provider_credentials: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]credentialRow{}
	for rows.Next() {
		var id string
		var r credentialRow
		if err := rows.Scan(&id, &r.TenantID, &r.Livemode, &r.SecretKey, &r.WebhookSecret, &r.AccountID, &r.Verified, &r.UpdatedAt); err != nil {
			t.Fatalf("scan stripe_provider_credentials: %v", err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read stripe_provider_credentials: %v", err)
	}
	return out
}

// isRLSRefusal reports whether err is Postgres refusing a row under a
// row-level security policy (SQLSTATE 42501, insufficient_privilege).
func isRLSRefusal(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}
