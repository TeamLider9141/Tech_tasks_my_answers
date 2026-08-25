# AI usage

## 1. Which AI tools did you use?

Claude Code.

## 2. What tasks did you ask them to perform?

- Discussing the storage design for availability (stored `reserved` counter
  vs computed aggregate) and the locking strategy before writing code.
- Generating the initial implementation: schema/migration, store layer with
  the reservation transactions, HTTP handlers, and the integration test
  suite.
- Drafting the documentation (README, REVIEW.md) which I then reviewed and
  edited.
- Environment plumbing: docker-compose file, Makefile, port-conflict fixes.

## 3. What is one AI suggestion you rejected, and why?

The first design direction kept a `reserved` counter column on the `stock`
table, updated on every create/cancel/confirm/expire. I rejected it in favor
of computing reserved quantity from active reservations: the counter must be
mutated in perfect lockstep on four different code paths and silently drifts
if any of them misses (especially lazy expiry, which would need a stock
write exactly when nothing else touches the row). The computed form makes
expiry a pure read-side filter — no write needed for correctness.

## 4. What generated code did you substantially change or simplify?

- The generated handler layer initially contained a broken shared-helper
  signature for the get/confirm/cancel endpoints; it was rewritten into a
  single `reservationOp` helper taking a `func(context.Context, string)`
  operation.
- Timestamps originally serialized in the server's local timezone
  (`+05:00`). I caught this in a manual `curl` walkthrough against the
  running service and added explicit UTC normalization at the store
  boundary, plus a regression assertion in the tests.
- Default ports and connection URLs were adjusted twice because generated
  defaults collided with services already running on my machine (5432,
  then 5433) — the compose file now maps PostgreSQL to 5434.

## 5. How did you verify that generated code was correct?

- The integration test suite runs every required behaviour against a real
  PostgreSQL instance, including a 10-goroutine concurrency test that fails
  if even one unit of stock is oversold, and it is run with `-race`.
- `go vet` on the whole module.
- A manual end-to-end `curl` session against the running server covering
  the happy path, idempotent replay (201 → 200 with the same reservation
  id), confirm with stock deduction, oversell rejection with the shortage
  payload, and 404 mapping — this is what surfaced the UTC bug above.
- I read the transaction code path by path and checked the locking
  argument by hand: every writer that affects availability takes the stock
  row locks in sorted product order, so competing reservations serialize
  and multi-item reservations cannot deadlock.
