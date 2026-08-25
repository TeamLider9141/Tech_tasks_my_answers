# Inventory Reservation Service

Small HTTP service that reserves products while a customer is going through
checkout. Written in Go with PostgreSQL. No framework: only the standard
library `net/http` router (Go 1.22+ patterns) and the `pgx` driver.

## Requirements

- Go 1.24+
- Docker + Docker Compose (for PostgreSQL), or any PostgreSQL 14+ instance

## How to run

```bash
# 1. Start PostgreSQL (on port 5434, so it won't conflict with a local
#    postgres already running on 5432)
make db-up

# 2. Run the service (applies migrations automatically at startup)
make run
```

The server listens on `:8080`. Configuration comes from env variables:

| Variable       | Default                                                            |
|----------------|--------------------------------------------------------------------|
| `DATABASE_URL` | `postgres://inventory:inventory@localhost:5434/inventory?sslmode=disable` |
| `ADDR`         | `:8080`                                                            |

Migrations are plain SQL files in `migrations/`, embedded into the binary.
A small runner applies them at startup and records them in the
`schema_migrations` table. It takes an advisory lock first, so two instances
starting at the same time won't apply the same migration twice. No external
migration tool is needed.

## Tests

Tests run against a real PostgreSQL database (`inventory_test`, created
automatically by the compose init script):

```bash
make db-up   # once
make test
```

You can set `TEST_DATABASE_URL` to point somewhere else. The tests truncate
all tables before every test, so don't point it at a database with real data.

I went with integration tests at the HTTP level instead of unit tests with
mocks. The risky logic here is SQL and transaction boundaries, and mocks
can't catch mistakes there. The suite covers everything the assignment asks
for: multi-item reservation, all-or-nothing failure, confirm, cancel and
release, rejecting confirm on an expired reservation, idempotent replay,
concurrent requests competing for the last stock, and validation errors.

## API

All request and response bodies are JSON. Timestamps are UTC (RFC 3339).

All errors have the same shape:

```json
{"error": {"code": "insufficient_stock", "message": "...", "shortages": [...]}}
```

| Code                       | HTTP | Meaning                                      |
|----------------------------|------|----------------------------------------------|
| `invalid_request`          | 400  | Bad JSON or missing/wrong fields             |
| `invalid_quantity`         | 400  | Quantity is not a positive integer           |
| `not_found`                | 404  | Warehouse, product or reservation not found  |
| `already_exists`           | 409  | Product/warehouse name already taken         |
| `insufficient_stock`       | 409  | Not enough stock; `shortages` has details    |
| `invalid_state`            | 409  | Action not allowed in the current status     |
| `reservation_expired`      | 409  | Confirm attempted on an expired reservation  |
| `idempotency_key_conflict` | 409  | Same key reused with a different body        |
| `internal_error`           | 500  | Unexpected error                             |

### Products and warehouses

The assignment doesn't ask for these endpoints, but without them you can't
try the service end to end without writing SQL by hand, so I added minimal
ones:

```bash
curl -X POST localhost:8080/products   -d '{"name":"laptop"}'
curl -X POST localhost:8080/warehouses -d '{"name":"tashkent-main"}'
# → 201 {"id":1,"name":"laptop"}
```

### Stock

```bash
# Add physical stock (positive quantities only)
curl -X POST localhost:8080/warehouses/1/stock \
  -d '{"product_id":1,"quantity":10}'
# → 200 {"warehouse_id":1,"product_id":1,"physical":10,"reserved":0,"available":10}

# Read stock levels
curl localhost:8080/warehouses/1/products/1/stock
# → 200 {"warehouse_id":1,"product_id":1,"physical":10,"reserved":4,"available":6}
```

`available = physical - reserved`, where reserved is the total quantity held
by active reservations that haven't expired yet.

### Reservations

Creating a reservation requires an `Idempotency-Key` header (1–200 chars,
chosen by the client, for example a UUID per checkout attempt):

```bash
curl -X POST localhost:8080/reservations \
  -H 'Idempotency-Key: checkout-42' \
  -d '{"warehouse_id":1,"items":[{"product_id":1,"quantity":4}]}'
```

- First request: `201` with the new reservation.
- Same key with the same body again: `200` with the original reservation,
  nothing new is created.
- Same key with a different body: `409 idempotency_key_conflict`.

A reservation is all-or-nothing: if any product doesn't have enough stock,
nothing is reserved and the response is `insufficient_stock` with a
`shortages` list showing what was missing.

```json
{
  "id": "17623ff8-f2c5-4484-829e-3ff5f30c13a5",
  "warehouse_id": 1,
  "status": "active",
  "items": [{"product_id": 1, "quantity": 4}],
  "created_at": "2026-08-25T12:35:33.915846Z",
  "expires_at": "2026-08-25T12:50:33.915846Z"
}
```

```bash
curl localhost:8080/reservations/{id}            # → 200 current state
curl -X POST localhost:8080/reservations/{id}/confirm
curl -X POST localhost:8080/reservations/{id}/cancel
```

### Reservation lifecycle

```
            ┌──────────► confirmed   (terminal; physical stock deducted)
            │ confirm
  active ───┤ cancel
            ├──────────► cancelled   (terminal; hold released)
            │ 15 min pass (database clock)
            └──────────► expired     (terminal; hold released)
```

- `confirm` on an already confirmed reservation (or `cancel` on a cancelled
  one) returns `200` and changes nothing, so client retries are safe.
- Any other wrong transition (for example confirming a cancelled
  reservation) returns `409` with the current status in the body.
- `confirm` subtracts the physical stock in the same transaction that
  finalizes the reservation.

## Database design

Tables (see `migrations/0001_init.sql`):

- `products` — id, unique name
- `warehouses` — id, unique name
- `stock` — one row per (warehouse, product): physical quantity,
  `CHECK (quantity >= 0)`
- `reservations` — uuid id, warehouse_id, status
  (`active | confirmed | cancelled | expired`), unique `idempotency_key`,
  `request_hash`, `created_at`, `expires_at`, `finalized_at`
- `reservation_items` — (reservation_id, product_id, quantity),
  `CHECK (quantity > 0)`

### Why availability is computed, not stored

`stock.quantity` holds only physical stock. Reserved quantity is a `SUM`
over the items of active reservations that haven't expired. I also
considered a `reserved` counter column on `stock` — reads would be faster —
but that counter has to be updated correctly in four places (create, cancel,
confirm, expire), and if any one of them misses, the number drifts and the
error stays forever. With the computed version, cancel is just a status
change, and expiry doesn't need any write at all to be correct.

### How oversell is prevented

Creating a reservation is one transaction:

1. `SELECT ... FOR UPDATE` on the stock rows of the requested products,
   always ordered by `product_id`. Same order everywhere means two
   multi-item reservations can't deadlock each other.
2. Compute the reserved sums and check `requested <= physical - reserved`
   for every item.
3. Insert the reservation and its items, or roll back everything.

Two concurrent requests for the same product line up on the row lock, so the
second one always sees the result of the first. The lock lives in
PostgreSQL, so it also works when several instances of the service run at
once — no in-process mutex, and normal `READ COMMITTED` is enough. The
`CHECK` constraints protect the same rules on the database level as a second
line of defense.

`confirm` locks the reservation row with `FOR UPDATE`, subtracts the stock
rows (same sorted order) and sets the status, all in one transaction.

### Idempotency

`reservations.idempotency_key` is `UNIQUE`. Next to it I store a SHA-256
hash of the meaningful part of the request (warehouse + sorted items), so a
retry with the same JSON in a different key order still counts as the same
request. Replay returns the stored reservation; the same key with a
different payload is rejected. If two requests race with the same fresh key,
the loser hits the unique index, then reads the winner's reservation and
returns that.

### Time and expiry

All timestamps are `timestamptz` written with the database `now()`, so
several app instances with different clocks can't disagree about expiry.
Expiry is lazy: the availability query just ignores active reservations
whose `expires_at` is in the past, and endpoints that touch such a
reservation update its status to `expired`. No background worker is needed
for correctness.

## Assumptions

- One reservation belongs to one warehouse (the assignment says "products
  from one warehouse").
- `confirm` means the sale is final: physical stock is subtracted. If it
  should instead hand off to a separate fulfillment step, only the confirm
  branch changes.
- Stock can only be added through the API; it only goes down through
  confirmed reservations. Corrections and write-offs are out of scope.
- Product and warehouse management is out of scope; the minimal create
  endpoints exist only to make the service testable end to end.
- An expired reservation is terminal: it can't be confirmed (required by
  the task) and also can't be cancelled (my choice — the 409 tells the
  client the real state instead of a misleading success).
- Quantities are integers, max 10^9 per request, to stay far away from
  bigint overflow.

## Tradeoffs

- **Computed availability**: every stock read does an aggregate over active
  reservations. At this scale it's a cheap indexed query. A very
  high-traffic system would probably keep a counter and reconcile it
  periodically.
- **`FOR UPDATE` row locks instead of `SERIALIZABLE`**: the locking point
  is explicit and visible, and clients don't need retry loops for
  serialization failures.
- **Integration tests instead of mocked unit tests**: fewer tests, but they
  test the real behavior against a real database.
- **Request hash from parsed fields** (warehouse + sorted items), not from
  the raw body, so formatting differences in a retry don't break
  idempotency.

## Known limitations

- Idempotency keys are global and live forever. Two different clients
  picking the same key would collide. With auth, keys should be scoped per
  client and cleaned up after some retention period.
- Idempotency only covers creation. A failed create (insufficient stock) is
  not recorded, so a retry runs the check again — still correct, but the
  client may see a different error than the first time.
- Because expiry is lazy, `status` in the database can still say `active`
  for an already-expired reservation until something touches it. The
  queries in the service account for this, but a manual report over the raw
  tables has to repeat the `expires_at` filter.
- No pagination or listing endpoints, no metrics, no structured request
  logging, no rate limiting.
- All reservations for the same product in a warehouse serialize on one row
  lock, so a very hot product becomes a bottleneck.

## What I would improve with more time

- A small background job that marks expired reservations in batches, so
  reports over raw tables get cleaner (correctness doesn't change).
- Per-client idempotency keys with a TTL and cleanup.
- Request logging middleware, request IDs, Prometheus metrics.
- CI with GitHub Actions (PostgreSQL service container + `go test -race`).
- A stress test with random create/confirm/cancel/expire interleavings,
  checking the availability invariant after every round.
- Pagination for reservations and stock listings.
