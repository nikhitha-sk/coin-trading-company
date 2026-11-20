package db

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies all pending schema migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, mustSub(migrations, "migrations"))
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// StarterCard describes one of the cards seeded into every new market.
type StarterCard struct {
	Slug       string
	Name       string
	SupplyType string
	MaxSupply  *int64
	Base       int64
	Slope      int64
}

func ptr(v int64) *int64 { return &v }

// StarterCards is the initial market. A mix of cheap/expensive and fixed/unlimited supply.
var StarterCards = []StarterCard{
	{"golden-monkey", "Golden Monkey", "fixed", ptr(500), 120, 4},
	{"copper-penny", "Copper Penny", "unlimited", nil, 10, 2},
	{"silver-stag", "Silver Stag", "fixed", ptr(2000), 60, 2},
	{"iron-anvil", "Iron Anvil", "unlimited", nil, 40, 2},
	{"crystal-owl", "Crystal Owl", "fixed", ptr(250), 300, 10},
	{"jade-dragon", "Jade Dragon", "fixed", ptr(100), 900, 40},
	{"paper-crane", "Paper Crane", "unlimited", nil, 5, 2},
	{"ruby-fox", "Ruby Fox", "fixed", ptr(1000), 150, 6},
}

// Seed inserts the starter cards. Safe to run repeatedly.
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	for _, c := range StarterCards {
		_, err := pool.Exec(ctx, `
			INSERT INTO cards (slug, name, supply_type, max_supply, base, slope)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (slug) DO NOTHING`,
			c.Slug, c.Name, c.SupplyType, c.MaxSupply, c.Base, c.Slope)
		if err != nil {
			return fmt.Errorf("seed card %s: %w", c.Slug, err)
		}
	}
	return nil
}
