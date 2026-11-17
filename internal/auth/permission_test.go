package auth

import (
	"slices"
	"testing"
)

// Each key type has a permission entry. Mutation: add a type to keyTypes
// without an entry in keyPermissions, and this fails.
// TestKeyShape_ParserAndTableAgree checks each type's prefixes.
func TestKeyTypes_EachHasPermissions(t *testing.T) {
	for _, kt := range keyTypes {
		if _, ok := keyPermissions[kt]; !ok {
			t.Errorf("%s has no permission entry", kt)
		}
	}
}

// A session holds every permission and a key its type's, even with a user
// named on it too. A caller with no key type (a background job's context, or
// a request nothing authenticated) or an unknown one holds none.
func TestHasPermission_BySessionOrKeyType(t *testing.T) {
	anIngestKey := Caller{TenantID: "hc_ten_1", KeyID: "hc_key_1", KeyType: KeyTypeIngest}
	for _, c := range []struct {
		name   string
		caller Caller
		holds  []Permission // and no other
	}{
		{"a session", aSession, allPermissions},
		{"a secret key", aKey, allPermissions},
		{"an ingest key", anIngestKey, []Permission{PermUsageIngest}},
		{"an ingest key with a user named", aKeyWithAUser, []Permission{PermUsageIngest}},
		{"a tenant only", aTenantOnly, nil},
		{"the removed platform type", Caller{TenantID: "hc_ten_1", KeyType: "platform"}, nil},
		{"the removed publishable type", Caller{TenantID: "hc_ten_1", KeyType: "publishable"}, nil},
		{"a type that never existed", Caller{TenantID: "hc_ten_1", KeyType: "nonexistent"}, nil},
	} {
		for _, p := range allPermissions {
			if got, want := hasPermission(c.caller, p), slices.Contains(c.holds, p); got != want {
				t.Errorf("%s: hasPermission(%s) = %v, want %v", c.name, p, got, want)
			}
		}
	}
}
