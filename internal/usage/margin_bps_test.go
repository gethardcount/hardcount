package usage

import "testing"

// TestMarginBps pins the one margin formula the headline and every
// per-model row share: (revenue − cost) / revenue in basis points, cost in
// micro-dollars, rounded half away from zero, none without revenue.
func TestMarginBps(t *testing.T) {
	cases := []struct {
		name         string
		revenueCents int64
		costMicros   int64
		want         *int64
	}{
		{"no revenue", 0, 5_000_000, nil},
		{"no revenue, no cost", 0, 0, nil},
		{"20 percent", 1000, 8_000_000, wantBps(2000)},
		{"free to serve", 1000, 0, wantBps(10000)},
		{"cost above revenue", 1000, 15_000_000, wantBps(-5000)},
		{"rounds to nearest", 3000, 4000, wantBps(9999)},                   // 9998.67
		{"half rounds away from zero", 2, 1, wantBps(10000)},               // 9999.5
		{"negative half rounds away from zero", 2, 30_001, wantBps(-5001)}, // -5000.5
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := marginBps(tc.revenueCents, tc.costMicros)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("marginBps(%d, %d) = %d, want none", tc.revenueCents, tc.costMicros, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("marginBps(%d, %d) = none, want %d", tc.revenueCents, tc.costMicros, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("marginBps(%d, %d) = %d, want %d", tc.revenueCents, tc.costMicros, *got, *tc.want)
			}
		})
	}
}

func wantBps(v int64) *int64 { return &v }
