# Coin Trading Company

[![CI](https://github.com/nikhitha-sk/coin-trading-company/actions/workflows/ci.yml/badge.svg)](https://github.com/nikhitha-sk/coin-trading-company/actions/workflows/ci.yml)

A multiplayer market game: trade cards whose prices move with real demand, mint your own, and climb the leaderboard, on top of a double-entry ledger that never loses a coin.

> **Status:** Phase 1 (money core) ✅ done. Next: Phase 2 (live charts + WebSockets + web UI).
> See [plan.md](plan.md) for the full build plan and [docs/spec.pdf](docs/spec.pdf) for the original spec.

**Stack:** Go (chi, pgx), PostgreSQL, Redis, Docker Compose · Next.js frontend coming in Phase 2.

---

## Quick start

Requirements: Docker. (Go 1.27+ if you want to run the API or tests outside Docker.)

```bash
docker compose up -d --build          # postgres, redis and the API on http://localhost:8081
curl http://localhost:8081/healthz    # {"db":"ok","redis":"ok"}
```

The API migrates the database and seeds 8 starter cards on start-up.

**Windows (PowerShell):** `.\scripts\dev.ps1 up`, `.\scripts\dev.ps1 test`, `.\scripts\dev.ps1 down`.
**Linux/macOS:** `make up`, `make test`, `make down`.

Ports are deliberately unusual so they don't clash with anything you already run:
Postgres **55432**, Redis **56379**, API **8081**.

## Play from the terminal (two players)

```bash
B=http://localhost:8081
curl -c alice.txt -X POST $B/dev/login -d '{"name":"alice"}'      # new players get 10,000 coins
curl -c bob.txt   -X POST $B/dev/login -d '{"name":"bob"}'

curl "$B/cards"                                                    # the market
curl "$B/quote?card=golden-monkey&side=buy&qty=10"                 # exact cost incl. 2% fee

curl -b alice.txt -X POST $B/trades \
  -d '{"card_id":1,"side":"buy","qty":10,"idempotency_key":"alice-0001"}'
curl -b bob.txt -X POST $B/trades \
  -d '{"card_id":1,"side":"buy","qty":5,"idempotency_key":"bob-00001"}'   # pushes the price up
curl -b alice.txt -X POST $B/trades \
  -d '{"card_id":1,"side":"sell","qty":10,"idempotency_key":"alice-0002"}' # alice sells for a profit

curl -b alice.txt $B/me              # balance
curl -b alice.txt $B/me/holdings     # positions with avg cost and P/L
curl $B/dev/reconcile                # proves no coin was created or lost
```

Send the same `idempotency_key` twice and you get the original trade back (`"replayed": true`) without being charged again.

## API (Phase 1)

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/healthz` | | DB + Redis health |
| POST | `/dev/login` | | `{"name"}` log in or sign up (**dev only**) |
| POST | `/logout` | | clear session |
| GET | `/me` | ✓ | player + balance |
| GET | `/me/holdings` | ✓ | positions, avg cost, market value, P/L |
| GET | `/cards` | | all cards with price, supply, holders |
| GET | `/cards/{id or slug}` | | one card |
| GET | `/quote?card=&side=&qty=` | | price a trade without executing it |
| POST | `/trades` | ✓ | `{"card_id","side","qty","idempotency_key"}` → 201 new, 200 replay |
| GET | `/dev/reconcile` | | full money audit (**dev only**) |

Errors look like `{"error":{"code":"insufficient_funds","message":"..."}}` with status 400 / 401 / 402 / 404 / 409.

## How the money stays correct

**Double-entry ledger.** Every coin movement is a ledger transaction whose entries sum to zero:

| Event | Entries |
|---|---|
| Sign-up | `system:mint −10,000` · `user +10,000` |
| Buy | `user −(amount + fee)` · `market_reserve +amount` · `burn +fee` |
| Sell | `market_reserve −amount` · `user +(amount − fee)` · `burn +fee` |

- The database enforces it: a deferred constraint trigger rejects any transaction that doesn't sum to zero at COMMIT, and the ledger tables are append-only.
- Wallet balances are a cache of the ledger, updated in the same transaction, with `CHECK (balance >= 0)` as a last line of defence.
- System accounts have no cached balance row, so they never become a hot row that every trade in the game waits on.

**One transaction per trade, fixed lock order.** `card → wallet → holding`, always. Trades on the same card queue on the card row. A player's trades queue on their wallet. A consistent order means no deadlocks.

**Idempotency.** Each trade carries a client-generated key. `UNIQUE (user_id, idempotency_key)` plus a re-check under the card lock means double-clicks and retries return the original trade instead of trading twice.

**Exact integer pricing.** `price = base + slope × supply`. A trade costs the exact area under the curve, `base·n + (slope/2)·n·(s0+s1)`. Slope is always even, so this is an exact integer and splitting an order never changes the curve cost. The 2% fee rounds **up** with a 1-coin minimum, so tiny orders can't dodge it. All maths is checked `int64`; there are no floats anywhere near money.

**Reconciliation** (`GET /dev/reconcile`) checks on one consistent snapshot that:
- every transaction balances, and the whole ledger sums to zero
- every wallet equals the sum of its ledger entries
- `minted == burned + reserve + in_wallets`
- for each card, holdings sum to circulating supply
- the market reserve holds exactly the area under every card's curve, so it can always pay every seller

## Tests

```bash
docker compose up -d postgres redis
cd backend
TEST_DATABASE_URL="postgres://coin:coin@localhost:55432/postgres?sslmode=disable" go test -count=1 ./...
```

Each test gets its own throwaway database. Highlights:

| Test | What it proves |
|---|---|
| `TestRaceForTheLastUnit` | 100 players buy the last unit at once → exactly 1 wins |
| `TestConcurrentDoubleSpend` | 50 simultaneous buys from a wallet that can afford 1 → exactly 1 succeeds |
| `TestIdempotentRetries` | same request 20× concurrently → 1 trade, charged once |
| `TestStressNoDeadlocksNoLeaks` | 32 workers × 20 players random trading → no deadlocks, zero drift (≈440 trades/sec on a laptop) |
| `TestAreaIsAdditive` / `TestSplittingNeverReducesTotalCost` | curve maths can't be gamed by splitting orders |
| `TestRoundTripAlwaysLosesMoney` | buy→sell loops always lose to fees |
| `TestDatabaseRejectsUnbalancedTxnEvenWithoutGo` | the DB itself refuses unbalanced ledger writes |

Set `STRESS_SECONDS=30` for a longer stress run. CI runs everything with the race detector.

## Project layout

```
backend/
  cmd/api/              HTTP server entry point
  internal/config/      env config
  internal/db/          pool, embedded goose migrations, seed cards
  internal/ledger/      double-entry posting + reconciliation
  internal/market/      bonding curve, trade service, market reconciliation
  internal/auth/        dev login, signup grant, signed session cookies
  internal/httpapi/     routes, JSON, error mapping
  internal/testutil/    per-test databases
docs/spec.pdf           original project spec
plan.md                 phase-by-phase build plan + progress tracker
```

## License

MIT
