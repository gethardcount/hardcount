package auth

import (
	"slices"
	"testing"
)

// Each stored key type has a permission entry, and the session type, which
// no key backs, is not stored. Mutation: add a type to storedKeyTypes without
// an entry in keyPermissions, and this fails. Its type prefixes are checked
// against the parser and the table in TestKeyShape_ParserAndTableAgree.
func TestStoredKeyTypes_EachHasPrefixesAndPermissions(t *testing.T) {
	for _, kt := range storedKeyTypes {
		if _, ok := keyPermissions[kt]; !ok {
			t.Errorf("%s has no permission entry", kt)
		}
	}
	if slices.Contains(storedKeyTypes, KeyTypeSession) {
		t.Errorf("storedKeyTypes holds %s, which no key backs", KeyTypeSession)
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
