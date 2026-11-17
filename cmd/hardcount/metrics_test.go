package main

import (
	"database/sql"
	"os"
	"regexp"
	"sort"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// monitoringPage is the operator's list of metrics: what each counts and what
// to do when it moves.
const monitoringPage = "../../docs/ops/runbook/monitoring.md"

var (
	hardcountName = regexp.MustCompile(`hardcount_[a-z0-9_]+`)
	// A Desc has no exported name accessor; its String form starts with it.
	descName = regexp.MustCompile(`^Desc\{fqName: "([^"]+)"`)
)

// TestMonitoringPage_NamesEveryExportedMetric fails when the server exports a
// hardcount_ metric that docs/ops/runbook/monitoring.md does not name, or when
// the page names one the server does not export. The names come from the
// registry /metrics serves, after the same registration the server runs at
// boot, so a vector with no series yet is still seen.
func TestMonitoringPage_NamesEveryExportedMetric(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://localhost/unused") // never connected: registering queries nothing
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	registerMetrics(pool, postgres.NewDB(pool, time.Second), time.Hour)

	descs := make(chan *prometheus.Desc)
	go func() {
		prometheus.DefaultRegisterer.(*prometheus.Registry).Describe(descs)
		close(descs)
	}()
	exported := map[string]bool{}
	for d := range descs {
		m := descName.FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("cannot read a metric name from %s", d)
		}
		if hardcountName.MatchString(m[1]) {
			exported[m[1]] = true
		}
	}

	page, err := os.ReadFile(monitoringPage)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, n := range hardcountName.FindAllString(string(page), -1) {
		named[n] = true
	}

	for _, n := range missing(exported, named) {
		t.Errorf("%s is exported but %s does not name it: add what it counts and what to do when it moves", n, monitoringPage)
	}
	for _, n := range missing(named, exported) {
		t.Errorf("%s names %s, which the server does not export", monitoringPage, n)
	}
}

// missing returns the names in want that are not in have, sorted.
func missing(want, have map[string]bool) []string {
	var out []string
	for n := range want {
		if !have[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
