package audit_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
)

// TestResolveActor pins who the audit log names: a customer first, then the
// caller as auth.Caller.Actor names it, then system.
func TestResolveActor(t *testing.T) {
	cases := []struct {
		name      string
		ctx       context.Context
		wantType  string
		wantActor string
	}{
		{
			name:      "session operator → user",
			ctx:       auth.WithUserID(context.Background(), "usr_123"),
			wantType:  "user",
			wantActor: "usr_123",
		},
		{
			name:      "api key → api_key",
			ctx:       auth.WithCaller(context.Background(), auth.Caller{KeyID: "hc_secret_abc"}),
			wantType:  "api_key",
			wantActor: "hc_secret_abc",
		},
		{
			name:      "customer token → customer",
			ctx:       auth.WithCustomerActor(context.Background(), "hc_cus_9"),
			wantType:  "customer",
			wantActor: "hc_cus_9",
		},
		{
			name:      "background worker → system",
			ctx:       context.Background(),
			wantType:  "system",
			wantActor: "system",
		},
		{
			name:      "customer beats the caller",
			ctx:       auth.WithCustomerActor(auth.WithCaller(context.Background(), auth.Caller{UserID: "usr_1", KeyID: "hc_key"}), "hc_cus_1"),
			wantType:  "customer",
			wantActor: "hc_cus_1",
		},
		{
			name:      "a key beats a user named on it",
			ctx:       auth.WithCaller(context.Background(), auth.Caller{UserID: "usr_2", KeyID: "hc_key_2"}),
			wantType:  "api_key",
			wantActor: "hc_key_2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotActor := audit.ResolveActor(tc.ctx)
			if gotType != tc.wantType || gotActor != tc.wantActor {
				t.Errorf("ResolveActor = (%q, %q), want (%q, %q)", gotType, gotActor, tc.wantType, tc.wantActor)
			}
		})
	}
}
