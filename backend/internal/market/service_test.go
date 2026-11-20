package market_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/auth"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/market"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/testutil"
)

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	users  *auth.Service
	market *market.Service
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.NewDB(t)
	l, err := ledger.Load(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, pool: pool, users: auth.NewService(pool, l), market: market.NewService(pool, l)}
}

func (e *env) player(name string) int64 {
	e.t.Helper()
	u, _, err := e.users.DevLogin(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func (e *env) card(slug, supplyType string, maxSupply *int64, base, slope int64) int64 {
	e.t.Helper()
	var id int64
	err := e.pool.QueryRow(context.Background(), `
		INSERT INTO cards (slug, name, supply_type, max_supply, base, slope)
		VALUES ($1, $1, $2, $3, $4, $5) RETURNING id`, slug, supplyType, maxSupply, base, slope).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) balance(userID int64) int64 {
	e.t.Helper()
	var b int64
	if err := e.pool.QueryRow(context.Background(), `SELECT balance FROM wallets WHERE user_id = $1`, userID).Scan(&b); err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) count(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) mustReconcile() ledger.Report {
	e.t.Helper()
	r, err := e.market.Reconcile(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	if !r.OK {
		e.t.Fatalf("reconcile failed: %v", r.Problems)
	}
	return r
}

func (e *env) trade(userID, cardID int64, side market.Side, qty int64) (market.Trade, error) {
	return e.market.ExecuteTrade(context.Background(), market.TradeRequest{
		UserID: userID, CardID: cardID, Side: side, Qty: qty, IdempotencyKey: uuid.NewString(),
	})
}

func ptr(v int64) *int64 { return &v }

func TestBuyThenSell(t *testing.T) {
	e := setup(t)
	alice := e.player("alice")
	card := e.card("test-card", "unlimited", nil, 100, 2)

	buy, err := e.trade(alice, card, market.Buy, 10)
	if err != nil {
		t.Fatal(err)
	}
	// area = 100*10 + 1*10*(0+10) = 1100; fee = ceil(22) = 22
	if buy.Amount != 1100 || buy.Fee != 22 || buy.Total != 1122 || buy.PriceAfter != 120 {
		t.Fatalf("unexpected buy: %+v", buy)
	}
	if got := e.balance(alice); got != auth.SignupGrant-1122 {
		t.Fatalf("balance after buy = %d", got)
	}
	h, _ := e.market.Holdings(context.Background(), alice)
	if len(h) != 1 || h[0].Qty != 10 || h[0].CostBasis != 1122 {
		t.Fatalf("holdings after buy: %+v", h)
	}
	e.mustReconcile()

	sell, err := e.trade(alice, card, market.Sell, 10)
	if err != nil {
		t.Fatal(err)
	}
	if sell.Amount != 1100 || sell.Fee != 22 || sell.Total != 1078 || sell.PriceAfter != 100 {
		t.Fatalf("unexpected sell: %+v", sell)
	}
	// Round trip cost exactly the two fees, which were burned.
	if got := e.balance(alice); got != auth.SignupGrant-44 {
		t.Fatalf("balance after round trip = %d, want %d", got, auth.SignupGrant-44)
	}
	r := e.mustReconcile()
	if r.Burned != 44 || r.Reserve != 0 {
		t.Fatalf("supply after round trip: %+v", r)
	}
}

func TestRejectedTradesChangeNothing(t *testing.T) {
	e := setup(t)
	bob := e.player("bob")
	fixed := e.card("tiny", "fixed", ptr(5), 10, 2)
	pricey := e.card("pricey", "unlimited", nil, 20_000, 0)

	cases := []struct {
		name string
		card int64
		side market.Side
		qty  int64
		want error
	}{
		{"too expensive", pricey, market.Buy, 1, market.ErrInsufficientFunds},
		{"over max supply", fixed, market.Buy, 6, market.ErrSupplyExceeded},
		{"sell without holding", fixed, market.Sell, 1, market.ErrNotEnoughSupply},
		{"unknown card", 999_999, market.Buy, 1, market.ErrCardNotFound},
		{"zero qty", fixed, market.Buy, 0, market.ErrInvalidQty},
		{"bad side", fixed, "hold", 1, market.ErrInvalidSide},
	}
	for _, tc := range cases {
		if _, err := e.trade(bob, tc.card, tc.side, tc.qty); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	// Someone else holds the card, but bob doesn't: selling must fail on holdings.
	carol := e.player("carol")
	if _, err := e.trade(carol, fixed, market.Buy, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.trade(bob, fixed, market.Sell, 1); !errors.Is(err, market.ErrInsufficientHoldings) {
		t.Errorf("sell other's card: err = %v", err)
	}

	if got := e.balance(bob); got != auth.SignupGrant {
		t.Fatalf("bob's balance changed to %d", got)
	}
	if n := e.count(`SELECT count(*) FROM trades WHERE user_id = $1`, bob); n != 0 {
		t.Fatalf("bob has %d trades", n)
	}
	e.mustReconcile()
}

// 100 players try to buy the last unit of a card at the same moment. Exactly one may win.
func TestRaceForTheLastUnit(t *testing.T) {
	e := setup(t)
	card := e.card("last-one", "fixed", ptr(1), 50, 0)
	const players = 100
	ids := make([]int64, players)
	for i := range ids {
		ids[i] = e.player(fmt.Sprintf("racer%d", i))
	}

	var wins, soldOut atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.trade(id, card, market.Buy, 1)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, market.ErrSupplyExceeded):
				soldOut.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 || soldOut.Load() != players-1 {
		t.Fatalf("wins = %d, sold out = %d; want 1 and %d", wins.Load(), soldOut.Load(), players-1)
	}
	if c := e.count(`SELECT circulating FROM cards WHERE id = $1`, card); c != 1 {
		t.Fatalf("circulating = %d, want 1", c)
	}
	e.mustReconcile()
}

// One player with money for exactly one purchase fires 50 buys at once across 5 cards.
// The wallet lock must make exactly one succeed.
func TestConcurrentDoubleSpend(t *testing.T) {
	e := setup(t)
	mallory := e.player("mallory")
	cards := make([]int64, 5)
	for i := range cards {
		cards[i] = e.card(fmt.Sprintf("big-%d", i), "unlimited", nil, 6_000, 0) // 6000 + 120 fee
	}

	var ok, broke atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.trade(mallory, cards[i%len(cards)], market.Buy, 1)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, market.ErrInsufficientFunds):
				broke.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != 1 {
		t.Fatalf("%d purchases succeeded, want exactly 1", ok.Load())
	}
	if got := e.balance(mallory); got != auth.SignupGrant-6_120 {
		t.Fatalf("balance = %d, want %d", got, auth.SignupGrant-6_120)
	}
	e.mustReconcile()
}

// The same request sent 20 times at once (double-click / retries) executes exactly once.
func TestIdempotentRetries(t *testing.T) {
	e := setup(t)
	dan := e.player("dan")
	card := e.card("idem", "unlimited", nil, 100, 2)
	key := uuid.NewString()
	req := market.TradeRequest{UserID: dan, CardID: card, Side: market.Buy, Qty: 3, IdempotencyKey: key}

	results := make([]market.Trade, 20)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tr, err := e.market.ExecuteTrade(context.Background(), req)
			if err != nil {
				t.Errorf("attempt %d: %v", i, err)
			}
			results[i] = tr
		}()
	}
	close(start)
	wg.Wait()

	fresh := 0
	for _, r := range results {
		if r.ID != results[0].ID {
			t.Fatalf("different trade ids returned: %d vs %d", r.ID, results[0].ID)
		}
		if !r.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d responses were fresh executions, want 1", fresh)
	}
	if n := e.count(`SELECT count(*) FROM trades WHERE user_id = $1`, dan); n != 1 {
		t.Fatalf("%d trade rows, want 1", n)
	}
	if got := e.balance(dan); got != auth.SignupGrant-results[0].Total {
		t.Fatalf("charged more than once: balance %d", got)
	}

	// Reusing the key for a different trade is an error, not a silent replay.
	req.Qty = 4
	if _, err := e.market.ExecuteTrade(context.Background(), req); !errors.Is(err, market.ErrIdempotencyKeyReused) {
		t.Fatalf("reused key: err = %v", err)
	}
	e.mustReconcile()
}

// Many players buy and sell many cards concurrently. Nothing may deadlock, error unexpectedly
// or leak a single coin. Duration: STRESS_SECONDS (default 5).
func TestStressNoDeadlocksNoLeaks(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}
	e := setup(t)
	duration := 5 * time.Second
	if s, err := strconv.Atoi(os.Getenv("STRESS_SECONDS")); err == nil && s > 0 {
		duration = time.Duration(s) * time.Second
	}

	cards, err := e.market.ListCards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	players := make([]int64, 20)
	for i := range players {
		players[i] = e.player(fmt.Sprintf("trader%d", i))
	}

	expected := []error{
		market.ErrInsufficientFunds, market.ErrInsufficientHoldings, market.ErrSupplyExceeded,
		market.ErrNotEnoughSupply, market.ErrTradeTooSmall,
	}
	var executed, rejected atomic.Int64
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 99))
			for time.Now().Before(deadline) {
				user := players[r.IntN(len(players))]
				card := cards[r.IntN(len(cards))].ID
				side := market.Buy
				if r.IntN(2) == 0 {
					side = market.Sell
				}
				_, err := e.trade(user, card, side, 1+r.Int64N(5))
				if err == nil {
					executed.Add(1)
					continue
				}
				known := false
				for _, want := range expected {
					if errors.Is(err, want) {
						known = true
						break
					}
				}
				if !known {
					t.Errorf("unexpected error (deadlock?): %v", err)
					return
				}
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	r := e.mustReconcile()
	t.Logf("%d trades executed, %d rejected by rules in %s (%.0f trades/sec); minted=%d burned=%d reserve=%d wallets=%d",
		executed.Load(), rejected.Load(), duration, float64(executed.Load())/duration.Seconds(),
		r.Minted, r.Burned, r.Reserve, r.InWallets)
	if executed.Load() == 0 {
		t.Fatal("no trades executed")
	}
}
