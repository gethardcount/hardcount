package usage

import (
	"net/http"
	"strings"
	"testing"
)

// A quantity is at most 100 characters, the limit every request decimal
// shares (domain.ParseDecimal). 100 is far past any value inside the size and
// scale limits written plainly; only padding with zeros reaches it.
//
// Mutation-verify: drop the length check in domain.ParseDecimal → the
// 101-character cases go red.
func TestUsageIngest_QuantityLengthLimit(t *testing.T) {
	at100 := "1." + strings.Repeat("0", 98)
	over := at100 + "0"
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{at100, http.StatusCreated},
		{`"` + at100 + `"`, http.StatusCreated},
		{over, http.StatusUnprocessableEntity},
		{`"` + over + `"`, http.StatusUnprocessableEntity},
		{`"` + strings.Repeat("0", 101) + `"`, http.StatusUnprocessableEntity},
	} {
		for _, door := range []string{"single", "batch", "backfill"} {
			t.Run(door+"/"+tc.raw[:8], func(t *testing.T) {
				store := newMemStore()
				h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
				code, body := quantityDoors(t, h)[door](quantityBody(tc.raw, "evt_len"))
				want := tc.want
				if code != want {
					t.Fatalf("status %d, want %d; body %s", code, want, body)
				}
				if want == http.StatusUnprocessableEntity && !strings.Contains(body, "100 characters") {
					t.Errorf("422 does not state the limit: %s", body)
				}
			})
		}
	}
}
