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
func TestHasPermission_UnknownTypeAndNoCaller(t *testing.T) {
	for _, kt := range []KeyType{"nonexistent", "platform", "publishable", ""} {
		for _, p := range allPermissions {
			if hasPermission(Caller{TenantID: "hc_ten_1", KeyType: kt}, p) {
				t.Errorf("key type %q should have no permissions, has %s", kt, p)
			}
		}
	}
}

// Who holds what: a session every permission, a key its type's. A caller with
// a key is judged as that key even when a user is named on it too.
func TestHasPermission_ByCaller(t *testing.T) {
	session := Caller{TenantID: "hc_ten_1", UserID: "hc_usr_1"}
	ingest := Caller{TenantID: "hc_ten_1", KeyID: "hc_key_1", KeyType: KeyTypeIngest}
	ingestWithUser := ingest
	ingestWithUser.UserID = "hc_usr_1"
	for _, tc := range []struct {
		name   string
		caller Caller
		perm   Permission
		want   bool
	}{
		{"a session holds a write permission", session, PermAPIKeyWrite, true},
		{"an ingest key sends usage", ingest, PermUsageIngest, true},
		{"an ingest key holds nothing else", ingest, PermAPIKeyWrite, false},
		{"an ingest key with a user named is still an ingest key", ingestWithUser, PermAPIKeyWrite, false},
	} {
		if got := hasPermission(tc.caller, tc.perm); got != tc.want {
			t.Errorf("%s: hasPermission = %v, want %v", tc.name, got, tc.want)
		}
	}
}
