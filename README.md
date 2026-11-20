# Coin Trading Company

A multiplayer market game: trade cards whose prices move with real demand, mint your own, and climb the leaderboard, on top of a double-entry ledger that never loses a coin.

> 🚧 Work in progress: currently building **Phase 1 (money core)**. See [plan.md](plan.md) for the full build plan and [docs/spec.pdf](docs/spec.pdf) for the original spec.

## Quick start

Requirements: Docker, Go 1.27+.

```bash
# start postgres + redis
docker compose up -d postgres redis

# run the API (http://localhost:8081)
cd backend && go run ./cmd/api
```

Windows (PowerShell): `.\scripts\dev.ps1 db` then `.\scripts\dev.ps1 run`.

Check it's alive: `curl http://localhost:8081/healthz`
