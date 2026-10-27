package money

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

// MulDivRem and MulDivRoundHalfToEven agree with exact big-integer math for
// any arguments whose result fits int64, and refuse the rest.
func TestMulDiv_MatchesExactMath(t *testing.T) {
	ref := func(a, b, d int64) (q, r *big.Int) {
		p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
		return new(big.Int).QuoRem(p, big.NewInt(d), new(big.Int))
	}
	maxI := big.NewInt(math.MaxInt64)
	check := func(a, b, d int64) {
		t.Helper()
		wq, wr := ref(a, b, d)
		q, r, ok := MulDivRem(a, b, d)
		if fits := wq.Cmp(maxI) <= 0; ok != fits {
			t.Fatalf("MulDivRem(%d, %d, %d) ok = %v, want %v", a, b, d, ok, fits)
		}
		if ok && (q != wq.Int64() || r != wr.Int64()) {
			t.Fatalf("MulDivRem(%d, %d, %d) = %d r %d, want %s r %s", a, b, d, q, r, wq, wr)
		}
		// Half to even on the exact quotient.
		want := new(big.Int).Set(wq)
		twice := new(big.Int).Mul(wr, big.NewInt(2))
		if c := twice.Cmp(big.NewInt(d)); c > 0 || (c == 0 && wq.Bit(0) == 1) {
			want.Add(want, big.NewInt(1))
		}
		got, ok := MulDivRoundHalfToEven(a, b, d)
		if fits := want.Cmp(maxI) <= 0; ok != fits {
			t.Fatalf("MulDivRoundHalfToEven(%d, %d, %d) ok = %v, want %v", a, b, d, ok, fits)
		}
		if ok && got != want.Int64() {
			t.Fatalf("MulDivRoundHalfToEven(%d, %d, %d) = %d, want %s", a, b, d, got, want)
		}
		// Where the product fits int64, the result is RoundHalfToEven's.
		if a == 0 || b <= math.MaxInt64/a {
			if rh := RoundHalfToEven(a*b, d); rh != got {
				t.Fatalf("MulDivRoundHalfToEven(%d, %d, %d) = %d, RoundHalfToEven = %d", a, b, d, got, rh)
			}
		}
	}
	const maxV = math.MaxInt64
	for _, c := range [][3]int64{
		{0, 0, 1}, {1, 1, 1}, {25, 1, 10}, {35, 1, 10}, {maxV, 1, 1}, {maxV, 1, 2},
		{maxV, 1_000_000, 1_000_000}, {maxV, 1_000_000, 2_000_000}, {maxV, 1_000_000, 999_999},
		{maxV, maxV, maxV}, {maxV, maxV, maxV - 1}, {maxV, 2, 1}, {maxV, 2, 2},
		{100_000_000_000_000, 200_000, 1_000_000}, {4_999_999_999_999_999_995, 200_000, 1_000_000},
		{3, 5, 2}, {1, 3, 2}, {maxV, 3, 2},
		// A denominator above MaxInt64/2 with a remainder above half of it.
		{2, 2_325_000_000_000_000_000, 4_700_000_000_000_000_000}, {maxV - 1, 1, maxV},
	} {
		check(c[0], c[1], c[2])
	}
	rng := rand.New(rand.NewSource(1))
	for range 20000 {
		a := rng.Int63() >> rng.Intn(63)
		b := rng.Int63() >> rng.Intn(63)
		d := rng.Int63()>>rng.Intn(63) + 1
		check(a, b, d)
	}
}

func TestMulDiv_RefusesOutOfRangeArguments(t *testing.T) {
	for _, c := range [][3]int64{{-1, 1, 1}, {1, -1, 1}, {1, 1, 0}, {1, 1, -1}} {
		if _, _, ok := MulDivRem(c[0], c[1], c[2]); ok {
			t.Errorf("MulDivRem%v ok, want refused", c)
		}
		if _, ok := MulDivRoundHalfToEven(c[0], c[1], c[2]); ok {
			t.Errorf("MulDivRoundHalfToEven%v ok, want refused", c)
		}
	}
}

// MulDivRoundHalfToEvenSigned agrees with exact big-integer math, rounding
// the magnitude half to even and putting the sign back, and equals
// RoundHalfToEven(a*b, d) wherever a*b fits int64.
func TestMulDivSigned_MatchesExactMath(t *testing.T) {
	maxI, minI := big.NewInt(math.MaxInt64), big.NewInt(-math.MaxInt64)
	check := func(a, b, d int64) {
		t.Helper()
		p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
		neg := p.Sign() < 0
		p.Abs(p)
		q, r := new(big.Int).QuoRem(p, big.NewInt(d), new(big.Int))
		twice := new(big.Int).Mul(r, big.NewInt(2))
		if c := twice.Cmp(big.NewInt(d)); c > 0 || (c == 0 && q.Bit(0) == 1) {
			q.Add(q, big.NewInt(1))
		}
		if neg {
			q.Neg(q)
		}
		got, ok := MulDivRoundHalfToEvenSigned(a, b, d)
		if fits := q.Cmp(maxI) <= 0 && q.Cmp(minI) >= 0; ok != fits {
			t.Fatalf("MulDivRoundHalfToEvenSigned(%d, %d, %d) ok = %v, want %v (exact %s)", a, b, d, ok, fits, q)
		}
		if ok && got != q.Int64() {
			t.Fatalf("MulDivRoundHalfToEvenSigned(%d, %d, %d) = %d, want %s", a, b, d, got, q)
		}
		prod := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
		// The helper refuses a result of MinInt64 (its range is symmetric).
		if prod.IsInt64() && prod.Int64() != math.MinInt64 {
			if rh := RoundHalfToEven(a*b, d); rh != got {
				t.Fatalf("MulDivRoundHalfToEvenSigned(%d, %d, %d) = %d, RoundHalfToEven = %d", a, b, d, got, rh)
			}
		}
	}
	const maxV, minV = math.MaxInt64, math.MinInt64
	for _, c := range [][3]int64{
		{0, 0, 1}, {-1, 1, 1}, {-25, 1, 10}, {-35, 1, 10}, {25, -1, 10}, {-15, -1, 10},
		{maxV, -1, 1}, {minV, 1, 1}, {minV, 1, 2}, {minV, -1, 1}, {minV, -1, 2}, {minV, minV, maxV},
		{maxV, maxV, maxV}, {-maxV, maxV, maxV}, {maxV, -2, 2}, {maxV, -2, 1},
		{-3, 5, 2}, {-1, 3, 2}, {maxV, -3, 2}, {-maxV, 3, 6}, {-1_000_000_000_000, 4_000_000_000_000, 3},
		// 65535 × 281479271743489 = 2^64 − 1: halved, MaxInt64 with a tie
		// that rounds up past it.
		{65535, 281479271743489, 2}, {-65535, 281479271743489, 2},
		{-2, 2_325_000_000_000_000_000, 4_700_000_000_000_000_000}, {minV, 1, 4}, {minV, 1, maxV},
	} {
		check(c[0], c[1], c[2])
	}
	rng := rand.New(rand.NewSource(2))
	for range 20000 {
		a := rng.Int63() >> rng.Intn(63)
		b := rng.Int63() >> rng.Intn(63)
		if rng.Intn(2) == 0 {
			a = -a
		}
		if rng.Intn(2) == 0 {
			b = -b
		}
		d := rng.Int63()>>rng.Intn(63) + 1
		check(a, b, d)
	}
	for _, d := range []int64{0, -1} {
		if _, ok := MulDivRoundHalfToEvenSigned(1, 1, d); ok {
			t.Errorf("MulDivRoundHalfToEvenSigned(1, 1, %d) ok, want refused", d)
		}
	}
}

func TestAdd_RefusesWrap(t *testing.T) {
	const maxV, minV = math.MaxInt64, math.MinInt64
	for _, c := range []struct {
		a, b, want int64
		ok         bool
	}{
		{1, 2, 3, true}, {-1, -2, -3, true}, {maxV, 0, maxV, true}, {maxV, minV, -1, true},
		{maxV - 1, 1, maxV, true}, {minV + 1, -1, minV, true}, {maxV, -maxV, 0, true},
		{maxV, 1, 0, false}, {1, maxV, 0, false}, {minV, -1, 0, false}, {maxV, maxV, 0, false}, {minV, minV, 0, false},
	} {
		got, ok := Add(c.a, c.b)
		if ok != c.ok || got != c.want {
			t.Errorf("Add(%d, %d) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestMul_MatchesExactMath(t *testing.T) {
	const maxV, minV = math.MaxInt64, math.MinInt64
	check := func(a, b int64) {
		t.Helper()
		p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
		got, ok := Mul(a, b)
		if ok != p.IsInt64() || (ok && got != p.Int64()) {
			t.Fatalf("Mul(%d, %d) = %d, %v; exact %s", a, b, got, ok, p)
		}
	}
	for _, c := range [][2]int64{
		{0, 0}, {0, minV}, {minV, 0}, {1, minV}, {minV, 1}, {-1, minV}, {minV, -1}, {-1, maxV}, {maxV, -1},
		{2, maxV / 2}, {2, maxV/2 + 1}, {-2, maxV/2 + 1}, {3_037_000_499, 3_037_000_499}, {3_037_000_500, 3_037_000_500},
		{-3_037_000_500, 3_037_000_500}, {minV / 2, 2}, {minV / 2, -2},
	} {
		check(c[0], c[1])
	}
	rng := rand.New(rand.NewSource(3))
	for range 20000 {
		a := rng.Int63() >> rng.Intn(63)
		b := rng.Int63() >> rng.Intn(63)
		if rng.Intn(2) == 0 {
			a = -a
		}
		if rng.Intn(2) == 0 {
			b = -b
		}
		check(a, b)
	}
}
