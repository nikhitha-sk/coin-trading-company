package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Report is the result of a reconciliation run.
type Report struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`

	// Money supply. Invariant: Minted == Burned + Reserve + InWallets.
	Minted    int64 `json:"minted"`
	Burned    int64 `json:"burned"`
	Reserve   int64 `json:"reserve"`
	InWallets int64 `json:"in_wallets"`

	Txns    int64 `json:"txns"`
	Entries int64 `json:"entries"`
}

func (r *Report) fail(format string, args ...any) {
	r.OK = false
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Reconcile proves the ledger is consistent, using one consistent snapshot of the database:
//   - every ledger txn sums to zero, and so does the whole ledger
//   - every cached wallet balance equals the sum of that user's entries
//   - no user account has a negative balance
//   - minted coins == burned + reserve + coins in wallets
//
// Market-level checks (reserve backing, holdings vs supply) live in market.Service.Reconcile.
func (l *Ledger) Reconcile(ctx context.Context, pool *pgxpool.Pool) (Report, error) {
	r := Report{OK: true, Problems: []string{}}

	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			return l.reconcileTx(ctx, tx, &r)
		})
	if err != nil {
		return r, err
	}
	return r, nil
}

// ReconcileTx runs the same checks inside an existing (ideally REPEATABLE READ) transaction.
func (l *Ledger) ReconcileTx(ctx context.Context, tx pgx.Tx) (Report, error) {
	r := Report{OK: true, Problems: []string{}}
	err := l.reconcileTx(ctx, tx, &r)
	return r, err
}

func (l *Ledger) reconcileTx(ctx context.Context, tx pgx.Tx, r *Report) error {
	var total int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0), count(*), count(DISTINCT txn_id) FROM ledger_entries`).
		Scan(&total, &r.Entries, &r.Txns); err != nil {
		return err
	}
	if total != 0 {
		r.fail("ledger does not sum to zero: %d", total)
	}

	rows, err := tx.Query(ctx, `
		SELECT txn_id, SUM(amount) FROM ledger_entries
		GROUP BY txn_id HAVING SUM(amount) <> 0 LIMIT 20`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			rows.Close()
			return err
		}
		r.fail("txn %d does not balance (sum = %d)", id, sum)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT w.user_id, w.balance, COALESCE(SUM(e.amount), 0) AS derived
		FROM wallets w
		LEFT JOIN ledger_entries e ON e.account_id = w.account_id
		GROUP BY w.user_id, w.balance
		HAVING w.balance <> COALESCE(SUM(e.amount), 0) OR COALESCE(SUM(e.amount), 0) < 0
		LIMIT 20`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var userID, cached, derived int64
		if err := rows.Scan(&userID, &cached, &derived); err != nil {
			rows.Close()
			return err
		}
		r.fail("wallet of user %d: cached balance %d, ledger says %d", userID, cached, derived)
	}
	rows.Close()

	var mint, burn int64
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(amount) FILTER (WHERE account_id = $1), 0),
			COALESCE(SUM(amount) FILTER (WHERE account_id = $2), 0),
			COALESCE(SUM(amount) FILTER (WHERE account_id = $3), 0)
		FROM ledger_entries`, l.Sys.Mint, l.Sys.Burn, l.Sys.Reserve).Scan(&mint, &burn, &r.Reserve); err != nil {
		return err
	}
	r.Minted, r.Burned = -mint, burn
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(balance), 0) FROM wallets`).Scan(&r.InWallets); err != nil {
		return err
	}
	if r.Minted != r.Burned+r.Reserve+r.InWallets {
		r.fail("money supply mismatch: minted %d != burned %d + reserve %d + wallets %d",
			r.Minted, r.Burned, r.Reserve, r.InWallets)
	}
	if r.Burned < 0 {
		r.fail("burn account is negative: %d", r.Burned)
	}
	if r.Reserve < 0 {
		r.fail("market reserve is negative: %d", r.Reserve)
	}
	return nil
}
