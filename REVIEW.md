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

**1. Race condition → oversell.** The `SELECT` and the `UPDATE` are two
separate statements (possibly even on two different pool connections).
Between them another `Reserve` call can read the same value. With
`quantity = 1`, two concurrent calls both read `1`, both pass the
`available < qty` check, both subtract — final quantity is `-1`. The product
is oversold. This is the main bug of the function.

**2. Negative stock isn't blocked anywhere.** The `UPDATE` subtracts
unconditionally: there is no `WHERE quantity >= $1`, and no
`CHECK (quantity >= 0)` on the table (at least nothing visible in this
snippet).

**3. `qty` isn't validated.** `qty = 0` "succeeds" and does nothing useful.
`qty < 0` actually *increases* stock, because subtracting a negative number
adds. A caller bug silently corrupts inventory.

**4. A retry reserves twice.** There is no idempotency. A client that timed
out and retries subtracts stock twice for one real order.

**5. Errors can't be told apart.** A missing product row comes back as a raw
`sql.ErrNoRows` from `Scan`, so the caller can't distinguish "no such
product" (404) from "database is down" (500). And
`errors.New("not enough stock")` is just a plain string — mapping it to an
API response would need string matching.

**6. The `UPDATE` result isn't checked.** If the row disappears between the
two statements, `Reserve` still returns success while zero rows changed.
`RowsAffected` should be checked.

**7. Nothing records what was reserved.** Stock goes down but there is no
reservation row — nothing to confirm, cancel, expire or audit. If the caller
crashes right after `Reserve`, that stock is lost forever. (This might be
the intended scope — see section 5.)

## 2. Business rule or implementation?

| # | Problem                              | What it is                                                        |
|---|--------------------------------------|-------------------------------------------------------------------|
| 1 | Oversell under concurrency           | Business rule, broken by an implementation flaw (no atomicity)    |
| 2 | Stock must never go negative         | Business rule (should also be enforced in the DB)                 |
| 3 | Quantity must be positive            | Business rule (input validation)                                  |
| 4 | A retry must not double-reserve      | Business rule (idempotency), needs implementation support         |
| 5 | Errors must be distinguishable       | Implementation                                                    |
| 6 | Unchecked `RowsAffected`             | Implementation                                                    |
| 7 | No reservation record / release path | Business scope question first, then implementation                |

## 3. How I would fix it

Merge the check and the decrement into one guarded statement. Then the
database does the check and the write atomically, and the race disappears:

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

Plus a constraint in the schema, so no future code path can break the rule:

```sql
ALTER TABLE stock ADD CONSTRAINT stock_quantity_nonnegative CHECK (quantity >= 0);
```

If the operation needs to grow (several products at once, a reservation
record, an expiry — problem 7), one statement isn't enough anymore. Then:
one transaction, `SELECT ... FOR UPDATE` on the stock rows in a fixed order,
check, insert the reservation, commit. That's the design I used in the main
service in this repository. The retry problem (4) is solved there with a
unique idempotency key that the client sends.

## 4. Tests that prove the fix

1. **Concurrency (the most important one, against real PostgreSQL):**
   stock 3, run 10 goroutines each calling `Reserve(..., 1)`. Exactly 3 must
   succeed, 7 must get `ErrInsufficientStock`, and the final quantity must
   be exactly 0 — never negative. Before the fix this test fails (not on
   every run, but reliably with `-race` and repeated runs). After the fix it
   can't fail.
2. **Boundary:** stock 5, reserve 5 → success, quantity 0; reserve 1 more →
   `ErrInsufficientStock`.
3. **Validation:** `qty = 0` and `qty = -3` → `ErrInvalidQuantity`, quantity
   unchanged (a negative qty must not increase stock).
4. **Unknown product:** → `ErrProductNotStocked`, not a raw scan error.
5. **Constraint:** a direct `UPDATE stock SET quantity = -1` must be
   rejected by the `CHECK`.
6. **If idempotency gets added (4):** same key twice → one decrement; two
   different keys → two decrements.

## 5. What I would ask the product owner before implementing

- Is `Reserve` a final decrement or a temporary hold? If checkout can be
  abandoned, we need a reservation entity with expiry and release — a much
  bigger contract than this function suggests (problem 7).
- Is stock global per product, or per warehouse/location? The table has no
  warehouse column; adding one later changes the primary key and every
  query.
- In a multi-item order with partial availability — reject everything, or
  reserve what is available?
- Are client retries expected? (Over a network: yes.) Then we need an
  idempotency agreement: who generates the key and what its scope is.
- Is overselling ever acceptable (for example backorders up to a limit), or
  is "never negative" a hard rule? The answer decides between a hard
  `CHECK` and a softer policy.
