package market

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

func randomCurve(r *rand.Rand) Curve {
	return Curve{Base: 1 + r.Int64N(1_000_000), Slope: 2 * r.Int64N(5_001)} // slope even, 0..10000
}

func TestAreaIsAdditive(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 10_000; i++ {
		c := randomCurve(r)
		s0 := r.Int64N(1_000_000)
		mid := s0 + r.Int64N(MaxTradeQty)
		s1 := mid + r.Int64N(MaxTradeQty)

		whole, err := c.Area(s0, s1)
		if err != nil {
			t.Fatal(err)
		}
		left, _ := c.Area(s0, mid)
		right, _ := c.Area(mid, s1)
		if left+right != whole {
			t.Fatalf("curve %+v: Area(%d,%d)=%d but split gives %d", c, s0, s1, whole, left+right)
		}
	}
}

func TestBuyingAllAtOnceEqualsOneByOne(t *testing.T) {
	c := Curve{Base: 37, Slope: 6}
	start := int64(123)
	n := int64(250)

	bulk, _ := c.Area(start, start+n)
	var oneByOne int64
	for s := start; s < start+n; s++ {
		a, _ := c.Area(s, s+1)
		oneByOne += a
	}
	if bulk != oneByOne {
		t.Fatalf("bulk %d != one-by-one %d", bulk, oneByOne)
	}
}

func TestSplittingNeverReducesTotalCost(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 2_000; i++ {
		c := Curve{Base: 1 + r.Int64N(200), Slope: 2 * r.Int64N(5)} // cheap cards: where rounding matters most
		supply := r.Int64N(1000)
		n := 2 + r.Int64N(50)

		whole, err := QuoteTrade(c, supply, Buy, n)
		if err != nil {
			t.Fatal(err)
		}
		var split int64
		for k := int64(0); k < n; k++ {
			q, err := QuoteTrade(c, supply+k, Buy, 1)
			if err != nil {
				t.Fatal(err)
			}
			split += q.Total
		}
		if split < whole.Total {
			t.Fatalf("curve %+v: buying %d in pieces cost %d < %d in one order", c, n, split, whole.Total)
		}
	}
}

func TestRoundTripAlwaysLosesMoney(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 10_000; i++ {
		c := randomCurve(r)
		supply := r.Int64N(100_000)
		n := 1 + r.Int64N(MaxTradeQty)

		buy, err := QuoteTrade(c, supply, Buy, n)
		if err != nil {
			t.Fatal(err)
		}
		sell, err := QuoteTrade(c, supply+n, Sell, n)
		if errors.Is(err, ErrTradeTooSmall) {
			continue // can't even sell back: definitely lost money
		}
		if err != nil {
			t.Fatal(err)
		}
		if sell.Total >= buy.Total {
			t.Fatalf("curve %+v: buy %d then sell gives %d >= %d paid", c, n, sell.Total, buy.Total)
		}
		if buy.PriceAfter != sell.PriceBefore || sell.PriceAfter != buy.PriceBefore {
			t.Fatalf("prices don't round-trip: %+v / %+v", buy, sell)
		}
	}
}

func TestFeeRoundsUpWithMinimum(t *testing.T) {
	cases := []struct{ amount, fee int64 }{
		{1, 1},     // min fee
		{49, 1},    // 0.98 -> 1
		{50, 1},    // exactly 1
		{51, 2},    // 1.02 -> 2 (rounding down would give 1)
		{100, 2},   // exactly 2
		{1000, 20}, // exactly 20
		{1001, 21}, // 20.02 -> 21
	}
	for _, tc := range cases {
		got, err := Fee(tc.amount)
		if err != nil || got != tc.fee {
			t.Errorf("Fee(%d) = %d, %v; want %d", tc.amount, got, err, tc.fee)
		}
	}
}

func TestQuoteRejectsBadInput(t *testing.T) {
	c := Curve{Base: 10, Slope: 2}
	if _, err := QuoteTrade(c, 0, Buy, 0); !errors.Is(err, ErrInvalidQty) {
		t.Errorf("qty 0: %v", err)
	}
	if _, err := QuoteTrade(c, 0, Buy, MaxTradeQty+1); !errors.Is(err, ErrInvalidQty) {
		t.Errorf("qty too big: %v", err)
	}
	if _, err := QuoteTrade(c, 5, Sell, 6); !errors.Is(err, ErrNotEnoughSupply) {
		t.Errorf("sell more than supply: %v", err)
	}
	if _, err := QuoteTrade(Curve{Base: 1, Slope: 0}, 1, Sell, 1); !errors.Is(err, ErrTradeTooSmall) {
		t.Errorf("sell worth 1 coin with 1 coin fee: %v", err)
	}
}

func TestNoOverflowAtSchemaLimits(t *testing.T) {
	// The schema caps base at 1e6, slope at 1e4 and supply at 1e6; the worst trade must fit in int64.
	c := Curve{Base: 1_000_000, Slope: 10_000}
	q, err := QuoteTrade(c, 1_000_000-MaxTradeQty, Buy, MaxTradeQty)
	if err != nil {
		t.Fatalf("max trade overflowed: %v", err)
	}
	if q.Total <= 0 {
		t.Fatalf("bad total %d", q.Total)
	}
	// Beyond the limits, overflow is reported instead of wrapping around.
	if _, err := (Curve{Base: math.MaxInt64, Slope: 2}).Area(0, 2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected ErrOverflow, got %v", err)
	}
}
