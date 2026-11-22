package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// notMetrics are hardcount_ words the runbooks use that name something other
// than a metric.
var notMetrics = map[string]string{
	"hardcount_app":         "the app's database role",
	"hardcount_cn_id":       "a Stripe metadata key",
	"hardcount_customer_id": "a Stripe metadata key",
	"hardcount_invoice_id":  "a Stripe metadata key",
	"hardcount_ids":         "a temporary table in a runbook query",
}

// TestMonitoringPage_NamesEveryExportedMetric fails when the server exports a
// hardcount_ metric that docs/ops/runbook/monitoring.md does not name, or when
// that page names one the server does not export.
func TestMonitoringPage_NamesEveryExportedMetric(t *testing.T) {
	exported := exportedNames(t)
	named := namesIn(t, monitoringPage)

	for _, n := range missing(exported, named) {
		t.Errorf("%s is exported but %s does not name it: add what it counts and what to do when it moves", n, monitoringPage)
	}
	for _, n := range missing(named, exported) {
		t.Errorf("%s names %s, which the server does not export", monitoringPage, n)
	}
}

// TestRunbooksAndDashboard_NameOnlyExportedMetrics fails when a runbook or the
// bundled Grafana dashboard names a hardcount_ metric the server does not
// export: an alert or a query on it would match nothing. A histogram's
// _bucket, _count and _sum series count as its name.
func TestRunbooksAndDashboard_NameOnlyExportedMetrics(t *testing.T) {
	exported := exportedNames(t)
	files, err := filepath.Glob("../../docs/ops/runbook/*.md")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "../../docs/ops/grafana-dashboard.json")

	for _, f := range files {
		for n := range namesIn(t, f) {
			base := n
			for _, suffix := range []string{"_bucket", "_count", "_sum"} {
				if b, ok := strings.CutSuffix(n, suffix); ok && exported[b] {
					base = b
				}
			}
			if !exported[base] && notMetrics[n] == "" {
				t.Errorf("%s names %s, which the server does not export", f, n)
			}
		}
	}
}

// exportedNames returns every hardcount_ metric name the server serves on
// /metrics, whether or not it has a series yet. Packages register their own
// metrics on the default registry when they load; the boot-time collectors
// go on a fresh registry here, so the test can run more than once.
func exportedNames(t *testing.T) map[string]bool {
	t.Helper()
	pool, err := sql.Open("pgx", "postgres://localhost/unused") // never connected: registering queries nothing
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	boot := prometheus.NewRegistry()
	registerMetrics(boot, pool, postgres.NewDB(pool, time.Second), time.Hour)

	names := map[string]bool{}
	for _, reg := range []*prometheus.Registry{prometheus.DefaultRegisterer.(*prometheus.Registry), boot} {
		descs := make(chan *prometheus.Desc)
		go func() {
			reg.Describe(descs)
			close(descs)
		}()
		for d := range descs {
			m := descName.FindStringSubmatch(d.String())
			if m == nil {
				t.Fatalf("cannot read a metric name from %s", d)
			}
			if hardcountName.MatchString(m[1]) {
				names[m[1]] = true
			}
		}
	}
	return names
}

// namesIn returns the hardcount_ words in the file at path.
func namesIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range hardcountName.FindAllString(string(b), -1) {
		names[n] = true
	}
	return names
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

// TestAlertedCounterSeries_ExistAtZero: every dead-letter and unpriced-usage
// series an alert reads is exported at 0 from boot. A counter series born at
// 1 has no earlier sample, so increase() reads it as 0 and the alert misses
// the first lost email or the first usage left off an invoice. Prometheus: "export a default value such
// as 0 for any time series you know may exist".
func TestAlertedCounterSeries_ExistAtZero(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	have := map[string]bool{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			key := f.GetName()
			for _, l := range m.GetLabel() {
				key += "," + l.GetName() + "=" + l.GetValue()
			}
			have[key] = true
		}
	}
	for _, want := range []string{
		"hardcount_email_outbox_dead_lettered_total,cause=permanent,mode=live",
		"hardcount_email_outbox_dead_lettered_total,cause=permanent,mode=test",
		"hardcount_email_outbox_dead_lettered_total,cause=max_attempts,mode=live",
		"hardcount_email_outbox_dead_lettered_total,cause=max_attempts,mode=test",
		"hardcount_webhook_outbox_dead_lettered_total,cause=max_attempts,mode=live",
		"hardcount_webhook_outbox_dead_lettered_total,cause=max_attempts,mode=test",
		"hardcount_usage_unpriced_quantity_total,mode=live",
		"hardcount_usage_unpriced_quantity_total,mode=test",
	} {
		if !have[want] {
			t.Errorf("series %s is not exported before its first increment", want)
		}
	}
}
