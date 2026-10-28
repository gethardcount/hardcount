package auth

import "testing"

func TestHasPermission_SecretHasAll(t *testing.T) {
	allPerms := []Permission{
		PermCustomerRead, PermCustomerWrite, PermPricingRead, PermPricingWrite,
		PermSubscriptionRead, PermSubscriptionWrite, PermUsageRead, PermUsageWrite,
		PermInvoiceRead, PermInvoiceWrite, PermDunningRead, PermDunningWrite,
		PermAPIKeyRead, PermAPIKeyWrite,
		PermTestClockWrite,
	}
	for _, p := range allPerms {
		if !HasPermission(KeyTypeSecret, p) {
			t.Errorf("secret should have %s", p)
		}
	}
	// Secret should NOT have tenant management
	if HasPermission(KeyTypeSecret, PermTenantWrite) {
		t.Error("secret should NOT have tenant:write")
	}
}

func TestHasPermission_PlatformOnlyTenants(t *testing.T) {
	if !HasPermission(KeyTypePlatform, PermTenantRead) {
		t.Error("platform should have tenant:read")
	}
	if !HasPermission(KeyTypePlatform, PermTenantWrite) {
		t.Error("platform should have tenant:write")
	}
	if HasPermission(KeyTypePlatform, PermCustomerRead) {
		t.Error("platform should NOT have customer:read")
	}
}

func TestHasPermission_UnknownType(t *testing.T) {
	if HasPermission("nonexistent", PermCustomerRead) {
		t.Error("unknown type should have no permissions")
	}
}

func TestAllPermissions(t *testing.T) {
	secretPerms := AllPermissions(KeyTypeSecret)
	if len(secretPerms) != 15 {
		t.Errorf("secret should have 15 permissions, got %d", len(secretPerms))
	}

	platformPerms := AllPermissions(KeyTypePlatform)
	if len(platformPerms) != 2 {
		t.Errorf("platform should have 2 permissions, got %d", len(platformPerms))
	}
}
