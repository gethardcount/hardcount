package customer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestPostgresStore_ListSearch exercises the decrypt-and-match search
// path with PII encryption ENABLED — the property under test is that
// search matches display_name / email substrings even though those
// columns hold ciphertext in the database (SQL ILIKE can't see them).
func TestPostgresStore_ListSearch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	store.SetEncryptor(enc)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Search")

	seed := []domain.Customer{
		{ExternalID: "acme_corp", DisplayName: "Acme Corporation", Email: "billing@acme.com"},
		{ExternalID: "globex", DisplayName: "Globex Inc", Email: "finance@globex.io"},
		{ExternalID: "initech", DisplayName: "Initech LLC", Email: "ap@initech.dev"},
	}
	for _, c := range seed {
		if _, err := store.Create(ctx, tenantID, c); err != nil {
			t.Fatalf("seed %s: %v", c.ExternalID, err)
		}
	}

	// Sanity: the at-rest value must be ciphertext, otherwise this test
	// silently degrades into a plaintext-scan test.
	var raw string
	rawTx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("bypass tx: %v", err)
	}
	err = rawTx.QueryRowContext(ctx,
		`SELECT display_name FROM customers WHERE tenant_id = $1 AND external_id = 'acme_corp'`, tenantID,
	).Scan(&raw)
	_ = rawTx.Rollback()
	if err != nil {
		t.Fatalf("read raw display_name: %v", err)
	}
	if strings.Contains(strings.ToLower(raw), "acme") {
		t.Fatalf("display_name stored as plaintext (%q) — encryption not active, test premise broken", raw)
	}

	cases := []struct {
		name, search string
		wantExtIDs   []string
	}{
		{"display name substring, case-insensitive", "ACME", []string{"acme_corp"}},
		{"email substring (encrypted column)", "finance@globex", []string{"globex"}},
		{"email domain fragment", ".dev", []string{"initech"}},
		{"external_id", "initech", []string{"initech"}},
		{"no match", "no-such-customer", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, total, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Search: tc.search})
			if err != nil {
				t.Fatalf("search %q: %v", tc.search, err)
			}
			if total != len(tc.wantExtIDs) || len(got) != len(tc.wantExtIDs) {
				t.Fatalf("search %q: got %d rows (total %d), want %d", tc.search, len(got), total, len(tc.wantExtIDs))
			}
			for i, want := range tc.wantExtIDs {
				if got[i].ExternalID != want {
					t.Errorf("search %q row %d: got %q, want %q", tc.search, i, got[i].ExternalID, want)
				}
			}
			// Rows must come back decrypted, not as ciphertext.
			for _, c := range got {
				if !strings.HasPrefix(c.DisplayName, "Acme") && !strings.HasPrefix(c.DisplayName, "Globex") && !strings.HasPrefix(c.DisplayName, "Initech") {
					t.Errorf("row %s display_name not decrypted: %q", c.ExternalID, c.DisplayName)
				}
			}
		})
	}

	// Search composes with the status filter (SQL side) — archive one
	// matching customer and assert it drops out.
	t.Run("composes with status filter", func(t *testing.T) {
		all, _, err := store.List(ctx, customer.ListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var acme domain.Customer
		for _, c := range all {
			if c.ExternalID == "acme_corp" {
				acme = c
			}
		}
		acme.Status = "archived"
		if _, err := store.Update(ctx, tenantID, acme); err != nil {
			t.Fatalf("archive: %v", err)
		}
		got, total, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Search: "acme", Status: "active"})
		if err != nil {
			t.Fatalf("search active: %v", err)
		}
		if total != 0 || len(got) != 0 {
			t.Errorf("archived customer should not match status=active search; got %d (total %d)", len(got), total)
		}
	})

	// Pagination over the matched set: total reflects all matches, the
	// window respects limit/offset.
	t.Run("paginates matched set", func(t *testing.T) {
		got, total, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Search: "@", Limit: 2})
		if err != nil {
			t.Fatalf("search paged: %v", err)
		}
		if total != 3 {
			t.Errorf("total: got %d, want 3 (every seeded email contains @)", total)
		}
		if len(got) != 2 {
			t.Errorf("page: got %d rows, want 2", len(got))
		}
		rest, _, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Search: "@", Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("search page 2: %v", err)
		}
		if len(rest) != 1 {
			t.Errorf("page 2: got %d rows, want 1", len(rest))
		}
	})

	// Go-side re-sort for encrypted sort keys: display_name asc must be
	// alphabetical on PLAINTEXT (ciphertext order would be arbitrary).
	t.Run("sorts by decrypted display_name", func(t *testing.T) {
		got, _, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Search: "@", Sort: "display_name", SortDir: "asc"})
		if err != nil {
			t.Fatalf("search sorted: %v", err)
		}
		for i := 1; i < len(got); i++ {
			if strings.ToLower(got[i-1].DisplayName) > strings.ToLower(got[i].DisplayName) {
				t.Errorf("display_name asc out of order: %q before %q", got[i-1].DisplayName, got[i].DisplayName)
			}
		}
	})
}

// TestPostgresStore_ListSortByEncryptedColumn locks the decrypt-then-sort
// fix: with PII encryption ENABLED, sorting the plain (no-search) list by
// display_name or email must order rows by PLAINTEXT. Pre-fix the SQL
// ORDER BY ordered the ciphertext — effectively random to an operator.
func TestPostgresStore_ListSortByEncryptedColumn(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	enc, err := crypto.NewEncryptor(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	store.SetEncryptor(enc)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "SortEnc")

	// Seed out of alphabetical order so insertion order can't fake a pass.
	seed := []domain.Customer{
		{ExternalID: "c3", DisplayName: "Zebra Systems", Email: "z@zebra.dev"},
		{ExternalID: "c1", DisplayName: "Acme Corporation", Email: "billing@acme.com"},
		{ExternalID: "c5", DisplayName: "Mango Analytics", Email: "ap@mango.io"},
		{ExternalID: "c2", DisplayName: "Borealis Labs", Email: "fin@borealis.co"},
		{ExternalID: "c4", DisplayName: "Quartz Cloud", Email: "ops@quartz.gg"},
	}
	for _, c := range seed {
		if _, err := store.Create(ctx, tenantID, c); err != nil {
			t.Fatalf("seed %s: %v", c.ExternalID, err)
		}
	}

	got, total, err := store.List(ctx, customer.ListFilter{
		TenantID: tenantID, Sort: "display_name", SortDir: "asc", Limit: 10,
	})
	if err != nil {
		t.Fatalf("List sort=display_name: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	want := []string{"Acme Corporation", "Borealis Labs", "Mango Analytics", "Quartz Cloud", "Zebra Systems"}
	for i, w := range want {
		if got[i].DisplayName != w {
			t.Fatalf("sort=display_name asc: position %d = %q, want %q (ciphertext ordering leaked through)", i, got[i].DisplayName, w)
		}
	}

	// Descending email + in-memory pagination across the sorted set.
	got, total, err = store.List(ctx, customer.ListFilter{
		TenantID: tenantID, Sort: "email", SortDir: "desc", Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatalf("List sort=email desc paged: %v", err)
	}
	if total != 5 {
		t.Fatalf("paged total = %d, want 5", total)
	}
	// emails desc: z@zebra.dev, ops@quartz.gg, fin@borealis.co, billing@acme.com, ap@mango.io
	if len(got) != 2 || got[0].Email != "fin@borealis.co" || got[1].Email != "billing@acme.com" {
		t.Fatalf("page 2 of email desc = %v, want [fin@borealis.co billing@acme.com]",
			[]string{got[0].Email, got[1].Email})
	}

	// created_at sort keeps the plain SQL path (no decrypt-scan) and still works.
	got, _, err = store.List(ctx, customer.ListFilter{
		TenantID: tenantID, Sort: "created_at", SortDir: "asc", Limit: 10,
	})
	if err != nil {
		t.Fatalf("List sort=created_at: %v", err)
	}
	if len(got) != 5 || got[0].DisplayName != "Zebra Systems" {
		t.Fatalf("created_at asc first row = %q, want the first-seeded Zebra Systems", got[0].DisplayName)
	}
}

// P11 DoD: the picker's server search finds customers BEYOND the
// default 50-row page — the property that makes the create-subscription
// picker (and every other server-searched picker) able to select any
// customer. Seeds 60, searches for the 57th.
func TestPostgresStore_SearchBeyondFirstPage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	store.SetEncryptor(enc)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Search Beyond 50")

	for i := 0; i < 60; i++ {
		if _, err := store.Create(ctx, tenantID, domain.Customer{
			ExternalID:  fmt.Sprintf("ext-%03d", i),
			DisplayName: fmt.Sprintf("Padding Corp %03d", i),
			Email:       fmt.Sprintf("pad%03d@example.test", i),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// Default page misses row 57 by construction (50-row cap, name order
	// irrelevant — the point is the picker can't rely on one page).
	res, _, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Search: "Padding Corp 057", Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].DisplayName != "Padding Corp 057" {
		t.Fatalf("search beyond page: got %d rows (%v), want exactly Padding Corp 057", len(res), res)
	}
}
