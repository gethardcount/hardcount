package auth

import "testing"

// The callers production builds: a session, a key, and a user alone (an
// account route's WithUserID). A key with a user named is a shape nothing
// builds; it is here to show the key decides.
var (
	aSession      = Caller{TenantID: "hc_ten_1", UserID: "hc_usr_1"}
	aKey          = Caller{TenantID: "hc_ten_1", KeyID: "hc_key_1", KeyType: KeyTypeSecret}
	aKeyWithAUser = Caller{TenantID: "hc_ten_1", UserID: "hc_usr_1", KeyID: "hc_key_1", KeyType: KeyTypeIngest}
	aUserOnly     = Caller{UserID: "hc_usr_1"}
)

// A session is a user and no key; a caller with a key is never a session.
func TestCaller_IsSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller Caller
		want   bool
	}{
		{"a session", aSession, true},
		{"a key", aKey, false},
		{"a key with a user named", aKeyWithAUser, false},
		{"a user only", aUserOnly, true},
		{"the zero caller", Caller{}, false},
	} {
		if got := tc.caller.IsSession(); got != tc.want {
			t.Errorf("%s: IsSession = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A session is named as its user, and a key as the key whoever else is named
// on it. A caller that is neither has no name.
func TestCaller_Actor(t *testing.T) {
	for _, tc := range []struct {
		name             string
		caller           Caller
		wantType, wantID string
	}{
		{"a session", aSession, "user", "hc_usr_1"},
		{"a key", aKey, "api_key", "hc_key_1"},
		{"a key with a user named", aKeyWithAUser, "api_key", "hc_key_1"},
		{"a user only", aUserOnly, "user", "hc_usr_1"},
		{"the zero caller", Caller{}, "", ""},
	} {
		if gotType, gotID := tc.caller.Actor(); gotType != tc.wantType || gotID != tc.wantID {
			t.Errorf("%s: Actor = %q, %q; want %q, %q", tc.name, gotType, gotID, tc.wantType, tc.wantID)
		}
	}
}

// WithTenantID and WithUserID each set one field of the caller in ctx and keep
// the others. Every field is set, so a setter that drops one fails.
func TestWithTenantIDAndWithUserID_KeepTheOtherFields(t *testing.T) {
	ctx := WithCaller(t.Context(), aKeyWithAUser)

	wantTenant := aKeyWithAUser
	wantTenant.TenantID = "hc_ten_2"
	if got := CallerOf(WithTenantID(ctx, "hc_ten_2")); got != wantTenant {
		t.Errorf("WithTenantID: caller = %+v, want %+v", got, wantTenant)
	}

	wantUser := aKeyWithAUser
	wantUser.UserID = "hc_usr_2"
	if got := CallerOf(WithUserID(ctx, "hc_usr_2")); got != wantUser {
		t.Errorf("WithUserID: caller = %+v, want %+v", got, wantUser)
	}
}
