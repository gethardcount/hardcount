package analytics

import (
	"errors"
	"math"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestNetMRR_OnlyTheNetMustFit: a step past int64 is fine when the net fits;
// a net past int64 is refused.
func TestNetMRR_OnlyTheNetMustFit(t *testing.T) {
	got, err := netMRR(MRRMovementTotals{New: 5e18, Expansion: 5e18, Contraction: 5e18})
	if err != nil || got != 5e18 {
		t.Fatalf("intermediate past int64, net fits: got %d, %v; want 5e18, nil", got, err)
	}
	got, err = netMRR(MRRMovementTotals{New: 100, Expansion: 20, Contraction: 30, Churned: 40})
	if err != nil || got != 50 {
		t.Fatalf("plain net: got %d, %v; want 50, nil", got, err)
	}
	if _, err := netMRR(MRRMovementTotals{New: math.MaxInt64, Expansion: 1}); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Fatalf("net past int64: got %v, want ErrAmountOverflow", err)
	}
}
