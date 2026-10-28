package middleware

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
)

// TestInitLeaderLeaseLost_PanickedSeriesStartAtZero: the page row is
// increase(hardcount_leader_lease_lost_total[1h]) > 0. A counter series that is
// first exported at 1 has no earlier sample, so increase() over it is empty
// and the first panicked tick after a start never paged. Init exports every
// role's reason="panicked" series at 0, so that first panic is an increase.
// It creates no other reason: once a heartbeat_timeout series exists, one
// timeout pages, and that waits for the heartbeat fix (RL-F2).
func TestInitLeaderLeaseLost_PanickedSeriesStartAtZero(t *testing.T) {
	roles := make([]string, 0, len(leader.Roles))
	for _, r := range leader.Roles {
		roles = append(roles, string(r))
	}
	InitLeaderLeaseLost(roles...)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(leaderLeaseLost)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]float64{}
	for _, fam := range families {
		if fam.GetName() != "hardcount_leader_lease_lost_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["reason"] != "panicked" {
				t.Errorf("series %v was created; only reason=\"panicked\" is pre-created", labels)
				continue
			}
			seen[labels["role"]] = m.GetCounter().GetValue()
		}
	}
	for _, role := range roles {
		v, ok := seen[role]
		if !ok {
			t.Errorf("no hardcount_leader_lease_lost_total{role=%q, reason=\"panicked\"} series after init", role)
		} else if v != 0 {
			t.Errorf("role %q: panicked series = %v, want 0", role, v)
		}
	}
}
