# Code review: `Reserve`

```go
func Reserve(ctx context.Context, db *sql.DB, productID int64, qty int) error {
	var available int

	err := db.QueryRowContext(
		ctx,
		`SELECT quantity FROM stock WHERE product_id = $1`,
		productID,
	).Scan(&available)
	if err != nil {
		return err
	}

	if available < qty {
		return errors.New("not enough stock")
	}

	_, err = db.ExecContext(
		ctx,
		`UPDATE stock SET quantity = quantity - $1 WHERE product_id = $2`,
		qty,
		productID,
	)
	return err
}
```

## 1. What can go wrong

**R1 — Check-then-act race (oversell).** The `SELECT` and the `UPDATE` are
two independent auto-committed statements, possibly on two different pool
connections. Between them, any number of concurrent `Reserve` calls can read
the same `quantity`. With `quantity = 1`, two concurrent calls both see `1`,
both pass `available < qty`, both decrement → final quantity `-1`. The
product is oversold and the stored value is negative. This is the defining
bug of the function.

**R2 — Nothing prevents negative stock even outside the race.** The `UPDATE`
decrements unconditionally — no `WHERE quantity >= $1`, and (as far as this
snippet shows) no `CHECK (quantity >= 0)` constraint as a last line of
defense.

**R3 — `qty` is not validated.** `qty = 0` "succeeds" doing nothing
meaningful; `qty < 0` *increases* stock through the subtraction — a caller
bug silently corrupts inventory.

**R4 — A retry double-reserves.** There is no idempotency: a client that
timed out and retries decrements stock twice for one business action.

**R5 — Unknown product is indistinguishable from infrastructure failure.**
A missing row surfaces as `sql.ErrNoRows` from `Scan`, returned raw. Callers
cannot tell "no such product" (client error, 404) from "database is down"
(500). Similarly `errors.New("not enough stock")` is an untyped, sentinel-less
string — impossible to map to a proper API response without string matching.

**R6 — The `UPDATE` result is not checked.** If the row disappears between
the two statements (or the `WHERE` guard from the fix is added), the call
reports success while affecting zero rows. `RowsAffected` must be checked.

**R7 — Nothing records *what* was reserved.** Stock is decremented but no
reservation row exists — nothing to confirm, cancel, expire, or audit. If
the caller crashes after `Reserve`, the stock is leaked forever. (Whether
this is a bug or the intended scope is a product question — see §5.)

## 2. Business rule vs implementation

| # | Concern | Classification |
|---|---------|----------------|
| R1 | Oversell under concurrency | **Business rule** violated, caused by an **implementation** flaw (no atomicity) |
| R2 | Stock must never go negative | **Business rule** (enforce in DB as well) |
| R3 | Quantity must be positive | **Business rule** (input validation) |
| R4 | Retry must not double-reserve | **Business rule** (idempotency), needs **implementation** support |
| R5 | Distinguishable, typed errors | **Implementation** |
| R6 | Unchecked `RowsAffected` | **Implementation** |
| R7 | No reservation entity / release path | **Business** scope question first, then implementation |

## 3. How I would correct it

Collapse check and decrement into one atomic, guarded statement — the
database becomes the arbiter and the race disappears:

```go
var (
	ErrInvalidQuantity   = errors.New("quantity must be positive")
	ErrProductNotStocked = errors.New("product has no stock record")
	ErrInsufficientStock = errors.New("insufficient stock")
)

func Reserve(ctx context.Context, db *sql.DB, productID int64, qty int) error {
	if qty <= 0 {
		return ErrInvalidQuantity
	}

	res, err := db.ExecContext(ctx, `
		UPDATE stock
		SET quantity = quantity - $1
		WHERE product_id = $2 AND quantity >= $1`,
		qty, productID)
	if err != nil {
		return fmt.Errorf("reserve product %d: %w", productID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either the product has no stock row or not enough quantity;
		// one cheap follow-up read tells the caller which.
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM stock WHERE product_id = $1)`,
			productID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrProductNotStocked
		}
		return ErrInsufficientStock
	}
	return nil
}
```

Plus a schema backstop, so no future code path can break the invariant:

```sql
ALTER TABLE stock ADD CONSTRAINT stock_quantity_nonnegative CHECK (quantity >= 0);
```

If the operation must grow (reserve several products, insert a reservation
record, set an expiry — R7), the single statement is no longer enough;
then: one transaction, `SELECT … FOR UPDATE` on the stock rows in a
deterministic order, check, insert reservation, commit — exactly the design
used by the service in this repository, which also addresses R4 with a
unique idempotency key supplied by the caller.

## 4. Tests that prove the correction

1. **Concurrency (the decisive one, against real PostgreSQL):** stock 3,
   spawn 10 goroutines each calling `Reserve(…, 1)`. Assert exactly 3
   succeed, 7 return `ErrInsufficientStock`, and the final quantity is
   exactly 0 — never negative. Before the fix this test fails
   nondeterministically but reliably under `-count=`/`-race` runs; after
   the fix it cannot fail.
2. **Boundary:** stock 5, reserve 5 → success, quantity 0; next reserve 1 →
   `ErrInsufficientStock`.
3. **Validation:** `qty = 0` and `qty = -3` → `ErrInvalidQuantity`, quantity
   unchanged (negative qty must not *increase* stock).
4. **Unknown product:** → `ErrProductNotStocked`, not a raw scan error.
5. **Constraint backstop:** direct SQL `UPDATE stock SET quantity = -1`
   must be rejected by the `CHECK` constraint.
6. **If idempotency is added (R4):** same key twice → one decrement; two
   different keys → two decrements.

## 5. Assumptions to confirm with a product owner before implementing

- **Is `Reserve` meant to be a permanent decrement or a temporary hold?**
  If checkout can be abandoned, we need a reservation entity with expiry
  and release — a much bigger contract than this function suggests (R7).
- **Is stock global per product, or per warehouse/location?** The table has
  no warehouse dimension; adding one later changes the primary key and
  every query.
- **What should happen on partial availability in a multi-item order** —
  reject everything (all-or-nothing) or reserve what is available?
- **How do client retries reach us?** If timeouts and retries are expected
  (they are, over any network), we need an idempotency contract — who
  generates the key and what its scope is.
- **Is overselling ever acceptable** (e.g. backorders allowed up to N), or
  is "never negative" a hard invariant? The answer decides between a hard
  `CHECK` and a softer policy.
