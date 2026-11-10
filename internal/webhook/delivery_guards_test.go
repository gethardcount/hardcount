package webhook

import (
	"context"
	"errors"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/doctor"
)

// TestRecoverDialHook_InAControlHook: a Control hook panics on a real dialer,
// with recoverDialHook deferred as dialControl defers it. net.Dialer runs
// Control on a goroutine of its own when the host has both IPv4 and IPv6
// addresses, as "localhost" has on most machines. The process survives and
// the dial is refused, although a listener is there to accept it. The error
// is errDialHookPanicked without the panic value, because a delivery's error
// is shown to the tenant.
//
// The hook is built here, because dialControl's own body has no seam through
// which a test can make it panic.
func TestRecoverDialHook_InAControlHook(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) (err error) {
		defer recoverDialHook(context.Background(), address, &err)
		panic("control hook bug")
	}}
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err == nil {
		_ = conn.Close()
		t.Fatal("the dial connected, want refused: a hook that panicked must let no address through")
	}
	if !errors.Is(err, errDialHookPanicked) {
		t.Errorf("dial error = %v, want errDialHookPanicked", err)
	}
	if strings.Contains(err.Error(), "control hook bug") {
		t.Errorf("dial error = %q carries the panic value, which a delivery's error would show to the tenant", err)
	}
}

// TestDoctorCheck_StatesTheDeliveryGiveUpAge: the doctor check for a delivery
// still pending past the give-up age states two intervals in its SQL, which
// cannot call this package.
//
// The first is the give-up age, and must equal deliveryGiveUpAfter. If the
// check's were shorter, it would flag rows the worker has not given up yet.
// If it were longer, a change to the retry ladder would move the worker's
// bound and leave the check behind.
//
// The second is the check's grace. The worker gives a delivery up at its next
// claim, and a delivery that counted a failure is next claimed at the retry
// the ladder set. The grace must cover the longest such wait, or the check
// flags a delivery the worker is about to give up.
func TestDoctorCheck_StatesTheDeliveryGiveUpAge(t *testing.T) {
	const name = "webhook_delivery_pending_past_give_up"
	var sql string
	for _, c := range doctor.Checks {
		if c.Name == name {
			sql = c.SQL
		}
	}
	if sql == "" {
		t.Fatalf("hardcount-doctor has no check named %s", name)
	}
	m := regexp.MustCompile(`created_at < now\(\) - interval '(\d+) hours (\d+) minutes' - interval '(\d+) hours?'`).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("check %s: no \"created_at < now() - interval 'H hours M minutes' - interval 'H hours'\" in its SQL; update this test with the check", name)
	}
	hours, _ := strconv.Atoi(m[1])
	minutes, _ := strconv.Atoi(m[2])
	if got := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute; got != deliveryGiveUpAfter() {
		t.Errorf("check %s gives up after %v, deliveryGiveUpAfter() = %v", name, got, deliveryGiveUpAfter())
	}

	graceHours, _ := strconv.Atoi(m[3])
	grace := time.Duration(graceHours) * time.Hour
	// A failure counted just inside the give-up age sets the longest wait:
	// the ladder's longest slot plus up to 30 seconds of jitter
	// (scheduleRetryOrFail). The worker then needs one 30-second tick to
	// claim the row, and one claim lease more when that claim's mark is lost.
	const jitter, tick = 30 * time.Second, 30 * time.Second
	if need := slices.Max(retryBackoffs[:]) + jitter + tick + retryClaimLease(10); grace < need {
		t.Errorf("check %s allows %v past the give-up age, want at least %v: a delivery waiting for a retry the ladder set would be flagged",
			name, grace, need)
	}
}
