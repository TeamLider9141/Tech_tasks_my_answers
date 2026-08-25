GO ?= go
DATABASE_URL ?= postgres://inventory:inventory@localhost:5434/inventory?sslmode=disable
TEST_DATABASE_URL ?= postgres://inventory:inventory@localhost:5434/inventory_test?sslmode=disable

.PHONY: db-up db-down run test vet

db-up:
	docker compose up -d --wait

db-down:
	docker compose down -v

run:
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/server

test:
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) $(GO) test ./... -count=1

vet:
	$(GO) vet ./...
