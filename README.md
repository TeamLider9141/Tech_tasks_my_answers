# Inventory Reservation Service

HTTP service for reserving products during checkout. Go + PostgreSQL. No
framework, only the standard `net/http` router and the `pgx` driver.

## Requirements

- Go 1.24+
- Docker + Docker Compose (for PostgreSQL)

## How to run

```bash
# starts PostgreSQL on port 5434 (5432 was busy on my machine)
make db-up

# runs the service, migrations are applied automatically
make run
```

The server runs on `:8080`. Config via env variables:

- `DATABASE_URL` — default `postgres://inventory:inventory@localhost:5434/inventory?sslmode=disable`
- `ADDR` — default `:8080`

Migrations are SQL files in `migrations/`. The app applies them at startup
and records them in the `schema_migrations` table (with a lock, so two
instances starting together don't clash).

## Tests

Tests need a real PostgreSQL (`inventory_test` database, created by the
compose setup automatically):

```bash
make db-up   # once
make test
```

The tests clean all tables before each test, so don't run them against a
database with real data. `TEST_DATABASE_URL` changes the connection.

I wrote integration tests through the HTTP API instead of unit tests with
mocks, because the main logic is in SQL and transactions and mocks can't
test that. Covered: reservation with several items, all-or-nothing fail,
confirm, cancel, expired reservation, idempotency replay, concurrent
requests for the last stock, validation errors.

## API

Everything is JSON. Timestamps are UTC (RFC 3339).

Error format:

```json
{"error": {"code": "insufficient_stock", "message": "...", "shortages": [...]}}
```

Error codes: `invalid_request` (400), `invalid_quantity` (400), `not_found`
(404), `already_exists` (409), `insufficient_stock` (409), `invalid_state`
(409), `reservation_expired` (409), `idempotency_key_conflict` (409),
`internal_error` (500).

### Products and warehouses

Not part of the task, but needed to try the service without writing SQL by
hand:

```bash
curl -X POST localhost:8080/products   -d '{"name":"laptop"}'
curl -X POST localhost:8080/warehouses -d '{"name":"tashkent-main"}'
# → 201 {"id":1,"name":"laptop"}
```

### Stock

```bash
# add stock
curl -X POST localhost:8080/warehouses/1/stock -d '{"product_id":1,"quantity":10}'

# check stock
curl localhost:8080/warehouses/1/products/1/stock
# → {"warehouse_id":1,"product_id":1,"physical":10,"reserved":4,"available":6}
```

`available = physical - reserved`, where reserved is the sum from active
reservations that haven't expired.

### Reservations

Creating one needs an `Idempotency-Key` header (the client picks it, for
example a UUID per checkout):

```bash
curl -X POST localhost:8080/reservations \
  -H 'Idempotency-Key: checkout-42' \
  -d '{"warehouse_id":1,"items":[{"product_id":1,"quantity":4}]}'
```

First time it returns `201`. The same key with the same body again returns
`200` with the old reservation. The same key with a different body returns
`409`.

The reservation is all-or-nothing: if any product doesn't have enough
stock, nothing is reserved and the response is `insufficient_stock` with a
`shortages` list.

```bash
curl localhost:8080/reservations/{id}
curl -X POST localhost:8080/reservations/{id}/confirm
curl -X POST localhost:8080/reservations/{id}/cancel
```

Statuses: `active` → `confirmed` (confirm; stock is deducted), `active` →
`cancelled` (cancel; the hold is released), `active` → `expired` (15
minutes pass; the hold is released). All three end states are final.

Repeating `confirm` on a confirmed reservation (or `cancel` on a cancelled
one) returns `200` and does nothing, so retries are safe. Other wrong
transitions return `409` with the current status.

## Database

Tables (`migrations/0001_init.sql`):

- `products` — id, name (unique)
- `warehouses` — id, name (unique)
- `stock` — warehouse_id + product_id, quantity (`CHECK >= 0`)
- `reservations` — uuid, warehouse_id, status, idempotency_key (unique),
  request_hash, created_at, expires_at
- `reservation_items` — reservation_id, product_id, quantity (`CHECK > 0`)

How the tables connect:

![Database ERD](<./Inventory Reservation Service — Database ERD.jpg>)

**Reserved quantity is calculated, not stored.** `stock.quantity` is only
physical stock; reserved is a SUM over active reservations. I considered a
`reserved` column first, but it has to be updated in 4 places (create,
cancel, confirm, expire) and if one is missed the number goes wrong and
stays wrong. With calculation, cancel is just a status update and expire
needs no update at all.

**Oversell protection.** Creating a reservation is one transaction: it
locks the stock rows with `SELECT ... FOR UPDATE` (always ordered by
`product_id`, so two multi-item reservations can't deadlock), checks
`requested <= physical - reserved` for each item, then inserts everything
or nothing. Parallel requests wait on the row lock, so the check always
sees fresh data. The lock is inside PostgreSQL, so it also works with
several instances of the service.

**Confirm** locks the reservation row, deducts the stock and updates the
status in one transaction.

**Idempotency.** `idempotency_key` is unique in the database. I also store
a SHA-256 hash of warehouse + sorted items, so the same request with
different JSON formatting still counts as the same. If two requests with
the same new key run at the same time, one hits the unique index and simply
returns the other one's reservation.

**Time.** Everything uses the database `now()`, so servers with different
clocks can't disagree about expiry. Expiry is lazy: queries just skip
reservations where `expires_at` has passed, and the status is updated to
`expired` when something touches the reservation. No background job is
needed.

## Assumptions

- One reservation = one warehouse.
- Confirm deducts physical stock (the sale is final).
- Stock can only be added; it only goes down through confirm.
- Product/warehouse management is out of scope, only minimal create
  endpoints for testing.
- An expired reservation can't be confirmed or cancelled.
- Quantity is max 10^9 per request.

## Tradeoffs and limitations

- Calculating reserved on every read is slower than a stored counter, but
  it can't drift. At this size it's fine.
- Row locks instead of `SERIALIZABLE` — simpler, and clients don't need
  retry loops.
- Idempotency keys are global and never deleted. They should be per-client
  with a TTL once auth exists.
- The `expired` status updates lazily, so queries directly on the tables
  must also check `expires_at`.
- No pagination, no metrics, no logging middleware, no rate limiting.
- A very popular product serializes on one row lock.

## What I would add with more time

- A background job to mark expired reservations.
- Per-client idempotency keys with a TTL.
- Request logging, metrics, CI.
- A load test for the no-oversell invariant.
- Pagination.
