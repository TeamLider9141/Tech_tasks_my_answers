# AI usage

## 1. Which AI tools did you use?

Claude Code.

## 2. What tasks did you ask them to perform?

- Talking through the storage design before writing code: store a `reserved`
  counter or compute the reserved quantity from reservations, and which
  locking approach to use.
- Generating the first version of the code: the migration, the store layer
  with the transactions, the HTTP handlers, and the integration tests.
- First drafts of the documentation (README, REVIEW.md), which I then
  reviewed and edited.
- Small environment things: the docker-compose file, the Makefile, fixing
  port conflicts.

## 3. What is one AI suggestion you rejected, and why?

The first design kept a `reserved` counter column on the `stock` table,
updated on every create/cancel/confirm/expire. I rejected it and compute the
reserved quantity from active reservations instead. The counter has to be
updated correctly in four different places, and if one of them misses, the
number drifts and stays wrong. Expiry is the worst case: with a counter it
needs a stock write exactly at a moment when nothing else touches the row.
With the computed version, expiry needs no write at all.

## 4. What generated code did you substantially change or simplify?

- The generated handlers had a broken shared helper for the
  get/confirm/cancel endpoints; I rewrote it into one `reservationOp` helper
  that takes the operation as a function.
- Timestamps were returned in the server's local timezone (`+05:00`). I
  noticed this while testing with curl against the running service, and
  added UTC normalization at the store boundary plus a test assertion for
  it.
- Ports and connection URLs changed twice, because the generated defaults
  conflicted with services already running on my machine (5432, then 5433).
  The compose file now maps PostgreSQL to 5434.

## 5. How did you verify that generated code was correct?

- The integration tests run every required behavior against a real
  PostgreSQL, including a test with 10 goroutines that fails if even one
  unit of stock is oversold. The suite is run with `-race`.
- `go vet` on the whole module.
- A manual curl session against the running server: happy path, idempotent
  replay (201 → 200 with the same reservation id), confirm with stock
  deduction, oversell rejection with the shortage list, and 404 mapping.
  This is how I found the UTC timezone bug above.
- I read the transaction code myself and checked the locking logic: every
  write that affects availability takes the stock row locks in sorted
  product order, so competing reservations line up and multi-item
  reservations can't deadlock.
