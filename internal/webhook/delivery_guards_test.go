package webhook

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/doctor"
)

// TestDoctorCheck_StatesTheDeliveryGiveUpAge: the doctor check for a delivery
// still pending past the give-up age states that age in its SQL, which
// cannot call deliveryGiveUpAfter. The two must be one age. If the check's
// were shorter, it would flag rows the worker has not given up yet. If it
// were longer, a change to the retry ladder would move the worker's bound
// and leave the check behind.
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
	m := regexp.MustCompile(`created_at < now\(\) - interval '(\d+) hours (\d+) minutes'`).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("check %s: no \"created_at < now() - interval 'H hours M minutes'\" in its SQL; update this test with the check", name)
	}
	hours, _ := strconv.Atoi(m[1])
	minutes, _ := strconv.Atoi(m[2])
	if got := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute; got != deliveryGiveUpAfter() {
		t.Errorf("check %s gives up after %v, deliveryGiveUpAfter() = %v", name, got, deliveryGiveUpAfter())
	}
}
