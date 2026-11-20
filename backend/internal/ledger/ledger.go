// Package ledger implements the double-entry ledger that every coin movement goes through.
//
// Rules:
//   - Every transaction is a set of entries whose amounts sum to exactly zero.
//   - Coins are int64 everywhere. Never floats.
//   - User balances are cached in wallets and updated in the same DB transaction as the entries.
//   - System accounts (mint, burn, market reserve) have no cached balance, so they never become
//     a hot row that serialises every trade in the game.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Kind labels what a ledger transaction was for.
type Kind string

const (
	KindSignup Kind = "signup"
	KindBuy    Kind = "buy"
	KindSell   Kind = "sell"
)

// Entry is one line of a ledger transaction: a signed amount against an account.
type Entry struct {
	AccountID int64
	Amount    int64
}

var (
	ErrUnbalanced        = errors.New("ledger: entries do not sum to zero")
	ErrInvalidEntry      = errors.New("ledger: invalid entry")
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	ErrUnknownAccount    = errors.New("ledger: unknown account")
)

// SystemAccounts holds the IDs of the built-in system accounts.
type SystemAccounts struct {
	Mint    int64 // source of all new coins; its balance is minus the total ever issued
	Burn    int64 // sink for fees and minting costs; only ever grows
	Reserve int64 // market maker: receives coins on buys, pays them out on sells
}

// Ledger posts balanced transactions.
type Ledger struct {
	Sys SystemAccounts
}

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Load looks up the system account IDs.
func Load(ctx context.Context, q Querier) (*Ledger, error) {
	l := &Ledger{}
	rows, err := q.Query(ctx, `SELECT id, name FROM accounts WHERE kind = 'system'`)
	if err != nil {
		return nil, fmt.Errorf("load system accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		switch name {
		case "system:mint":
			l.Sys.Mint = id
		case "system:burn":
			l.Sys.Burn = id
		case "system:market_reserve":
			l.Sys.Reserve = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if l.Sys.Mint == 0 || l.Sys.Burn == 0 || l.Sys.Reserve == 0 {
		return nil, errors.New("ledger: system accounts missing (did migrations run?)")
	}
	return l, nil
}

func (l *Ledger) isSystem(accountID int64) bool {
	return accountID == l.Sys.Mint || accountID == l.Sys.Burn || accountID == l.Sys.Reserve
}

// OpenUserAccount creates the ledger account and empty wallet for a new user.
func (l *Ledger) OpenUserAccount(ctx context.Context, tx pgx.Tx, userID int64) (accountID int64, err error) {
	err = tx.QueryRow(ctx,
		`INSERT INTO accounts (kind, user_id, name) VALUES ('user', $1::bigint, 'user:' || $1::bigint) RETURNING id`,
		userID).Scan(&accountID)
	if err != nil {
		return 0, fmt.Errorf("create account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallets (user_id, account_id, balance) VALUES ($1, $2, 0)`, userID, accountID); err != nil {
		return 0, fmt.Errorf("create wallet: %w", err)
	}
	return accountID, nil
}

// Post records a balanced transaction inside the caller's DB transaction and updates the
// cached wallet balances of any user accounts involved. Callers must already hold
// FOR UPDATE locks on those wallets (see market.Service for the lock order).
func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, kind Kind, entries ...Entry) (txnID int64, err error) {
	if err := validate(entries); err != nil {
		return 0, err
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns (kind) VALUES ($1) RETURNING id`, string(kind)).Scan(&txnID); err != nil {
		return 0, fmt.Errorf("insert ledger txn: %w", err)
	}

	accounts := make([]int64, len(entries))
	amounts := make([]int64, len(entries))
	for i, e := range entries {
		accounts[i], amounts[i] = e.AccountID, e.Amount
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (txn_id, account_id, amount)
		SELECT $1, a, m FROM unnest($2::bigint[], $3::bigint[]) AS t(a, m)`,
		txnID, accounts, amounts); err != nil {
		return 0, fmt.Errorf("insert ledger entries: %w", err)
	}

	for _, e := range entries {
		if l.isSystem(e.AccountID) {
			continue
		}
		tag, err := tx.Exec(ctx,
			`UPDATE wallets SET balance = balance + $2 WHERE account_id = $1`, e.AccountID, e.Amount)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.ConstraintName == "wallets_balance_check" {
				return 0, ErrInsufficientFunds
			}
			return 0, fmt.Errorf("update wallet: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("%w: %d", ErrUnknownAccount, e.AccountID)
		}
	}
	return txnID, nil
}

func validate(entries []Entry) error {
	if len(entries) < 2 {
		return fmt.Errorf("%w: need at least two entries", ErrInvalidEntry)
	}
	var sum int64
	for _, e := range entries {
		if e.Amount == 0 || e.AccountID == 0 {
			return fmt.Errorf("%w: %+v", ErrInvalidEntry, e)
		}
		sum += e.Amount
	}
	if sum != 0 {
		return fmt.Errorf("%w (sum = %d)", ErrUnbalanced, sum)
	}
	return nil
}
