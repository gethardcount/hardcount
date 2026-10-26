package usage

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The Usage page's per-meter number follows the aggregation billing uses,
// the meter's own (ADR-130 §2). A meter's prices are on its plans, so the
// page reads the meter alone.
func TestBillingModes(t *testing.T) {
	meters := []domain.Meter{
		{ID: "peak", Aggregation: "max"},
		{ID: "total", Aggregation: "sum"},
		{ID: "latest", Aggregation: "last"},
		{ID: "events", Aggregation: "count"},
		{ID: "unknown", Aggregation: ""},
	}
	want := map[string]string{"peak": "max", "total": "sum", "latest": "last", "events": "count", "unknown": "sum"}
	got := BillingModes(meters)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: mode = %q, want %q", id, got[id], w)
		}
	}
}
