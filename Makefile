# Linux/macOS/CI helper. On Windows use: .\scripts\dev.ps1 <command>
TEST_DATABASE_URL ?= postgres://coin:coin@localhost:55432/postgres?sslmode=disable
export TEST_DATABASE_URL

.PHONY: up down db run test test-race vet fmt seed

up:          ## start the full stack (postgres, redis, api)
	docker compose up -d --build

db:          ## start only postgres + redis (run the API yourself with `make run`)
	docker compose up -d postgres redis

down:
	docker compose down

run:         ## run the API locally (migrates + seeds on start)
	cd backend && go run ./cmd/api

test:
	cd backend && go test -count=1 ./...

test-race:
	cd backend && go test -count=1 -race ./...

vet:
	cd backend && go vet ./...

fmt:
	cd backend && gofmt -w .
