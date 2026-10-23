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
		// Where the product fits int64 (and 2d does, which RoundHalfToEven
		// needs), the result is RoundHalfToEven's.
		if (a == 0 || b <= math.MaxInt64/a) && d <= math.MaxInt64/2 {
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
