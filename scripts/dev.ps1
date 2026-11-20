# Windows helper (PowerShell). Usage: .\scripts\dev.ps1 <command>
#   up     start postgres, redis and the api in Docker
#   db     start only postgres + redis
#   down   stop everything
#   run    run the API locally with `go run`
#   test   run all Go tests (needs `db` running)
#   vet    run go vet
param([Parameter(Position = 0)][string]$Command = "help")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$backend = Join-Path $root "backend"
if (-not $env:TEST_DATABASE_URL) {
    $env:TEST_DATABASE_URL = "postgres://coin:coin@localhost:55432/postgres?sslmode=disable"
}

switch ($Command) {
    "up"   { docker compose -f "$root\docker-compose.yml" up -d --build }
    "db"   { docker compose -f "$root\docker-compose.yml" up -d postgres redis }
    "down" { docker compose -f "$root\docker-compose.yml" down }
    "run"  { Push-Location $backend; try { go run ./cmd/api } finally { Pop-Location } }
    "test" { Push-Location $backend; try { go test -count=1 ./... } finally { Pop-Location } }
    "vet"  { Push-Location $backend; try { go vet ./... } finally { Pop-Location } }
    default { Get-Content $PSCommandPath -TotalCount 8 | Select-Object -Skip 1 }
}
