// Package market contains bonding-curve pricing and the trade service.
package market

import (
	"errors"
	"math"
	"math/bits"
)

// Economy constants.
const (
	FeeBps      int64 = 200    // 2% trade fee, burned
	MinFee      int64 = 1      // every trade burns at least 1 coin
	MaxTradeQty int64 = 10_000 // upper bound per single trade
)

var (
	ErrOverflow        = errors.New("market: arithmetic overflow")
	ErrInvalidQty      = errors.New("market: quantity must be between 1 and 10000")
	ErrTradeTooSmall   = errors.New("market: trade too small to cover the fee")
	ErrNotEnoughSupply = errors.New("market: not enough circulating supply")
)

// Curve is a linear bonding curve: price(s) = Base + Slope*s, where s is circulating supply.
// Slope must be even so that the area under the curve is always an exact integer.
type Curve struct {
	Base  int64
	Slope int64
}

// PriceAt is the marginal price at a given circulating supply.
func (c Curve) PriceAt(supply int64) (int64, error) {
	m, err := mul(c.Slope, supply)
	if err != nil {
		return 0, err
	}
	return add(c.Base, m)
}

// Area is the exact area under the curve between supply s0 and s1 (s0 <= s1):
//
//	base*(s1-s0) + slope*(s1² - s0²)/2  =  base*n + (slope/2)*n*(s0+s1)
//
// Because it is an integral, Area(a,b) + Area(b,c) == Area(a,c) exactly: splitting one big
// order into many small ones never changes the curve cost.
func (c Curve) Area(s0, s1 int64) (int64, error) {
	if s0 < 0 || s1 < s0 || c.Base < 0 || c.Slope < 0 || c.Slope%2 != 0 {
		return 0, ErrOverflow
	}
	n := s1 - s0
	a, err := mul(c.Base, n)
	if err != nil {
		return 0, err
	}
	sum, err := add(s0, s1)
	if err != nil {
		return 0, err
	}
	b, err := mul(n, sum)
	if err != nil {
		return 0, err
	}
	b, err = mul(c.Slope/2, b)
	if err != nil {
		return 0, err
	}
	return add(a, b)
}

// Fee is 2% of the trade amount, rounded UP, and at least MinFee.
// Rounding up means splitting an order into tiny pieces can only cost more fees, never less.
func Fee(amount int64) (int64, error) {
	x, err := mul(amount, FeeBps)
	if err != nil {
		return 0, err
	}
	fee := (x + 9_999) / 10_000
	if fee < MinFee {
		fee = MinFee
	}
	return fee, nil
}

// Side is buy or sell.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Valid reports whether s is a known side.
func (s Side) Valid() bool { return s == Buy || s == Sell }

// Quote is the full price breakdown of a trade.
type Quote struct {
	Side        Side  `json:"side"`
	Qty         int64 `json:"qty"`
	Amount      int64 `json:"amount"`  // area under the curve
	Fee         int64 `json:"fee"`     // burned
	Royalty     int64 `json:"royalty"` // paid to the card creator (Phase 3)
	Total       int64 `json:"total"`   // buy: coins the player pays; sell: coins the player receives
	PriceBefore int64 `json:"price_before"`
	PriceAfter  int64 `json:"price_after"`
}

// QuoteTrade prices buying or selling qty units when `supply` units are circulating.
// It does not check balances, holdings or max supply; the trade service does that.
func QuoteTrade(c Curve, supply int64, side Side, qty int64) (Quote, error) {
	if qty < 1 || qty > MaxTradeQty {
		return Quote{}, ErrInvalidQty
	}
	q := Quote{Side: side, Qty: qty}
	var s0, s1, after int64
	switch side {
	case Buy:
		s0, s1, after = supply, supply+qty, supply+qty
	case Sell:
		if qty > supply {
			return Quote{}, ErrNotEnoughSupply
		}
		s0, s1, after = supply-qty, supply, supply-qty
	default:
		return Quote{}, errors.New("market: side must be buy or sell")
	}

	var err error
	if q.Amount, err = c.Area(s0, s1); err != nil {
		return Quote{}, err
	}
	if q.Fee, err = Fee(q.Amount); err != nil {
		return Quote{}, err
	}
	if q.PriceBefore, err = c.PriceAt(supply); err != nil {
		return Quote{}, err
	}
	if q.PriceAfter, err = c.PriceAt(after); err != nil {
		return Quote{}, err
	}

	if side == Buy {
		if q.Total, err = add(q.Amount, q.Fee); err != nil {
			return Quote{}, err
		}
	} else {
		q.Total = q.Amount - q.Fee
		if q.Total <= 0 {
			return Quote{}, ErrTradeTooSmall
		}
	}
	return q, nil
}

// mul multiplies two non-negative int64s, reporting overflow.
func mul(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, ErrOverflow
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, ErrOverflow
	}
	return int64(lo), nil
}

// add adds two non-negative int64s, reporting overflow.
func add(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}
