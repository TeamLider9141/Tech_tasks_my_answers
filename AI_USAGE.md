# AI usage

## 1. Which AI tools did you use?

Claude Code.

## 2. What tasks did you ask them to perform?

- Discussing the design before coding: store a `reserved` counter or
  calculate it, and which locking approach to use.
- Generating the first version of the code: migration, store layer,
  handlers, tests.
- First drafts of README and REVIEW.md, which I reviewed and edited after.
- docker-compose, Makefile, fixing port conflicts.

## 3. What is one AI suggestion you rejected, and why?

The first design had a `reserved` counter column in the `stock` table,
updated on every create/cancel/confirm/expire. I rejected it because the
counter must be updated in four different places, and if one is missed the
number goes wrong and stays wrong. Calculating reserved from active
reservations is safer — expire then needs no update at all.

## 4. What generated code did you substantially change or simplify?

- The generated handlers had a broken shared helper for the
  get/confirm/cancel endpoints. I rewrote it as one `reservationOp` helper
  that takes the operation as a function.
- Timestamps came back in local time (`+05:00`). I found it while testing
  with curl and added UTC conversion in the store layer plus a test for it.
- Ports changed twice because 5432 and 5433 were busy on my machine; the
  compose file now uses 5434.

## 5. How did you verify that generated code was correct?

- Integration tests on a real PostgreSQL, including a test with 10
  goroutines that fails if even one unit is oversold. Run with `-race`.
- `go vet` on the module.
- Manual curl testing: create, replay with the same key (201 → 200, same
  id), confirm with stock deduction, oversell error, 404s. That's how I
  found the timezone bug.
- I read the transaction code myself and checked that every write takes
  the stock locks in the same sorted order.
