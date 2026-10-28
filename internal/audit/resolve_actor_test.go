package audit_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
)

// TestResolveActor pins the actor-attribution priority that makes the audit log
// able to answer "who did this?" — customer > user > api_key > system. The
// load-bearing case is `user`: a dashboard operator on a session cookie
// (auth.WithUserID, no key id) must resolve to actor_type='user', not the
// 'system' fallback that the pre-fix KeyID-only path produced.
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
			ctx:       auth.WithKeyID(context.Background(), "hc_secret_abc"),
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
			name:      "customer beats user beats key",
			ctx:       auth.WithCustomerActor(auth.WithKeyID(auth.WithUserID(context.Background(), "usr_1"), "hc_key"), "hc_cus_1"),
			wantType:  "customer",
			wantActor: "hc_cus_1",
		},
		{
			name:      "user beats key when both present",
			ctx:       auth.WithKeyID(auth.WithUserID(context.Background(), "usr_2"), "hc_key_2"),
			wantType:  "user",
			wantActor: "usr_2",
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
