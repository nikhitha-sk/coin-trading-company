package market

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
)

var (
	ErrCardNotFound         = errors.New("market: card not found")
	ErrInvalidSide          = errors.New("market: side must be buy or sell")
	ErrInvalidKey           = errors.New("market: idempotency_key must be 8-100 characters")
	ErrInsufficientFunds    = errors.New("market: insufficient funds")
	ErrInsufficientHoldings = errors.New("market: you don't hold enough of this card")
	ErrSupplyExceeded       = errors.New("market: not enough supply left")
	ErrIdempotencyKeyReused = errors.New("market: idempotency_key was already used for a different trade")
	ErrNoWallet             = errors.New("market: user has no wallet")
)

// Card is a tradable card with its live market state.
type Card struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	CreatorID   *int64    `json:"creator_id"`
	SupplyType  string    `json:"supply_type"`
	MaxSupply   *int64    `json:"max_supply"`
	Circulating int64     `json:"circulating"`
	Base        int64     `json:"base"`
	Slope       int64     `json:"slope"`
	RoyaltyBps  int       `json:"royalty_bps"`
	Price       int64     `json:"price"`
	Holders     int64     `json:"holders"`
	CreatedAt   time.Time `json:"created_at"`
}

func (c Card) curve() Curve { return Curve{Base: c.Base, Slope: c.Slope} }

// TradeRequest is what a player asks for.
type TradeRequest struct {
	UserID         int64
	CardID         int64
	Side           Side
	Qty            int64
	IdempotencyKey string
}

// Trade is an executed trade.
type Trade struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
	CardID         int64     `json:"card_id"`
	Side           Side      `json:"side"`
	Qty            int64     `json:"qty"`
	Amount         int64     `json:"amount"`
	Fee            int64     `json:"fee"`
	Royalty        int64     `json:"royalty"`
	Total          int64     `json:"total"`
	PriceAfter     int64     `json:"price_after"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
	// Replayed is true when this request repeated an earlier idempotency key and nothing new happened.
	Replayed bool `json:"replayed"`
}

// Holding is a player's position in one card.
type Holding struct {
	CardID      int64  `json:"card_id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Qty         int64  `json:"qty"`
	CostBasis   int64  `json:"cost_basis"`
	AvgCost     int64  `json:"avg_cost"`
	Price       int64  `json:"price"`
	MarketValue int64  `json:"market_value"`
	ProfitLoss  int64  `json:"profit_loss"`
}

// Service executes trades and answers market queries.
type Service struct {
	db     *pgxpool.Pool
	ledger *ledger.Ledger
}

// NewService creates the market service.
func NewService(db *pgxpool.Pool, l *ledger.Ledger) *Service {
	return &Service{db: db, ledger: l}
}

const cardColumns = `c.id, c.slug, c.name, c.creator_id, c.supply_type, c.max_supply, c.circulating,
	c.base, c.slope, c.royalty_bps, c.created_at,
	(SELECT count(*) FROM holdings h WHERE h.card_id = c.id AND h.qty > 0)`

func scanCard(row pgx.Row) (Card, error) {
	var c Card
	err := row.Scan(&c.ID, &c.Slug, &c.Name, &c.CreatorID, &c.SupplyType, &c.MaxSupply, &c.Circulating,
		&c.Base, &c.Slope, &c.RoyaltyBps, &c.CreatedAt, &c.Holders)
	if err != nil {
		return c, err
	}
	c.Price, err = c.curve().PriceAt(c.Circulating)
	return c, err
}

// ListCards returns all cards with their current price.
func (s *Service) ListCards(ctx context.Context) ([]Card, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cardColumns+` FROM cards c ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := []Card{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

// GetCard looks a card up by numeric id or slug.
func (s *Service) GetCard(ctx context.Context, ref string) (Card, error) {
	var row pgx.Row
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		row = s.db.QueryRow(ctx, `SELECT `+cardColumns+` FROM cards c WHERE c.id = $1`, id)
	} else {
		row = s.db.QueryRow(ctx, `SELECT `+cardColumns+` FROM cards c WHERE c.slug = $1`, ref)
	}
	c, err := scanCard(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Card{}, ErrCardNotFound
	}
	return c, err
}

// Quote prices a trade at the current supply without locking anything.
// The executed trade may differ if someone else trades first.
func (s *Service) Quote(ctx context.Context, cardRef string, side Side, qty int64) (Quote, error) {
	if !side.Valid() {
		return Quote{}, ErrInvalidSide
	}
	c, err := s.GetCard(ctx, cardRef)
	if err != nil {
		return Quote{}, err
	}
	if side == Buy && c.MaxSupply != nil && c.Circulating+qty > *c.MaxSupply {
		return Quote{}, ErrSupplyExceeded
	}
	return QuoteTrade(c.curve(), c.Circulating, side, qty)
}

// Holdings lists a player's positions valued at current prices.
func (s *Service) Holdings(ctx context.Context, userID int64) ([]Holding, error) {
	rows, err := s.db.Query(ctx, `
		SELECT c.id, c.slug, c.name, h.qty, h.cost_basis, c.base, c.slope, c.circulating
		FROM holdings h JOIN cards c ON c.id = h.card_id
		WHERE h.user_id = $1 AND h.qty > 0
		ORDER BY c.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Holding{}
	for rows.Next() {
		var h Holding
		var cv Curve
		var circ int64
		if err := rows.Scan(&h.CardID, &h.Slug, &h.Name, &h.Qty, &h.CostBasis, &cv.Base, &cv.Slope, &circ); err != nil {
			return nil, err
		}
		if h.Price, err = cv.PriceAt(circ); err != nil {
			return nil, err
		}
		h.AvgCost = h.CostBasis / h.Qty
		h.MarketValue = h.Price * h.Qty
		h.ProfitLoss = h.MarketValue - h.CostBasis
		out = append(out, h)
	}
	return out, rows.Err()
}

// ExecuteTrade runs a buy or sell as ONE database transaction. Either everything happens
// (ledger entries, wallet, holdings, supply, trade row) or nothing does.
//
// Lock order is always: card -> wallet -> holding. Every money transaction in the codebase
// must lock in this order, which is what makes deadlocks impossible.
//
// Idempotency: (user_id, idempotency_key) is unique. Repeating a request (double-click,
// network retry) returns the original trade with Replayed=true and moves no money.
func (s *Service) ExecuteTrade(ctx context.Context, req TradeRequest) (Trade, error) {
	if err := validateRequest(req); err != nil {
		return Trade{}, err
	}

	// Fast path: a retry of a trade that already completed.
	if t, found, err := s.findTrade(ctx, s.db, req.UserID, req.IdempotencyKey); err != nil {
		return Trade{}, err
	} else if found {
		return replay(t, req)
	}

	var result Trade
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		result, err = s.executeTx(ctx, tx, req)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "trades_user_id_idempotency_key_key" {
			// Same key raced in on a different card; the other request won. Return its result.
			t, found, ferr := s.findTrade(ctx, s.db, req.UserID, req.IdempotencyKey)
			if ferr != nil {
				return Trade{}, ferr
			}
			if found {
				return replay(t, req)
			}
		}
		return Trade{}, err
	}
	return result, nil
}

func (s *Service) executeTx(ctx context.Context, tx pgx.Tx, req TradeRequest) (Trade, error) {
	// 1. Lock the card. This serialises all trades on this card.
	var card Card
	err := tx.QueryRow(ctx, `
		SELECT id, supply_type, max_supply, circulating, base, slope, royalty_bps
		FROM cards WHERE id = $1 FOR UPDATE`, req.CardID).
		Scan(&card.ID, &card.SupplyType, &card.MaxSupply, &card.Circulating, &card.Base, &card.Slope, &card.RoyaltyBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, ErrCardNotFound
	}
	if err != nil {
		return Trade{}, fmt.Errorf("lock card: %w", err)
	}

	// Re-check idempotency now that we hold the card lock: a concurrent request with the
	// same key on this card has either committed (we see it) or not started its insert.
	if t, found, err := s.findTrade(ctx, tx, req.UserID, req.IdempotencyKey); err != nil {
		return Trade{}, err
	} else if found {
		return replay(t, req)
	}

	// 2. Lock the wallet.
	var accountID, balance int64
	err = tx.QueryRow(ctx, `SELECT account_id, balance FROM wallets WHERE user_id = $1 FOR UPDATE`, req.UserID).
		Scan(&accountID, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, ErrNoWallet
	}
	if err != nil {
		return Trade{}, fmt.Errorf("lock wallet: %w", err)
	}

	// 3. Lock the holding (may not exist yet).
	var held, costBasis int64
	err = tx.QueryRow(ctx, `SELECT qty, cost_basis FROM holdings WHERE user_id = $1 AND card_id = $2 FOR UPDATE`,
		req.UserID, req.CardID).Scan(&held, &costBasis)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, fmt.Errorf("lock holding: %w", err)
	}

	// 4. Price it on the curve.
	q, err := QuoteTrade(card.curve(), card.Circulating, req.Side, req.Qty)
	if err != nil {
		return Trade{}, err
	}

	// 5. Check the rules and build the ledger entries.
	var entries []ledger.Entry
	var newHeld, newCostBasis, newCirculating int64
	switch req.Side {
	case Buy:
		if card.MaxSupply != nil && card.Circulating+req.Qty > *card.MaxSupply {
			return Trade{}, ErrSupplyExceeded
		}
		if balance < q.Total {
			return Trade{}, ErrInsufficientFunds
		}
		entries = []ledger.Entry{
			{AccountID: accountID, Amount: -q.Total},
			{AccountID: s.ledger.Sys.Reserve, Amount: q.Amount},
			{AccountID: s.ledger.Sys.Burn, Amount: q.Fee},
		}
		newHeld = held + req.Qty
		newCostBasis = costBasis + q.Total
		newCirculating = card.Circulating + req.Qty

	case Sell:
		if held < req.Qty {
			return Trade{}, ErrInsufficientHoldings
		}
		entries = []ledger.Entry{
			{AccountID: s.ledger.Sys.Reserve, Amount: -q.Amount},
			{AccountID: accountID, Amount: q.Total},
			{AccountID: s.ledger.Sys.Burn, Amount: q.Fee},
		}
		newHeld = held - req.Qty
		// Remove the sold share of the cost basis; selling everything clears it exactly.
		newCostBasis = costBasis - costBasis*req.Qty/held
		if newHeld == 0 {
			newCostBasis = 0
		}
		newCirculating = card.Circulating - req.Qty
	}

	// 6. Write the ledger (also updates the cached wallet balance).
	kind := ledger.KindBuy
	if req.Side == Sell {
		kind = ledger.KindSell
	}
	txnID, err := s.ledger.Post(ctx, tx, kind, entries...)
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return Trade{}, ErrInsufficientFunds
	}
	if err != nil {
		return Trade{}, err
	}

	// 7. Update holdings and supply.
	if _, err := tx.Exec(ctx, `
		INSERT INTO holdings (user_id, card_id, qty, cost_basis) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, card_id) DO UPDATE SET qty = EXCLUDED.qty, cost_basis = EXCLUDED.cost_basis`,
		req.UserID, req.CardID, newHeld, newCostBasis); err != nil {
		return Trade{}, fmt.Errorf("update holding: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE cards SET circulating = $2 WHERE id = $1`, req.CardID, newCirculating); err != nil {
		return Trade{}, fmt.Errorf("update supply: %w", err)
	}

	// 8. Record the trade. The UNIQUE key is the final guard against duplicates.
	t := Trade{
		UserID: req.UserID, CardID: req.CardID, Side: req.Side, Qty: req.Qty,
		Amount: q.Amount, Fee: q.Fee, Royalty: q.Royalty, Total: q.Total, PriceAfter: q.PriceAfter,
		IdempotencyKey: req.IdempotencyKey,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO trades (user_id, card_id, side, qty, amount, fee, royalty, total, price_after,
		                    idempotency_key, ledger_txn_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, created_at`,
		t.UserID, t.CardID, string(t.Side), t.Qty, t.Amount, t.Fee, t.Royalty, t.Total, t.PriceAfter,
		t.IdempotencyKey, txnID).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return Trade{}, err
	}
	return t, nil
}

func validateRequest(req TradeRequest) error {
	if !req.Side.Valid() {
		return ErrInvalidSide
	}
	if req.Qty < 1 || req.Qty > MaxTradeQty {
		return ErrInvalidQty
	}
	if n := len(req.IdempotencyKey); n < 8 || n > 100 {
		return ErrInvalidKey
	}
	return nil
}

func (s *Service) findTrade(ctx context.Context, q ledger.Querier, userID int64, key string) (Trade, bool, error) {
	var t Trade
	var side string
	err := q.QueryRow(ctx, `
		SELECT id, user_id, card_id, side, qty, amount, fee, royalty, total, price_after, idempotency_key, created_at
		FROM trades WHERE user_id = $1 AND idempotency_key = $2`, userID, key).
		Scan(&t.ID, &t.UserID, &t.CardID, &side, &t.Qty, &t.Amount, &t.Fee, &t.Royalty, &t.Total,
			&t.PriceAfter, &t.IdempotencyKey, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, false, nil
	}
	t.Side = Side(side)
	return t, err == nil, err
}

// replay returns the stored trade for a repeated key, or an error if the key was reused
// for a different request.
func replay(t Trade, req TradeRequest) (Trade, error) {
	if t.CardID != req.CardID || t.Side != req.Side || t.Qty != req.Qty {
		return Trade{}, ErrIdempotencyKeyReused
	}
	t.Replayed = true
	return t, nil
}
