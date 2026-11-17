package auth

import "testing"

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

// A key of an unknown type holds no permission. Neither does a caller with no
// user and no key (the "" type): a background job's context, or a request
// that nothing authenticated.
func TestHasPermission_UnknownType(t *testing.T) {
	for _, kt := range []KeyType{"nonexistent", "platform", "publishable", ""} {
		for _, p := range allPermissions {
			if HasPermission(Caller{TenantID: "hc_ten_1", KeyType: kt}, p) {
				t.Errorf("key type %q should have no permissions, has %s", kt, p)
			}
		}
	}
}
