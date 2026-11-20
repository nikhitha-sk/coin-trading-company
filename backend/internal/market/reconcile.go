package market

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nikhitha-sk/coin-trading-company/backend/internal/ledger"
)

// Reconcile runs the ledger checks plus market-level invariants, on one consistent snapshot:
//   - for every card, the sum of all holdings equals circulating supply
//   - the market reserve holds exactly the area under every card's curve from 0 to its
//     circulating supply, i.e. it can always pay out every holder who sells
//     (this will become ">=" once Phase 4 price drift tops the reserve up)
func (s *Service) Reconcile(ctx context.Context) (ledger.Report, error) {
	var report ledger.Report
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			if report, err = s.ledger.ReconcileTx(ctx, tx); err != nil {
				return err
			}
			return s.reconcileMarket(ctx, tx, &report)
		})
	return report, err
}

func (s *Service) reconcileMarket(ctx context.Context, tx pgx.Tx, r *ledger.Report) error {
	rows, err := tx.Query(ctx, `
		SELECT c.slug, c.circulating, c.base, c.slope, COALESCE(SUM(h.qty), 0)
		FROM cards c LEFT JOIN holdings h ON h.card_id = c.id
		GROUP BY c.id ORDER BY c.id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var expectedReserve int64
	for rows.Next() {
		var slug string
		var circ, held int64
		var cv Curve
		if err := rows.Scan(&slug, &circ, &cv.Base, &cv.Slope, &held); err != nil {
			return err
		}
		if held != circ {
			fail(r, "card %s: holdings sum to %d but circulating is %d", slug, held, circ)
		}
		area, err := cv.Area(0, circ)
		if err != nil {
			fail(r, "card %s: reserve area overflow", slug)
			continue
		}
		expectedReserve += area
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if r.Reserve != expectedReserve {
		fail(r, "market reserve is %d but the curves require exactly %d", r.Reserve, expectedReserve)
	}
	return nil
}

func fail(r *ledger.Report, format string, args ...any) {
	r.OK = false
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}
