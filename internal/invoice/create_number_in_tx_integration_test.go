package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestCreate_FailedCreateUsesNoNumber: a manual invoice takes its number on
// the transaction that inserts it. A create that fails in that transaction
// gives the number back, so the next invoice takes it and the tenant's
// numbers keep no gap. The failure here is the in-transaction audit row,
// which runs after the invoice and its lines are inserted.
func TestCreate_FailedCreateUsesNoNumber(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Invoice Number In Tx")
	cust := seedAuditCustomer(t, db, tenantID, "number_in_tx")
	settings := tenant.NewSettingsStore(db)
	store := invoice.NewPostgresStore(db)

	nextSeq := func() int {
		t.Helper()
		seq, err := settings.NextInvoiceSequence(ctx, tenantID)
		if err != nil {
			t.Fatalf("read next invoice sequence: %v", err)
		}
		return seq
	}
	countInvoices := func() int {
		t.Helper()
		_, total, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("list invoices: %v", err)
		}
		return total
	}

	svc := invoice.NewService(store, nil, settings)
	first, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if first.InvoiceNumber != "HC-000001" {
		t.Fatalf("first invoice number: got %q, want HC-000001", first.InvoiceNumber)
	}
	seqBefore := nextSeq()

	emitter := &failingEmitter{}
	failing := invoice.NewService(store, nil, settings)
	failing.SetAuditLogger(emitter)
	for _, tc := range []struct {
		name  string
		input invoice.CreateInput
	}{
		{"bare header", invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"}},
		{"with lines", invoice.CreateInput{CustomerID: cust.ID, Currency: "USD", LineItems: []invoice.AddLineItemInput{
			{Description: "Setup fee", Quantity: 1, UnitAmountCents: 5000},
		}}},
	} {
		if _, err := failing.Create(ctx, tenantID, tc.input); !errors.Is(err, errEmitBoom) {
			t.Fatalf("%s: want the injected audit failure; got %v", tc.name, err)
		}
	}
	if emitter.calls != 2 {
		t.Fatalf("audit emission ran %d times, want 2 (the test proves nothing otherwise)", emitter.calls)
	}
	if n := countInvoices(); n != 1 {
		t.Fatalf("tenant has %d invoices after two failed creates, want 1", n)
	}
	if got := nextSeq(); got != seqBefore {
		t.Fatalf("next invoice sequence after two failed creates: got %d, want %d (each failed create used up a number)", got, seqBefore)
	}

	next, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"})
	if err != nil {
		t.Fatalf("create after the failures: %v", err)
	}
	if want := fmt.Sprintf("HC-%06d", seqBefore); next.InvoiceNumber != want {
		t.Fatalf("invoice after the failures: got %q, want %q, the number the failed creates would have had", next.InvoiceNumber, want)
	}
}

// TestCreate_TwoAtOnceTakeConsecutiveNumbers: two manual creates for one
// tenant that overlap both commit, with consecutive, distinct numbers, and
// neither deadlocks. Each round runs createTwoAtOnce: A holds the tenant's
// number row inside its transaction while B waits for it. Round 1 runs before
// the tenant has a settings row, so A's number inserts the row and B waits on
// that insert. Round 2 runs on the row A left, where B waits on A's update of
// it: the steady state, and the round a number read outside the row lock
// fails (B would take A's number). A burst of four creates released together
// follows. The stored invoices then run HC-000001 to HC-000008 with no gap
// and no repeat.
func TestCreate_TwoAtOnceTakeConsecutiveNumbers(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Invoice Numbers Two At Once")
	cust := seedAuditCustomer(t, db, tenantID, "two_at_once")
	settings := tenant.NewSettingsStore(db)
	store := invoice.NewPostgresStore(db)
	newService := func() *invoice.Service { return invoice.NewService(store, nil, settings) }

	for i, want := range [][2]string{{"HC-000001", "HC-000002"}, {"HC-000003", "HC-000004"}} {
		a, b := createTwoAtOnce(t, ctx, db, newService, tenantID, cust.ID)
		if a.InvoiceNumber != want[0] || b.InvoiceNumber != want[1] {
			t.Fatalf("round %d numbers: A %q, B %q; want %s then %s", i+1, a.InvoiceNumber, b.InvoiceNumber, want[0], want[1])
		}
	}

	// Then a burst: four creates released together, so they reach the number
	// row at the same moment rather than one already holding it. They queue on
	// the row and all commit, with the next four numbers in some order. A path
	// that took a shared lock on the row before updating it would deadlock
	// here, two holders each waiting to upgrade. Four stays under the test
	// pool's five connections; each create holds one.
	const burst = 4
	svc := newService()
	start := make(chan struct{})
	errsOut := make(chan error, burst)
	for range burst {
		go func() {
			<-start
			_, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"})
			errsOut <- err
		}()
	}
	close(start)
	for range burst {
		select {
		case err := <-errsOut:
			if err != nil {
				t.Fatalf("burst create: %v (a deadlock loser fails here with SQLSTATE 40P01)", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a burst create did not return within 30s")
		}
	}

	stored, total, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	numbers := make([]string, 0, len(stored))
	for _, inv := range stored {
		numbers = append(numbers, inv.InvoiceNumber)
	}
	sort.Strings(numbers)
	want := make([]string, 0, 4+burst)
	for n := 1; n <= 4+burst; n++ {
		want = append(want, fmt.Sprintf("HC-%06d", n))
	}
	if total != len(want) || strings.Join(numbers, ",") != strings.Join(want, ",") {
		t.Fatalf("stored invoices: %d %v, want %d %v", total, numbers, len(want), want)
	}
	if seq, err := settings.NextInvoiceSequence(ctx, tenantID); err != nil || seq != len(want)+1 {
		t.Fatalf("next invoice sequence: %d (%v), want %d", seq, err, len(want)+1)
	}
}

// createTwoAtOnce runs two manual creates that overlap and returns both.
// Create A, a bare header, pauses inside its transaction after it has taken
// its number (its in-transaction audit emission blocks), so it holds the
// tenant's number row. Create B, with a line, then starts, and the test waits
// until pg_stat_activity shows B's number statement blocked by A's backend:
// the two really overlap. Released, A commits and B goes on. Either create
// failing fails the test, so a deadlock loser (SQLSTATE 40P01) fails it.
func createTwoAtOnce(t *testing.T, ctx context.Context, db *postgres.DB, newService func() *invoice.Service, tenantID, customerID string) (a, b domain.Invoice) {
	t.Helper()
	pause := &pausingEmitter{pid: make(chan int, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(pause.release) }) }
	// Registered after SetupTestDB, so it runs first: a failed assertion
	// never leaves A's transaction open over the database's cleanup.
	t.Cleanup(release)

	svcA := newService()
	svcA.SetAuditLogger(pause)
	svcB := newService()

	type result struct {
		inv domain.Invoice
		err error
	}
	aDone := make(chan result, 1)
	go func() {
		inv, err := svcA.Create(ctx, tenantID, invoice.CreateInput{CustomerID: customerID, Currency: "USD"})
		aDone <- result{inv, err}
	}()
	var aPID int
	select {
	case aPID = <-pause.pid:
	case r := <-aDone:
		t.Fatalf("create A returned before its transaction paused: %v", r.err)
	case <-time.After(30 * time.Second):
		t.Fatal("create A never reached its in-transaction audit emission")
	}

	bDone := make(chan result, 1)
	go func() {
		inv, err := svcB.Create(ctx, tenantID, invoice.CreateInput{CustomerID: customerID, Currency: "USD", LineItems: []invoice.AddLineItemInput{
			{Description: "Setup fee", Quantity: 1, UnitAmountCents: 5000},
		}})
		bDone <- result{inv, err}
	}()
	if q := waitForBackendBlockedBy(t, ctx, db.Pool, aPID); !strings.Contains(q, "tenant_settings") {
		t.Fatalf("create B waits on A at %q, want its number statement on tenant_settings", q)
	}
	select {
	case r := <-bDone:
		t.Fatalf("create B returned (%q, %v) while A still held the number row", r.inv.InvoiceNumber, r.err)
	default:
	}
	release()

	wait := func(name string, done <-chan result) domain.Invoice {
		t.Helper()
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("create %s: %v", name, r.err)
			}
			return r.inv
		case <-time.After(30 * time.Second):
			t.Fatalf("create %s did not return within 30s of the release", name)
			return domain.Invoice{}
		}
	}
	return wait("A", aDone), wait("B", bDone)
}

// pausingEmitter is an invoice.AuditLogger whose in-transaction emission
// reports its backend's pid and then waits for release. The emission runs
// after the number and the inserts, so the paused create holds the tenant's
// number row until release.
type pausingEmitter struct {
	pid     chan int
	release chan struct{}
}

func (p *pausingEmitter) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}

func (p *pausingEmitter) LogInTx(ctx context.Context, tx *sql.Tx, _ audit.Entry) error {
	var pid int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return err
	}
	p.pid <- pid
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForBackendBlockedBy returns the statement of a backend that waits for a
// lock the backend pid holds, once one does.
func waitForBackendBlockedBy(t *testing.T, ctx context.Context, admin *sql.DB, pid int) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var query string
		err := admin.QueryRowContext(ctx,
			`SELECT query FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, pid).Scan(&query)
		switch {
		case err == nil:
			return query
		case !errors.Is(err, sql.ErrNoRows):
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend waited for a lock that backend %d holds within 15s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
