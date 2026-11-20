# Coin Trading Company: Build Plan

A multiplayer market game: players trade cards whose prices move with real demand, mint their own cards, and climb a leaderboard. Underneath is a double-entry ledger that never loses a coin.

**Stack:** Go (chi, pgx, sqlc), PostgreSQL, Redis, WebSockets (coder/websocket), robfig/cron, Next.js + Tailwind + Zustand + lightweight-charts, Docker Compose.

**Estimated time:** 7–8 weeks part-time (about 10–15 hrs/week), split into 4 phases.

---

## Progress tracker

| Phase | Focus | Target weeks | Status |
|-------|-------|--------------|--------|
| 1 | Foundation: money core (ledger, wallets, buy/sell) | Weeks 1–2 | ✅ Done |
| 2 | Market and real-time (curve, ticks, WebSockets, UI) | Weeks 3–4 | ⬜ Not started |
| 3 | Game layer and economy (portfolio, leaderboard, rewards, card creation, anti-exploit) | Weeks 5–6 | ⬜ Not started |
| 4 | Living market and launch (drift, events, newspaper, OAuth, deploy, README) | Weeks 7–8 | ⬜ Not started |

Status key: ⬜ Not started · 🟨 In progress · ✅ Done

**Rule:** don't start a phase until the previous phase's **exit criteria** all pass. Each phase ends with something working and demo-able.

---

## Decisions to lock in before writing code

These came out of reviewing the spec. Settle them now so later phases don't need rewrites.

- [x] **Money is `BIGINT` integer coins everywhere**, including curve maths. No `float` anywhere money is touched (Go `int64`, SQL `BIGINT`, JSON numbers as integers).
- [x] **The fee rounds UP**, plus a minimum fee of 1 coin: `fee = max(1, ceil(area * FeeBps / 10_000))`. With rounding down, trades worth less than 50 coins would pay 0 fee, and players could split orders to avoid fees.
- [x] **Curve maths without truncation:** keep `slope` an even number, or do the curve maths in half-coins, so `slope*(s1² − s0²)/2` is always exact. One big order must then cost exactly the same as many small ones (a test proves this).
- [x] **Ledger accounts:**
  - `user:<id>`: one per player
  - `system:mint`: source of all new coins (its balance goes negative by the amount issued)
  - `system:burn`: where fees and minting costs go (its balance only grows)
  - `system:market_reserve`: receives coins from buys and pays sellers
  - **Invariant:** the sum of every account's balance is always **0**. Every transaction's entries also sum to 0.
- [x] **Sell price:** selling `n` pays the area under the curve from `supply − n` to `supply`, minus the fee. It is paid out of `market_reserve`.
- [ ] **Holding cap:** 20% of `max_supply` for fixed-supply cards. For unlimited-supply cards, 20% of current circulating supply, or 100 units, whichever is larger. This stops the first buyer being blocked.
- [ ] **Price drift (Phase 4):** price = `base + slope × supply + drift_offset`, with `drift_offset` limited to ±10% of the curve price. If drift lets sellers withdraw more than the reserve holds, the shortfall is topped up with an explicit `system:mint` entry tagged `kind = 'drift'`, so inflation can be seen and measured.
- [x] **Lock order** inside every money transaction: **card row → wallet rows (by ascending user_id) → holdings**. Never in any other order.

---

## Project structure (target)

```
coin-trading-company/
├── docker-compose.yml
├── Makefile                  # make up, make test, make migrate, make sim
├── backend/
│   ├── cmd/
│   │   ├── api/              # HTTP + WebSocket server
│   │   ├── scheduler/        # cron jobs
│   │   └── sim/              # bot simulation tool
│   ├── internal/
│   │   ├── db/               # sqlc generated code + queries/*.sql
│   │   ├── ledger/           # double-entry posting, reconciliation
│   │   ├── market/           # bonding curve, trade service
│   │   ├── realtime/         # WebSocket hub, Redis subscriber
│   │   ├── auth/             # sessions, OAuth, dev login
│   │   ├── rewards/          # daily claims
│   │   ├── leaderboard/
│   │   └── news/
│   └── migrations/           # goose or golang-migrate SQL files
└── frontend/                 # Next.js app
    ├── app/                  # pages: /market, /card/[id], /portfolio, /leaderboard, /create, /news
    ├── components/
    └── lib/                  # api client, ws client, zustand stores
```

---

# Phase 1: Foundation, the money core (Weeks 1–2)

**Goal:** a backend where players can buy and sell cards safely. No UI yet beyond curl/Postman. This phase is the heart of the project. If money correctness is solid here, everything after it is features.

### 1.1 Project setup
- [x] Create repo, `.gitignore`, `README.md` stub, `LICENSE`
- [x] `docker-compose.yml` with `postgres:16`, `redis:7`, and the `api` service (hot reload via `air` optional)
- [x] Go module init; add `chi`, `pgx/v5`, `sqlc`, a migration tool (`goose` or `golang-migrate`)
- [x] `Makefile` targets: `up`, `down`, `migrate`, `sqlc`, `test`, `lint`
- [x] Config from env vars (`DATABASE_URL`, `REDIS_URL`, `SESSION_SECRET`, `APP_ENV`)
- [x] Health endpoint `GET /healthz` checks DB + Redis
- [x] GitHub Actions CI: run `go vet`, `golangci-lint`, and tests against a Postgres service container

### 1.2 Database schema (migrations)
- [x] `users` (id, oauth_provider, oauth_id UNIQUE, name, created_at)
- [x] `accounts` (id, kind: user|system, user_id NULL, name UNIQUE, e.g. `system:burn`)
- [x] `wallets` (user_id PK, balance BIGINT NOT NULL CHECK (balance >= 0)): cached balance
- [x] `ledger_txns` (id, kind: signup|buy|sell|reward|mint_card|drift, created_at)
- [x] `ledger_entries` (id, txn_id FK, account_id FK, amount BIGINT, created_at)
- [x] Constraint trigger or deferred check: **entries per txn_id sum to 0**
- [x] `cards` (id, slug UNIQUE, name, creator_id NULL, supply_type fixed|unlimited, max_supply NULL, circulating BIGINT, base BIGINT, slope BIGINT, royalty_bps INT CHECK (≤ 1000), created_at)
- [x] `holdings` (user_id, card_id, qty BIGINT CHECK (qty >= 0), avg_cost BIGINT, PK(user_id, card_id))
- [x] `trades` (id, user_id, card_id, side, qty, total, fee, royalty, price_after, idempotency_key, created_at, **UNIQUE(user_id, idempotency_key)**)
- [x] Seed script: create system accounts and 8–10 starter cards with different base/slope values

### 1.3 Ledger package (`internal/ledger`)
- [x] `Post(tx, kind, entries []Entry) (txnID, error)`: inserts txn + entries inside the caller's DB transaction, rejects if entries don't sum to 0, and updates `wallets.balance` for user accounts
- [x] `Balance(account)` derived from entries (for verification)
- [x] `Reconcile()`: checks (a) every wallet balance = sum of its entries, (b) all entries sum to 0 globally, (c) no negative user balance. Returns a report.
- [x] Unit tests: posting unbalanced entries fails; reconcile catches a manually corrupted wallet

### 1.4 Auth (dev mode first)
- [x] Session cookies (signed, HttpOnly, SameSite=Lax)
- [x] **Dev-only login:** `POST /dev/login {name}` creates or loads a user. Enabled only when `APP_ENV=dev`. This lets you test many players without OAuth.
- [x] On first login: create user + wallet, and post a **signup grant**: `system:mint −10,000 / user +10,000`
- [x] `GET /me` returns user + balance
- [x] Middleware: `RequireUser`

### 1.5 Bonding curve (`internal/market/curve.go`)
- [x] `Price(card) int64` = base + slope × circulating
- [x] `BuyCost(card, n) (area, fee)` using the exact-area formula
- [x] `SellProceeds(card, n) (area, fee)` = area from s−n to s
- [x] Fee: round up, minimum 1
- [x] **Unit tests:**
  - buying `n` at once == buying 1 at a time `n` times (area part), for many random cards
  - buy n then sell n always loses money (because of fees)
  - no overflow at max supply / max price (test the extremes)

### 1.6 Trade service + endpoints
- [x] `POST /trades` `{card_id, side, qty, idempotency_key}`
- [x] Inside one DB transaction:
  1. Check idempotency: if `(user_id, key)` already exists, return the original result (no-op)
  2. `SELECT … FROM cards WHERE id=$1 FOR UPDATE`
  3. `SELECT … FROM wallets WHERE user_id=$1 FOR UPDATE`
  4. Compute cost/proceeds + fee
  5. Validate: qty > 0, balance ≥ cost, supply not exceeded, holding ≥ qty for sells
  6. Post ledger entries: buyer −total, `market_reserve` +area, `burn` +fee (royalty comes in Phase 3)
  7. Upsert holdings (update avg_cost on buy), update `cards.circulating`
  8. Insert trade row
  9. COMMIT
- [x] `GET /cards`, `GET /cards/:id`, `GET /quote?card=&side=&qty=` (preview cost incl. fee, no lock)
- [x] Map errors to clean HTTP responses (400 validation, 402 insufficient funds, 409 conflict)

### 1.7 Correctness tests (the important ones)
- [x] **Race test:** a card with 1 unit left, and 100 goroutines buy it at once. Exactly **one** succeeds and the ledger reconciles.
- [x] **Double-spend test:** one user with balance for exactly 1 purchase fires 50 concurrent buys. Only 1 succeeds and the balance never goes negative.
- [x] **Idempotency test:** same key sent 20 times concurrently gives 1 trade row and the same response every time
- [x] **Deadlock test:** many users buy and sell many cards concurrently for 30 s with no deadlock errors
- [x] After every test: `Reconcile()` passes

### ✅ Phase 1 exit criteria
- [x] `docker compose up` starts everything; `make test` is green in CI
- [x] Can log in as 2 dev users, buy and sell via curl, and see balances change
- [x] Race, double-spend, idempotency and deadlock tests all pass
- [x] Reconcile reports **0 drift**

### 📝 Phase 1 notes (what was actually built)
- **Plain pgx instead of sqlc.** The trade transaction is hand-written SQL with explicit `FOR UPDATE` locks, which keeps the locking easy to read. sqlc can still be added later for simple queries.
- **gofmt + go vet in CI instead of golangci-lint.** Tests run with `-race`. golangci-lint can be added later.
- **Makefile** has `up / db / down / run / test / test-race / vet / fmt`. Migrations and seeding run automatically on API start, so there are no separate targets. Windows uses `scripts/dev.ps1`.
- **Holdings store `cost_basis` (total paid)** instead of `avg_cost`. The average is derived, so there is no rounding drift.
- **Extra safety beyond the plan:** ledger tables are append-only (trigger), a DB-level check rejects unbalanced txns at COMMIT, and market reconciliation proves the reserve can always pay every seller.
- **Local stress run:** ~2,200 trades in 5 s (~440 trades/sec) with 20 players / 32 workers, 0 drift.
- **Ports:** Postgres 55432, Redis 56379, API 8081 (5432/5433/8080 were already in use locally).

---

# Phase 2: Market and real-time (Weeks 3–4)

**Goal:** a playable web UI where a trade in one browser instantly moves the chart in another.

### 2.1 Price history
- [ ] `price_ticks` table (card_id, ts, price): write one row per trade, inside the trade transaction
- [ ] `candles_1m` table or materialized rollup (card_id, bucket, open, high, low, close, volume)
- [ ] Rollup job (simple goroutine on a ticker for now; moves to the scheduler in Phase 4)
- [ ] `GET /cards/:id/candles?from=&to=&interval=1m|5m|1h`
- [ ] 24h change + 24h volume computed for the market list (cache in Redis, 10 s TTL)

### 2.2 Redis pub/sub
- [ ] **After COMMIT** (never before), publish to `ticks` channel: `{card, price, qty, side, player, ts}`
- [ ] If publish fails, log it and continue. The trade is already safe in Postgres, and the next tick will correct the price.

### 2.3 Realtime hub (`internal/realtime`)
- [ ] `GET /ws` upgrades to WebSocket (requires session)
- [ ] Hub subscribes to Redis `ticks`, buffers for **250 ms**, then broadcasts one batched message:
  ```json
  {"type":"ticks","t":0,"prices":[{"card":"","price":0,"chg24h":0,"vol1m":0}],"trades":[{"card":"","side":"","qty":0,"player":""}]}
  ```
- [ ] Per-client bounded send buffer. If a client is slow, **drop old frames** rather than blocking others.
- [ ] Ping/pong heartbeat; clean disconnect handling
- [ ] Optional: client can subscribe to specific cards (`{"type":"sub","cards":[...]}`)
- [ ] Test: 200 fake WebSocket clients connected, and a trade reaches all of them in < 500 ms

### 2.4 Frontend setup
- [ ] Next.js (App Router) + Tailwind + Zustand
- [ ] API client with cookie auth; WebSocket client with auto-reconnect + backoff
- [ ] Dev login page (name field), shown only in dev
- [ ] Layout: top bar with balance (live), nav: Market · Portfolio · Leaderboard

### 2.5 Market UI
- [ ] **Market page:** table/grid of cards with price, 24h %, supply, holders, sparkline. Prices update live from WebSocket.
- [ ] **Card page:** candle chart (lightweight-charts) updating live; buy/sell panel
- [ ] **Trade panel:**
  - Qty input, live **quote** (cost, fee, price after trade) via `/quote`
  - Generates an idempotency key (UUID) when the form opens; reuses it on retry
  - Disable button while pending; show success/error toast
- [ ] **Trade feed ticker:** last 20 trades across the market, scrolling live

### ✅ Phase 2 exit criteria
- [ ] Two browsers (normal + incognito) as different users: a buy in one moves the chart and feed in the other within ~0.5 s
- [ ] Double-clicking Buy creates exactly one trade
- [ ] Killing and restarting the API: clients reconnect automatically
- [ ] Charts show correct history after a page refresh

---

# Phase 3: Game layer and economy (Weeks 5–6)

**Goal:** turn the market into a game with goals, progression, and an economy that bots can't break.

### 3.1 Portfolio
- [ ] `GET /portfolio`: holdings with qty, avg cost, current price, unrealized P/L, % P/L
- [ ] Net worth = balance + Σ(holding qty × current price)
- [ ] Optional "liquidation value" (what you'd actually get selling everything, using the curve + fee). This is honest and a good talking point.
- [ ] Portfolio page with holdings table + small net-worth history chart (snapshot every 10 min into `networth_snapshots`)

### 3.2 Leaderboard
- [ ] Redis ZSET `leaderboard:networth`, updated by a job every 60 s
- [ ] `GET /leaderboard?limit=50` + "your rank" (`ZREVRANK`)
- [ ] Rebuild command: recompute the whole ZSET from Postgres (proves Redis is only a cache)
- [ ] Leaderboard page, highlighting the current user

### 3.3 Daily rewards
- [ ] `daily_claims` (user_id, day DATE, streak, amount, **UNIQUE(user_id, day)**)
- [ ] `POST /rewards/claim`: in one transaction, insert claim row (unique constraint blocks double claims) and post `system:mint → user`
- [ ] Streak: if yesterday claimed then streak+1, else 1. Amount = base 500 + 100 × min(streak, 7)
- [ ] Day boundary in **UTC** (document it)
- [ ] UI: claim button with streak counter; disabled after claiming
- [ ] Test: 20 concurrent claims result in exactly 1 success

### 3.4 Card creation
- [ ] Unlock check: net worth ≥ 50,000
- [ ] `POST /cards` `{name, art, supply_type, max_supply?, royalty_bps}`
- [ ] Validation: name length, profanity/blocklist filter, unique slug, royalty ≤ 10%, max_supply range
- [ ] Art: upload to local disk / S3-compatible storage, or start with a preset icon + colour picker (simpler)
- [ ] Minting fee (e.g. 5,000 coins), **burned**: `user → system:burn`
- [ ] Limit: max 1 new card per user per 24 h
- [ ] Create card page with live preview

### 3.5 Royalties
- [ ] On every trade of a user-created card: royalty = ceil(area × royalty_bps / 10,000) paid to creator
- [ ] Ledger entries per buy: buyer −(area+fee+royalty), reserve +area, burn +fee, creator +royalty
- [ ] Lock order update: card → wallets sorted by user_id (buyer and creator) → holdings
- [ ] **No royalty when creator trades their own card** (blocks self-farming)

### 3.6 Anti-exploit rules
- [ ] **Holding cap** (20% rule as decided above), enforced inside the trade transaction
- [ ] **Cooldown:** 10 s per (user, card) using Redis `SET key NX EX 10`
- [ ] **Rate limit:** e.g. 30 trades/min per user (Redis sliding window or token bucket)
- [ ] Max qty per single trade (e.g. 1,000)
- [ ] Return clear error messages so the UI can show "Wait 7 s" etc.

### 3.7 Bot simulation (`cmd/sim`)
- [ ] Spawns N bots (default 50) via dev login, each with a strategy: random, momentum (buy rising cards), whale (tries to corner a card), farmer (creates cards + self-trades), looper (buy→sell instantly)
- [ ] Runs for X minutes or Y trades, then prints:
  - total trades, trades/sec, errors by type
  - money supply: minted, burned, in wallets, in reserve
  - **Reconcile result (must be 0 drift)**
  - per-strategy profit/loss (loopers and farmers **must lose money**)
  - Gini coefficient of wealth
- [ ] Use results to tune: fee %, slope values, reward amounts, minting fee

### ✅ Phase 3 exit criteria
- [ ] Sim runs **10,000+ trades** with **0 coins created or lost** (reconcile clean)
- [ ] Looper and farmer bots end with **less** money than they started with
- [ ] Whale bot is blocked by the holding cap
- [ ] A player can go from signup to creating their own card in a normal play session
- [ ] Leaderboard rebuild from Postgres matches the live ZSET

---

# Phase 4: Living market and launch (Weeks 7–8)

**Goal:** the market feels alive even when few people are online, real users can sign in, and it's deployed with a great README.

### 4.1 Scheduler service (`cmd/scheduler`)
- [ ] Separate binary using `robfig/cron`; uses a Postgres advisory lock so only one instance runs jobs
- [ ] Move candle rollup + leaderboard refresh + net-worth snapshots here
- [ ] **Every minute: price drift.** Random walk on `drift_offset` per card, bounded to ±10%; record a tick; publish to Redis
- [ ] **Reserve check after drift:** if `market_reserve` can't cover all holders selling, top it up with a `system:mint` entry tagged `drift` (visible inflation)
- [ ] **Market events** (random, e.g. 1–2 per day): "Market crash" (category −15% offset), "Gold rush" (category +15%). Store in `market_events` and broadcast over WebSocket as a banner.
- [ ] **Nightly (UTC midnight):** run `Reconcile()` and store the result in `reconciliation_runs`; alert/log loudly on any drift
- [ ] Add `category` column to cards for events

### 4.2 Newspaper
- [ ] `news_items` (id, day, headline, body, card_id NULL)
- [ ] Midnight job computes: biggest gainer, biggest crash, most traded, new cards, richest player, longest streak
- [ ] Template-based headlines first ("GOLDEN-MONKEY SOARS 42%!")
- [ ] Stretch: one LLM call per day to rewrite headlines in a fun style (fallback to templates on failure)
- [ ] `/news` page styled as a front page; link from home

### 4.3 Real authentication
- [ ] GitHub + Google OAuth (one account per OAuth identity)
- [ ] Disable `/dev/login` when `APP_ENV=prod` (add a test for this)
- [ ] CSRF protection for state-changing requests; secure cookies in prod
- [ ] Username picker on first login (unique, filtered)

### 4.4 Hardening
- [ ] Structured logging (`slog`) with request IDs
- [ ] Graceful shutdown for API, hub and scheduler
- [ ] Basic metrics endpoint: trades/sec, WebSocket clients, money supply
- [ ] **Economy dashboard** page (admin only): money supply over time, minted vs burned, Gini, reserve health
- [ ] Load test with `k6` or the sim tool: record real numbers (trades/sec, p95 latency, concurrent WS clients) **for the README and resume**
- [ ] Postgres backups (daily `pg_dump` cron on the server)

### 4.5 Deployment
- [ ] Production `docker-compose.prod.yml` (API, scheduler, Postgres, Redis, Caddy/Nginx for HTTPS)
- [ ] Deploy to a small VPS (or Fly.io / Railway for API + managed Postgres/Redis), frontend on Vercel or in the same compose
- [ ] Domain + HTTPS; WebSocket works through the proxy
- [ ] Seed production cards; set economy constants from the Phase 3 sim results

### 4.6 README and demo
- [ ] README: what it is, GIF of two browsers trading with live charts, architecture diagram, how to run locally (`make up`)
- [ ] "How money stays correct" section: ledger, lock order, idempotency, reconciliation
- [ ] "Economy design" section: faucets vs sinks, sim results table
- [ ] Load test numbers
- [ ] 60-second screen recording
- [ ] Invite friends; collect feedback; fix top 3 issues

### ✅ Phase 4 exit criteria
- [ ] Public URL with OAuth login works; friends can play together
- [ ] Prices move on their own (drift + events) and the newspaper appears each day
- [ ] Nightly reconciliation has been clean for 3+ consecutive days with real players
- [ ] README has GIF, architecture, real numbers; resume bullets filled in

---

## Stretch goals (after launch)
- [ ] Limit orders + simple order book alongside the curve
- [ ] Seasons: monthly reset + hall of fame
- [ ] Achievements (first trade, 7-day streak, created a card that hit 10 holders…)
- [ ] Mobile-friendly PWA
- [ ] Card categories + filters/search on the market page

---

## Testing checklist (use throughout)

| What | How | When |
|------|-----|------|
| Curve maths | Unit + property tests (random cards, random splits) | Phase 1 |
| Concurrency / money safety | Goroutine race, double-spend, idempotency, deadlock tests | Phase 1, re-run every phase |
| Ledger integrity | `Reconcile()` after every test + nightly job | Always |
| Real-time | 2+ browsers / incognito windows with dev login; 200 fake WS clients | Phase 2 |
| Economy | Bot simulation with exploit strategies | Phase 3 |
| Load | k6 / sim tool; record trades/sec + p95 | Phase 4 |
| Real players | Friends on the deployed build | Phase 4 |

---

## Interview talking points to prepare as you build
- Why balances are **derived** from a double-entry ledger, and how reconciliation proves nothing leaked
- `SELECT … FOR UPDATE` with fixed lock order vs `SERIALIZABLE` + retries: trade-offs
- Idempotency keys + unique constraints for safe retries
- Bonding curve vs order book
- Faucets vs sinks; how the bot sim was used to tune the economy
- Redis ZSET leaderboard (O(log n)), rebuildable from Postgres
- Publishing to Redis **after** commit; why Redis is never the source of truth for money

## Resume bullets (fill in real numbers at the end)
- Built a real-time multiplayer trading game (Go, Postgres, Redis, Next.js) with bonding-curve pricing and a double-entry ledger handling **N** trades/sec with zero balance drift under concurrent load tests.
- Designed anti-exploit economy rules validated by a bot simulation of **N** trades, and streamed live prices over WebSockets to **N** concurrent players.
