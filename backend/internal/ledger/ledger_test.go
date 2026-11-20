package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/testutil"
)

func setup(t *testing.T) (*pgxpool.Pool, *ledger.Ledger) {
	t.Helper()
	pool := testutil.NewDB(t)
	l, err := ledger.Load(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return pool, l
}

// newUser creates a user + wallet and mints `grant` coins into it.
func newUser(t *testing.T, pool *pgxpool.Pool, l *ledger.Ledger, name string, grant int64) (userID, accountID int64) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oauth_provider, oauth_id, name) VALUES ('test', $1, $1) RETURNING id`, name).
			Scan(&userID); err != nil {
			return err
		}
		var err error
		if accountID, err = l.OpenUserAccount(ctx, tx, userID); err != nil {
			return err
		}
		_, err = l.Post(ctx, tx, ledger.KindSignup,
			ledger.Entry{AccountID: l.Sys.Mint, Amount: -grant},
			ledger.Entry{AccountID: accountID, Amount: grant})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return userID, accountID
}

func mustReconcile(t *testing.T, pool *pgxpool.Pool, l *ledger.Ledger) ledger.Report {
	t.Helper()
	r, err := l.Reconcile(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("reconcile failed: %v", r.Problems)
	}
	return r
}

func TestSignupGrantUpdatesWalletAndSupply(t *testing.T) {
	pool, l := setup(t)
	userID, _ := newUser(t, pool, l, "alice", 10_000)

	var balance int64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance FROM wallets WHERE user_id = $1`, userID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 10_000 {
		t.Fatalf("balance = %d, want 10000", balance)
	}
	r := mustReconcile(t, pool, l)
	if r.Minted != 10_000 || r.InWallets != 10_000 {
		t.Fatalf("supply: %+v", r)
	}
}

func TestPostRejectsUnbalancedEntries(t *testing.T) {
	pool, l := setup(t)
	_, acct := newUser(t, pool, l, "bob", 100)
	ctx := context.Background()

	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := l.Post(ctx, tx, ledger.KindBuy,
			ledger.Entry{AccountID: acct, Amount: -50},
			ledger.Entry{AccountID: l.Sys.Burn, Amount: 49})
		return err
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("err = %v, want ErrUnbalanced", err)
	}
	mustReconcile(t, pool, l)
}

func TestDatabaseRejectsUnbalancedTxnEvenWithoutGo(t *testing.T) {
	pool, l := setup(t)
	ctx := context.Background()
	// Bypass the Go validation entirely: the deferred trigger must still catch it at COMMIT.
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var txn int64
		if err := tx.QueryRow(ctx, `INSERT INTO ledger_txns (kind) VALUES ('hack') RETURNING id`).Scan(&txn); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (txn_id, account_id, amount) VALUES ($1, $2, -1000)`,
			txn, l.Sys.Mint)
		return err
	})
	if err == nil {
		t.Fatal("expected commit of an unbalanced txn to fail")
	}
	mustReconcile(t, pool, l)
}

func TestLedgerIsAppendOnly(t *testing.T) {
	pool, l := setup(t)
	newUser(t, pool, l, "carol", 100)
	if _, err := pool.Exec(context.Background(), `UPDATE ledger_entries SET amount = amount * 2`); err == nil {
		t.Fatal("expected UPDATE on ledger_entries to be blocked")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("expected DELETE on ledger_entries to be blocked")
	}
}

func TestOverspendIsRejected(t *testing.T) {
	pool, l := setup(t)
	_, acct := newUser(t, pool, l, "dave", 100)
	ctx := context.Background()
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := l.Post(ctx, tx, ledger.KindBuy,
			ledger.Entry{AccountID: acct, Amount: -101},
			ledger.Entry{AccountID: l.Sys.Burn, Amount: 101})
		return err
	})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	mustReconcile(t, pool, l)
}

func TestReconcileDetectsCorruptedWallet(t *testing.T) {
	pool, l := setup(t)
	userID, _ := newUser(t, pool, l, "erin", 500)
	// Simulate a bug that changed a cached balance without a ledger entry.
	if _, err := pool.Exec(context.Background(),
		`UPDATE wallets SET balance = balance + 1 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	r, err := l.Reconcile(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("expected reconcile to detect the corrupted wallet")
	}
	t.Logf("detected: %v", r.Problems)
}
