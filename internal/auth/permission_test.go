package auth

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// Each stored key type has the type prefixes the parser reads, live and test,
// and a permission entry. Mutation: add a type to storedKeyTypes without an
// entry in keyPermissions, and this fails; its prefix then also fails the
// table's CHECK (TestKeyShape_ParserAndTableAgree) until a migration widens it.
func TestStoredKeyTypes_EachHasPrefixesAndPermissions(t *testing.T) {
	for _, kt := range storedKeyTypes {
		for _, live := range []bool{true, false} {
			p := typePrefix(kt, live)
			if !slices.Contains(typePrefixes, p) {
				t.Errorf("%s: type prefix %s is not one the parser reads", kt, p)
			}
			if _, ok := lookupPrefix(p + strings.Repeat("0", 2*secretBytes)); !ok {
				t.Errorf("%s: the parser refuses a key with prefix %s", kt, p)
			}
		}
		if _, ok := keyPermissions[kt]; !ok {
			t.Errorf("%s has no permission entry", kt)
		}
	}
	if slices.Contains(storedKeyTypes, KeyTypeSession) {
		t.Errorf("storedKeyTypes holds %s, which no key backs", KeyTypeSession)
	}
}

// A secret key and a dashboard session hold every permission, the usage
// ingest one included, so both still send usage.
func TestHasPermission_SecretAndSessionHoldAll(t *testing.T) {
	for _, kt := range []KeyType{KeyTypeSecret, KeyTypeSession} {
		for _, p := range allPermissions {
			if !HasPermission(kt, p) {
				t.Errorf("%s should have %s", kt, p)
			}
		}
		if got := len(keyPermissions[kt]); got != len(allPermissions) {
			t.Errorf("%s has %d permissions, want %d", kt, got, len(allPermissions))
		}
	}
}

// An ingest key holds exactly one permission: sending usage (ADR-133).
// Mutation: give it any other permission, such as usage:write (which
// gates backfill and the held-usage release), and this fails.
func TestHasPermission_IngestHoldsOnlyUsageIngest(t *testing.T) {
	if got := slices.Collect(maps.Keys(keyPermissions[KeyTypeIngest])); !slices.Equal(got, []Permission{PermUsageIngest}) {
		t.Fatalf("ingest key permissions = %v, want only %s", got, PermUsageIngest)
	}
	for _, p := range allPermissions {
		if got, want := HasPermission(KeyTypeIngest, p), p == PermUsageIngest; got != want {
			t.Errorf("ingest key %s = %v, want %v", p, got, want)
		}
	}
}

func TestHasPermission_UnknownType(t *testing.T) {
	for _, kt := range []KeyType{"nonexistent", "platform", "publishable", ""} {
		for _, p := range allPermissions {
			if HasPermission(kt, p) {
				t.Errorf("key type %q should have no permissions, has %s", kt, p)
			}
		}
	}
}
