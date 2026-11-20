package db_test

import (
	"context"
	"testing"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/db"
	"github.com/nikhitha-sk/coin-trading-company/backend/internal/testutil"
)

func TestMigrateAndSeedAreIdempotent(t *testing.T) {
	pool := testutil.NewDB(t) // already migrated + seeded once
	ctx := context.Background()

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := db.Seed(ctx, pool); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	var cards, systemAccounts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cards`).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE kind = 'system'`).Scan(&systemAccounts); err != nil {
		t.Fatal(err)
	}
	if cards != len(db.StarterCards) {
		t.Errorf("cards = %d, want %d", cards, len(db.StarterCards))
	}
	if systemAccounts != 3 {
		t.Errorf("system accounts = %d, want 3", systemAccounts)
	}
}

func TestSchemaRejectsOddSlope(t *testing.T) {
	pool := testutil.NewDB(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO cards (slug, name, supply_type, base, slope) VALUES ('odd', 'Odd', 'unlimited', 10, 3)`)
	if err == nil {
		t.Fatal("expected odd slope to be rejected")
	}
}
