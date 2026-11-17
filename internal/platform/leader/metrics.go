package leader

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var leaderLeaseLost = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "hardcount_leader_lease_lost_total",
		Help: "Led ticks that ended without completing (ADR-114), by role and reason. heartbeat_timeout = the holder could not renew within the abandon window and cancelled its own work; takeover = a renew found another holder; released = a renew found the row released; paused = an operator paused the role mid-tick (expected — exclude from paging); panicked = the tick's work panicked and the lease was released as not completed; in billing and dunning only that step or row stopped and the rest of the tick ran (stack on the first 'step panicked; running the rest' line; for the other roles on 'scheduler panic recovered'). It repeats one interval later until the input is fixed, except a panic while billing one subscription, which repeats only when that subscription's retry wait ends; for a dunning run that panics, pause that run (runbook background-job-stuck). For lost leases correctness held via fence + row CAS; the cause (frozen process, DB stall, pooler) is what to look at.",
	},
	[]string{"role", "reason"},
)

// RecordLeaseLost counts a lost lease (leader.LostFunc adapter).
func RecordLeaseLost(role, reason string) {
	leaderLeaseLost.WithLabelValues(role, reason).Inc()
}

// InitLeaseLost exports hardcount_leader_lease_lost_total{role,
// reason="panicked"} at 0 for each role, at boot. The alert that pages is
// increase(...[1h]) > 0, and a series first exported at 1 has no earlier
// sample. Without the 0, the first panicked tick after a start would never
// page. Prometheus advises exporting a default value, such as 0, for any time
// series you know may exist. Only "panicked" is created. A heartbeat_timeout
// series created now would page on one timeout, and that waits for the
// heartbeat fix, RL-F2. ADR-114 records the same rule in its amendment
// 2026-10-19, "a panic stops one step", item 6.
func InitLeaseLost(roles ...string) {
	for _, role := range roles {
		leaderLeaseLost.WithLabelValues(role, "panicked")
	}
}

// leaderStatusCollector exposes leader_leases as gauges, with a query on
// every scrape. It does not cache, because a cached "last tick 30s ago" is
// exactly the false value an operator alerts on.
// hardcount_leader_last_tick_age_seconds is the cluster fact: when the role
// last finished a tick on any replica. The per-replica
// hardcount_scheduler_last_run_timestamp_seconds only says that this
// replica's loop is alive.
type leaderStatusCollector struct {
	query        func(context.Context) ([]StatusRow, error)
	age          *prometheus.Desc
	held         *prometheus.Desc
	paused       *prometheus.Desc
	scrapeFailed *prometheus.Desc
}

// RegisterStatusMetrics wires the leader_status collector. Call it once, at
// boot. A second call panics on the duplicate registration, which is the
// right outcome for wiring it twice.
func RegisterStatusMetrics(reg prometheus.Registerer, query func(context.Context) ([]StatusRow, error)) {
	reg.MustRegister(&leaderStatusCollector{
		query:        query,
		age:          prometheus.NewDesc("hardcount_leader_last_tick_age_seconds", "Seconds since the role's last completed tick on ANY replica (leader_leases.last_tick_ended_at). Absent until the role has ticked once. Alert when it exceeds a few multiples of the role's interval — that is the cluster-wide stall signal a single replica's liveness gauge cannot give.", []string{"role"}, nil),
		held:         prometheus.NewDesc("hardcount_leader_held", "1 while some replica holds the role's lease (a tick is running), else 0.", []string{"role"}, nil),
		paused:       prometheus.NewDesc("hardcount_leader_paused", "1 while the role is paused by an operator (leader_pause), else 0.", []string{"role"}, nil),
		scrapeFailed: prometheus.NewDesc("hardcount_leader_status_scrape_failed", "1 when this scrape could not read leader_status, else 0. While it is 1, hardcount_leader_last_tick_age_seconds, hardcount_leader_held and hardcount_leader_paused are absent. The stalled-role alert cannot fire while it is 1: check that the database answers the leader_status query.", nil, nil),
	})
}

func (c *leaderStatusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.age
	ch <- c.held
	ch <- c.paused
	ch <- c.scrapeFailed
}

func (c *leaderStatusCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.query(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.scrapeFailed, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeFailed, prometheus.GaugeValue, 0)
	for _, r := range rows {
		if r.HasTicked {
			ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, r.LastTickAgeS, string(r.Role))
		}
		ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, b2f(r.Held), string(r.Role))
		ch <- prometheus.MustNewConstMetric(c.paused, prometheus.GaugeValue, b2f(r.Paused), string(r.Role))
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
