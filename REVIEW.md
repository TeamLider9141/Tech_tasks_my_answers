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

1. **Race condition → oversell.** The `SELECT` and the `UPDATE` are two
   separate statements. Two parallel calls with `quantity = 1` both read
   `1`, both pass the check, both subtract → final quantity is `-1`. The
   product is oversold. This is the main bug.
2. **Negative stock is not blocked.** No `WHERE quantity >= $1` in the
   `UPDATE`, and no `CHECK` constraint on the table.
3. **`qty` is not validated.** `qty = 0` does nothing but "succeeds".
   A negative qty actually *adds* stock.
4. **No idempotency.** A client that times out and retries subtracts stock
   twice for one order.
5. **Errors are hard to use.** A missing product comes out as raw
   `sql.ErrNoRows`, so the caller can't tell 404 from 500. "not enough
   stock" is just a string — you'd have to match on the text.
6. **The `UPDATE` result is not checked.** `RowsAffected` can be 0 and the
   function still returns nil.
7. **Nothing is recorded.** Stock goes down but there is no reservation
   row, so nothing can be confirmed, cancelled or released later. If the
   caller crashes, that stock is lost. (Maybe that's the intended scope —
   see section 5.)

## 2. Business rule or implementation?

- Oversell (1) — a business rule broken because of an implementation
  problem (no atomicity).
- Stock never negative (2) — business rule, should also be a DB constraint.
- Positive quantity (3) — business rule, input validation.
- Retry safety (4) — business rule, needs implementation support
  (idempotency key).
- Error types (5) and `RowsAffected` (6) — implementation.
- No reservation record (7) — first a business scope question, then
  implementation.

## 3. Fix

Do the check and the subtraction in one statement, then the database does
it atomically and the race is gone:

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

Also add a constraint so nothing else can break the rule:

```sql
ALTER TABLE stock ADD CONSTRAINT stock_quantity_nonnegative CHECK (quantity >= 0);
```

If this needs to grow (several products at once, a reservation record, an
expiry — problem 7), one `UPDATE` is not enough anymore. Then: a
transaction, `SELECT ... FOR UPDATE` on the stock rows in a fixed order,
check, insert, commit. That's how the main service in this repo works. The
idempotency key there fixes problem 4.

## 4. Tests

1. **Concurrency** (the most important one, on a real PostgreSQL): stock 3,
   10 goroutines call `Reserve(..., 1)`. Exactly 3 succeed, 7 get
   `ErrInsufficientStock`, final quantity exactly 0 — never negative. Fails
   before the fix, can't fail after.
2. **Boundary:** stock 5, reserve 5 → ok; reserve 1 more → error.
3. **Validation:** qty 0 and -3 → `ErrInvalidQuantity`, stock unchanged.
4. **Unknown product** → `ErrProductNotStocked`, not a raw scan error.
5. **Constraint:** a direct `UPDATE stock SET quantity = -1` is rejected by
   the `CHECK`.
6. **If idempotency is added:** same key twice → one decrement.

## 5. Questions for the product owner

- Is `Reserve` a final decrement or a temporary hold? If a hold, we need a
  reservation entity with expiry and release (problem 7).
- Is stock per product, or per warehouse? Adding a warehouse later changes
  the schema and every query.
- A multi-item order with partial stock — reject all, or reserve what's
  there?
- Are retries expected? Then who generates the idempotency key and what's
  its scope?
- Is overselling ever allowed (backorders)? That decides between a hard
  `CHECK` and a softer rule.
