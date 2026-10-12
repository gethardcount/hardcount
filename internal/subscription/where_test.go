package subscription

import (
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

func TestBuildSubWhere_Search(t *testing.T) {
	where, args := buildSubWhere(ListFilter{Search: "acme"})
	if !strings.Contains(where, "(s.display_name ILIKE $1 OR s.code ILIKE $1)") {
		t.Fatalf("want display_name/code ILIKE clause reusing one placeholder, got %q", where)
	}
	if len(args) != 1 || args[0] != "%acme%" {
		t.Fatalf("want single pattern arg %%acme%%, got %v", args)
	}
}

func TestBuildSubWhere_SearchEscapesLikeMeta(t *testing.T) {
	_, args := buildSubWhere(ListFilter{Search: "50%_off"})
	want := `%50\%\_off%`
	if len(args) != 1 || args[0] != want {
		t.Fatalf("want escaped pattern %q, got %v", want, args)
	}
}

func TestBuildSubWhere_SearchComposesWithOtherFilters(t *testing.T) {
	where, args := buildSubWhere(ListFilter{
		CustomerID: "cus_1",
		Status:     "active",
		Search:     "pro",
	})
	if !strings.Contains(where, "s.customer_id = $1") ||
		!strings.Contains(where, "s.status = $2") ||
		!strings.Contains(where, "ILIKE $3") {
		t.Fatalf("placeholder numbering wrong: %q", where)
	}
	if len(args) != 3 {
		t.Fatalf("want 3 args, got %v", args)
	}
}

// display_name is a real column on subscriptions (subCols) — the sort
// key must map to it, not silently proxy to created_at.
func TestSubscriptionSortColumn_DisplayName(t *testing.T) {
	if got := subscriptionSortColumn("display_name"); got != "s.display_name" {
		t.Fatalf("display_name sort = %q, want s.display_name", got)
	}
	if got := subscriptionSortColumn("name"); got != "s.display_name" {
		t.Fatalf("name sort = %q, want s.display_name", got)
	}
}

// TestBuildSubWhere_BillingFailing: ?billing=failing appends the shared
// predicate (no placeholder of its own) and composes with the other filters
// without shifting their placeholders.
func TestBuildSubWhere_BillingFailing(t *testing.T) {
	where, args := buildSubWhere(ListFilter{Status: "active", Billing: ListBillingFailing, Search: "acme"})
	if !strings.Contains(where, domain.SubscriptionBillingFailingSQL("s")) {
		t.Fatalf("where lacks the shared failing predicate: %s", where)
	}
	if !strings.Contains(where, "s.status = $1") || !strings.Contains(where, "ILIKE $2") || len(args) != 2 {
		t.Fatalf("placeholders shifted: %s / %v", where, args)
	}
	// Any other value is not a filter; the handler rejects it before here.
	if where, _ := buildSubWhere(ListFilter{Billing: "healthy"}); strings.Contains(where, "billing_failed_for") {
		t.Fatalf("unknown billing value filtered: %s", where)
	}
}
