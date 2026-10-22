package money

import "testing"

func TestAllocateLIFO(t *testing.T) {
	t.Run("fills newest-first, spills to older", func(t *testing.T) {
		// caps ordered newest-first: [upgrade=5000, base=10000]. Credit 7000 →
		// 5000 to the upgrade invoice (fully reversed), 2000 spills to base.
		out, rem := AllocateLIFO(7000, []int64{5000, 10000})
		if rem != 0 {
			t.Fatalf("remainder=%d, want 0", rem)
		}
		if out[0] != 5000 || out[1] != 2000 {
			t.Fatalf("got %v, want [5000 2000]", out)
		}
	})

	t.Run("fits entirely in the newest bucket → older untouched", func(t *testing.T) {
		out, rem := AllocateLIFO(3000, []int64{5000, 10000})
		if rem != 0 || out[0] != 3000 || out[1] != 0 {
			t.Fatalf("got %v rem=%d, want [3000 0] rem=0 (base untouched)", out, rem)
		}
	})

	t.Run("exceeds all caps → remainder reported", func(t *testing.T) {
		out, rem := AllocateLIFO(20000, []int64{5000, 10000})
		if rem != 5000 {
			t.Fatalf("remainder=%d, want 5000", rem)
		}
		if out[0] != 5000 || out[1] != 10000 {
			t.Fatalf("got %v, want [5000 10000] (both at cap)", out)
		}
	})
}
