package auth

import (
	"slices"
	"testing"
)

// everyPermission is every permission a route can require.
var everyPermission = []Permission{
	PermCustomerRead, PermCustomerWrite, PermPricingRead, PermPricingWrite,
	PermSubscriptionRead, PermSubscriptionWrite, PermUsageRead, PermUsageWrite,
	PermUsageIngest,
	PermInvoiceRead, PermInvoiceWrite, PermDunningRead, PermDunningWrite,
	PermAPIKeyRead, PermAPIKeyWrite,
	PermTestClockWrite,
}

// A secret key and a dashboard session hold every permission, the usage
// ingest one included, so both still send usage.
func TestHasPermission_SecretAndSessionHoldAll(t *testing.T) {
	for _, kt := range []KeyType{KeyTypeSecret, KeyTypeSession} {
		for _, p := range everyPermission {
			if !HasPermission(kt, p) {
				t.Errorf("%s should have %s", kt, p)
			}
		}
		if got := len(AllPermissions(kt)); got != len(everyPermission) {
			t.Errorf("%s has %d permissions, want %d", kt, got, len(everyPermission))
		}
	}
}

// An ingest key holds exactly one permission: sending usage (ADR-133).
// Mutation: give it any other permission, such as usage:write (which
// gates backfill and the held-usage release), and this fails.
func TestHasPermission_IngestHoldsOnlyUsageIngest(t *testing.T) {
	if got := AllPermissions(KeyTypeIngest); !slices.Equal(got, []Permission{PermUsageIngest}) {
		t.Fatalf("ingest key permissions = %v, want only %s", got, PermUsageIngest)
	}
	for _, p := range everyPermission {
		if got, want := HasPermission(KeyTypeIngest, p), p == PermUsageIngest; got != want {
			t.Errorf("ingest key %s = %v, want %v", p, got, want)
		}
	}
}

func TestHasPermission_UnknownType(t *testing.T) {
	for _, kt := range []KeyType{"nonexistent", "platform", "publishable", ""} {
		for _, p := range everyPermission {
			if HasPermission(kt, p) {
				t.Errorf("key type %q should have no permissions, has %s", kt, p)
			}
		}
	}
}
