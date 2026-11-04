// hardcount-bench-seed prepares a database for an ingest benchmark and prints
// the credentials the load generator needs. It does not generate load.
//
// k6 generates the load (scripts/bench-rig/ingest.js). This binary does the
// two things k6 cannot do, and both need Hardcount's own code. It creates the
// bench tenant, customers and meter in the right livemode partition. It also
// mints an API key through auth, which owns the salt-and-hash format. A
// hand-rolled copy of that format would stop matching without a word.
//
// Usage:
//
//	DATABASE_URL="postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable" \
//	  go run ./cmd/hardcount-bench-seed
//
// Prints a JSON object to stdout:
//
//	{"api_key":"hc_…","event_name":"bench_tokens",
//	 "customer_count":"200","customer_id_prefix":"hc_cus_bench_",
//	 "external_customer_id_prefix":"bench-customer-",
//	 "external_customer_id":"bench-customer-000","customer_id":"hc_cus_bench_000"}
//
// It creates BENCH_CUSTOMERS customers (default 200), not one. With a single
// customer the run is a pathological case, not a benchmark. Every event lands
// on one customer_id, so the resolve cache never misses and the btrees append
// to one hot edge. Worst of all, the responsiveness probe's per-customer
// usage aggregate reads every row in the table for its one customer. At 1.1M
// rows that already costs ~180ms on a warm laptop. On the AWS rig the probe
// would then report the product as degraded, for a reason no real tenant
// would hit. Customers are numbered NNN, so the load generator can pick one
// arithmetically.
//
// It is idempotent, so it is safe to re-run. The fixtures are test-mode
// (livemode=false), so a key minted here authenticates into the same
// partition the fixtures live in. A key minted in the other mode sees no
// bench data.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

const (
	benchTenant                 = "hc_ten_bench"
	benchMeter                  = "hc_mtr_bench"
	benchMeterKey               = "bench_tokens"
	benchCustomerIDPrefix       = "hc_cus_bench_"
	benchCustomerExternalPrefix = "bench-customer-"
	defaultBenchCustomers       = 200
)

func benchLivemode() bool {
	switch os.Getenv("BENCH_LIVEMODE") {
	case "", "true", "1", "live":
		return true
	case "false", "0", "test":
		return false
	}
	log.Fatalf("BENCH_LIVEMODE must be true|false, got %q", os.Getenv("BENCH_LIVEMODE"))
	return true
}

func benchCustomerCount() int {
	if v := os.Getenv("BENCH_CUSTOMERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Fatalf("BENCH_CUSTOMERS must be a positive integer, got %q", v)
	}
	return defaultBenchCustomers
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()

	db := postgres.NewDB(pool, 30*time.Second)

	// BENCH_LIVEMODE selects which ingest path the benchmark measures, and
	// the default is the production one. Test mode is more than a label. In
	// test mode the service does an extra test-clock lookup per event, which
	// live mode skips by design ("the high-volume production ingest path
	// stays at zero extra queries", simNow in internal/usage/service.go). So
	// every number measured in test mode includes a query production never
	// makes. The bench database is throwaway, so live mode here touches no
	// money.
	live := benchLivemode()
	// The key-minting path reads WithLivemode (auth opens a TxTenant). It
	// does not reach the fixture INSERTs: see the note in bootstrapFixtures.
	ctx := postgres.WithLivemode(context.Background(), live)

	n := benchCustomerCount()
	bootstrapFixtures(ctx, db, n, live)

	out := map[string]string{
		"api_key":                     mintBenchAPIKey(ctx, db, live),
		"livemode":                    strconv.FormatBool(live),
		"event_name":                  benchMeterKey,
		"customer_count":              strconv.Itoa(n),
		"customer_id_prefix":          benchCustomerIDPrefix,
		"external_customer_id_prefix": benchCustomerExternalPrefix,
		// The first customer, for callers that need only one (bringup's
		// smoke test). /v1/usage-events/aggregate?customer_id= takes the
		// internal id, not the external one.
		"external_customer_id": fmt.Sprintf("%s%03d", benchCustomerExternalPrefix, 0),
		"customer_id":          fmt.Sprintf("%s%03d", benchCustomerIDPrefix, 0),
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatalf("write credentials: %v", err)
	}
}

// bootstrapFixtures makes sure the benchmark tenant, customers and meter
// exist. It is idempotent. It uses TxBypass because this is a CLI tool with
// full database access. The runtime path sets tenant_id per request with
// TxTenant, as usual.
func bootstrapFixtures(ctx context.Context, db *postgres.DB, customers int, live bool) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		log.Fatalf("begin bootstrap: %v", err)
	}
	defer postgres.Rollback(tx)

	// Set app.livemode on this transaction explicitly.
	//
	// `customers` and `meters` carry the same BEFORE INSERT set_livemode
	// trigger as usage_events. The trigger overwrites whatever livemode the
	// INSERT supplied with the app.livemode setting, and refuses the insert
	// when the setting is unset (migration 00213). BeginTx sets that setting
	// only for TxTenant: the TxBypass branch sets app.bypass_rls and nothing
	// else. So postgres.WithLivemode on the context has no effect here.
	//
	// The trigger runs only on INSERT. On a re-run, the ON CONFLICT DO
	// UPDATE paths below set livemode from their own parameter, so a reused
	// database can hide a wrong app.livemode that a fresh database shows.
	mode := "off"
	if live {
		mode = "on"
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
		log.Fatalf("set livemode on bootstrap tx: %v", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, status) VALUES ($1, 'hardcount-bench', 'active')
		ON CONFLICT (id) DO NOTHING
	`, benchTenant); err != nil {
		log.Fatalf("upsert tenant: %v", err)
	}

	// One statement for all customers, not a loop of round trips.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, livemode)
		SELECT $1 || lpad(g::text, 3, '0'),
		       $2,
		       $3 || lpad(g::text, 3, '0'),
		       'Bench Customer ' || g,
		       'bench-' || g || '@hardcount.local',
		       $5
		FROM generate_series(0, $4 - 1) g
		ON CONFLICT (id) DO UPDATE SET livemode = $5
	`, benchCustomerIDPrefix, benchTenant, benchCustomerExternalPrefix, customers, live); err != nil {
		log.Fatalf("upsert customers: %v", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO meters (id, tenant_id, name, event_name, unit, aggregation, livemode)
		VALUES ($1, $2, 'Bench Tokens', $3, 'tokens', $4, $5)
		ON CONFLICT (id) DO UPDATE SET livemode = $5
	`, benchMeter, benchTenant, benchMeterKey, domain.AggregationSum, live); err != nil {
		log.Fatalf("upsert meter: %v", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("commit bootstrap: %v", err)
	}
}

// mintBenchAPIKey creates a test-mode secret key scoped to the bench tenant
// and returns the raw value. It goes through the real auth service instead
// of inserting a row by hand. The salt-and-hash format belongs to auth, and a
// hand-rolled copy here would stop matching without a word on the day that
// format changes.
func mintBenchAPIKey(ctx context.Context, db *postgres.DB, live bool) string {
	svc := auth.NewService(auth.NewPostgresStore(db))
	// Mint the key in the same mode as the fixtures. Otherwise it
	// authenticates into the other partition and sees no bench data.
	res, err := svc.CreateKey(postgres.WithLivemode(ctx, live), benchTenant, auth.CreateKeyInput{
		Name: "hardcount-bench", KeyType: auth.KeyTypeSecret,
	})
	if err != nil {
		log.Fatalf("mint bench api key: %v", err)
	}
	return res.RawKey
}
