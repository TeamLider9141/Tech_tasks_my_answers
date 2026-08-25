# Inventory Reservation Service

A small HTTP service for reserving products while a customer completes
checkout. Go + PostgreSQL, no framework: the standard library `net/http`
router (Go 1.22+ patterns) and the `pgx` driver are the only meaningful
dependencies.

## Requirements

- Go 1.24+
- Docker + Docker Compose (for PostgreSQL), or any PostgreSQL 14+ instance

## Setup and run

```bash
# 1. Start PostgreSQL (listens on localhost:5434 to avoid clashing with a
#    locally installed PostgreSQL on 5432)
make db-up

# 2. Run the service (applies migrations automatically at startup)
make run
```

The server listens on `:8080` by default. Configuration is environment-based:

| Variable       | Default                                                            |
|----------------|--------------------------------------------------------------------|
| `DATABASE_URL` | `postgres://inventory:inventory@localhost:5434/inventory?sslmode=disable` |
| `ADDR`         | `:8080`                                                            |

Migrations are plain SQL files in `migrations/`, embedded into the binary and
applied at startup by a small runner (tracked in `schema_migrations`, guarded
by an advisory lock so multiple instances can boot concurrently). No external
migration tool is required.

## Tests

Tests run against a **real PostgreSQL database** (`inventory_test`, created
automatically by the compose init script):

```bash
make db-up   # once
make test
```

To point tests elsewhere set `TEST_DATABASE_URL`. The suite truncates all
tables before each test, so never point it at a database you care about.

The tests are integration tests at the HTTP handler level: they exercise
routing, validation, transaction logic and error mapping in one pass, and
they cover every behaviour required by the assignment (atomic multi-item
reservation, all-or-nothing failure, confirm, cancel + release, expired
confirm rejection, idempotent replay, concurrent competition for insufficient
stock, invalid input and invalid transitions).

## API

All request and response bodies are JSON. Timestamps are UTC (RFC 3339).

Errors share one shape:

```json
{"error": {"code": "insufficient_stock", "message": "...", "shortages": [...]}}
```

| Code                       | HTTP | Meaning                                        |
|----------------------------|------|------------------------------------------------|
| `invalid_request`          | 400  | Malformed JSON, missing/out-of-range fields    |
| `invalid_quantity`         | 400  | Quantity not a positive integer                |
| `not_found`                | 404  | Unknown warehouse / product / reservation      |
| `already_exists`           | 409  | Product/warehouse name taken                   |
| `insufficient_stock`       | 409  | Reservation rejected; `shortages` lists detail |
| `invalid_state`            | 409  | Transition not allowed from current status     |
| `reservation_expired`      | 409  | Confirm attempted on an expired reservation    |
| `idempotency_key_conflict` | 409  | Key reused with a different payload            |
| `internal_error`           | 500  | Unexpected failure                             |

### Convenience endpoints

Not part of the required surface, but without them the service cannot be
exercised end-to-end without hand-written SQL:

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

`available = physical − quantity held by active, non-expired reservations`.

### Reservations

Creation requires an **`Idempotency-Key` header** (1–200 chars, chosen by the
client, e.g. a UUID per checkout attempt):

```bash
curl -X POST localhost:8080/reservations \
  -H 'Idempotency-Key: checkout-42' \
  -d '{"warehouse_id":1,"items":[{"product_id":1,"quantity":4}]}'
```

`201` with the reservation on first creation; `200` with the **original**
reservation when the same key is replayed with the same payload; `409` when
the key is reused with a different payload. A reservation reserves every
item or nothing: if any product lacks stock the whole request fails with
`insufficient_stock` and a `shortages` list.

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

- Repeating `confirm` on a confirmed reservation (or `cancel` on a cancelled
  one) returns `200` and changes nothing — safe client retries.
- Any other transition (confirm a cancelled/expired reservation, cancel a
  confirmed/expired one) returns `409` with the current status in the body,
  so a client that lost track of state learns the truth from the error.
- Confirming deducts physical stock in the same transaction that finalizes
  the reservation, so availability is continuous: before confirm the goods
  are held by the reservation, after confirm they are gone from physical.

## Database and transaction design

Schema (`migrations/0001_init.sql`):

```mermaid
erDiagram
    warehouses ||--o{ stock : holds
    products ||--o{ stock : "stocked as"
    warehouses ||--o{ reservations : receives
    reservations ||--o{ reservation_items : contains
    products ||--o{ reservation_items : reserves

    products {
        bigint id PK
        text name UK
        timestamptz created_at
    }
    warehouses {
        bigint id PK
        text name UK
        timestamptz created_at
    }
    stock {
        bigint warehouse_id PK, FK
        bigint product_id PK, FK
        bigint quantity "CHECK quantity >= 0"
        timestamptz updated_at
    }
    reservations {
        uuid id PK
        bigint warehouse_id FK
        reservation_status status "active | confirmed | cancelled | expired"
        text idempotency_key UK
        text request_hash
        timestamptz created_at
        timestamptz expires_at
        timestamptz finalized_at "null until confirmed/cancelled"
    }
    reservation_items {
        uuid reservation_id PK, FK
        bigint product_id PK, FK
        bigint quantity "CHECK quantity > 0"
    }
```

**Availability is computed, not stored.** `stock.quantity` holds physical
stock only; reserved quantity is `SUM(items of active reservations whose
expires_at > now())`. The alternative — a `reserved` counter on the stock
row — is faster to read but must be mutated in perfect lockstep on create,
cancel, confirm *and* expiry, and drifts permanently if any path misses.
With the computed form, cancel is a status flip and expiry needs **no write
at all** to be correct.

**No oversell under concurrency.** Reservation creation runs in one
transaction that:

1. locks the stock rows of the requested products with
   `SELECT … FOR UPDATE` in `product_id` order (deterministic order →
   no deadlocks between multi-item reservations);
2. computes reserved sums and checks `requested ≤ physical − reserved`
   for every item;
3. inserts the reservation and its items, or aborts entirely.

Competing requests for the same product serialize on the row lock —
across goroutines and across service instances, because the lock lives in
PostgreSQL. Plain `READ COMMITTED` suffices; there is no in-process mutex.
`CHECK` constraints (`quantity >= 0`, item `quantity > 0`) back the
application logic at the database layer.

**Confirm** locks the reservation row `FOR UPDATE`, then decrements the
stock rows (same sorted order) and flips the status in one transaction.
A `CHECK` violation here is impossible by invariant: every active
reservation was admitted against `physical − reserved ≥ 0`.

**Idempotent creation.** `reservations.idempotency_key` is `UNIQUE`; a
SHA-256 hash of the semantic payload (warehouse + sorted items) is stored
next to it. Replay returns the stored reservation; a different payload under
the same key is rejected. If two requests with the same fresh key race, the
loser hits the unique index, re-reads the winner's row and returns it — the
database, not the application, arbitrates.

**Time.** All timestamps are `timestamptz` written with `now()` — the
database clock is the single source of truth, so multiple app instances with
skewed clocks cannot disagree about expiry. Expiration is **lazy**: the
availability query simply ignores active reservations past `expires_at`, and
endpoints that touch a reservation materialize `active → expired` when the
deadline has passed. No background worker is needed for correctness.

## Assumptions

- A reservation is scoped to **one warehouse** (per the assignment's
  "products from one warehouse").
- Confirming a reservation **deducts physical stock** — the sale is final
  and goods leave the sellable pool. If confirm should instead hand off to a
  separate fulfilment step, only the confirm branch changes.
- Stock can only be **added**; decrements happen exclusively through
  confirmed reservations. Corrections/write-offs are out of scope.
- Product and warehouse management is out of scope; minimal create
  endpoints exist purely to make the service testable end-to-end.
- An expired reservation is terminal: it can be neither confirmed (required)
  nor cancelled (design choice — the client learns the real state from the
  409 instead of a misleading success).
- Quantities are integers, capped at 10⁹ per request to stay far from
  `bigint` overflow.

## Important tradeoffs

- **Computed availability** trades read-time work (an aggregate over active
  reservations per stock read) for write-time simplicity and immunity to
  counter drift. At this scale the aggregate is a cheap indexed lookup; a
  high-traffic system might maintain a counter and reconcile.
- **Row locks over `SERIALIZABLE`**: explicit `FOR UPDATE` locks make the
  serialization point visible and avoid retry loops that `SERIALIZABLE`
  failures would force on every client path.
- **Integration tests over unit tests with mocks**: the risky logic lives in
  SQL and transaction boundaries, which mocks cannot exercise. Fewer,
  behaviour-level tests against real PostgreSQL cover more of what can
  actually break.
- **Handler-computed request hash** (semantic: warehouse + sorted items)
  rather than raw-body hash, so a retried request with different JSON key
  order or whitespace still counts as the same request.

## Known limitations

- Idempotency keys live forever (one row each) and are global, not
  per-client — two clients choosing the same key would collide. Namespacing
  by authenticated client would fix this once auth exists; keys should also
  be purged after a retention window.
- Idempotency covers reservation **creation** only. Confirm/cancel are
  naturally idempotent through the state machine, but a *failed* create
  (e.g. insufficient stock) is not recorded, so a retry re-executes the
  check — correct, but the client may see a different error than the
  original attempt.
- Expired reservations are materialized lazily, so `status` in the database
  may read `active` for a reservation that is already dead until something
  touches it. All correctness-relevant queries account for this; an
  operational report over raw tables must repeat the `expires_at` filter.
- No pagination/listing endpoints, no metrics, no structured request
  logging, no rate limiting.
- The stock lock serializes all reservations touching the same product in a
  warehouse; a very hot product becomes a contention point.

## What I would improve with more time

- A tiny background sweeper to materialize expired reservations in batches,
  keeping reporting queries honest without changing correctness.
- Per-client idempotency scope + key TTL/cleanup.
- Structured request logging middleware, request IDs, and Prometheus
  metrics (reservation outcomes, lock wait times).
- CI (GitHub Actions: compose PostgreSQL service + `go test -race`).
- Property-style concurrency tests (random interleavings of
  create/confirm/cancel/expire under load, asserting the availability
  invariant after every round).
- API pagination for reservations and stock listings.
