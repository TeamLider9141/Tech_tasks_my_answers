package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type StockLevel struct {
	WarehouseID int64 `json:"warehouse_id"`
	ProductID   int64 `json:"product_id"`
	Physical    int64 `json:"physical"`
	Reserved    int64 `json:"reserved"`
	Available   int64 `json:"available"`
}

// AddStock increases physical stock for a product in a warehouse and returns
// the resulting level. Only positive adjustments are supported; removing
// stock is out of scope for this service.
func (s *Store) AddStock(ctx context.Context, warehouseID, productID, qty int64) (StockLevel, error) {
	if qty <= 0 {
		return StockLevel{}, ErrInvalidQuantity
	}
	level := StockLevel{WarehouseID: warehouseID, ProductID: productID}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := s.warehouseExists(ctx, tx, warehouseID); err != nil {
			return err
		}
		if err := productsExist(ctx, tx, []int64{productID}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO stock (warehouse_id, product_id, quantity)
			VALUES ($1, $2, $3)
			ON CONFLICT (warehouse_id, product_id)
			DO UPDATE SET quantity = stock.quantity + EXCLUDED.quantity, updated_at = now()
			RETURNING quantity`,
			warehouseID, productID, qty,
		).Scan(&level.Physical); err != nil {
			return err
		}
		reserved, err := reservedByProduct(ctx, tx, warehouseID, []int64{productID})
		if err != nil {
			return err
		}
		level.Reserved = reserved[productID]
		level.Available = level.Physical - level.Reserved
		return nil
	})
	if err != nil {
		return StockLevel{}, err
	}
	return level, nil
}

// GetStock reports physical, reserved and available stock. A product that
// exists but has never been stocked in the warehouse reports zeros rather
// than an error.
func (s *Store) GetStock(ctx context.Context, warehouseID, productID int64) (StockLevel, error) {
	level := StockLevel{WarehouseID: warehouseID, ProductID: productID}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := s.warehouseExists(ctx, tx, warehouseID); err != nil {
			return err
		}
		if err := productsExist(ctx, tx, []int64{productID}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(
				(SELECT quantity FROM stock WHERE warehouse_id = $1 AND product_id = $2), 0)`,
			warehouseID, productID,
		).Scan(&level.Physical); err != nil {
			return err
		}
		reserved, err := reservedByProduct(ctx, tx, warehouseID, []int64{productID})
		if err != nil {
			return err
		}
		level.Reserved = reserved[productID]
		level.Available = level.Physical - level.Reserved
		return nil
	})
	if err != nil {
		return StockLevel{}, err
	}
	return level, nil
}

func productsExist(ctx context.Context, tx pgx.Tx, ids []int64) error {
	rows, err := tx.Query(ctx, `SELECT id FROM products WHERE id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	found := map[int64]bool{}
	var id int64
	if _, err := pgx.ForEachRow(rows, []any{&id}, func() error {
		found[id] = true
		return nil
	}); err != nil {
		return err
	}
	for _, want := range ids {
		if !found[want] {
			return &NotFoundError{Kind: "product", ID: want}
		}
	}
	return nil
}

// reservedByProduct sums quantities held by active, non-expired reservations
// for the given products. Expiration uses the database clock (now()), so an
// expired reservation stops reducing availability even before its status
// column is materialized to 'expired'.
func reservedByProduct(ctx context.Context, tx pgx.Tx, warehouseID int64, productIDs []int64) (map[int64]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT ri.product_id, SUM(ri.quantity)::bigint
		FROM reservation_items ri
		JOIN reservations r ON r.id = ri.reservation_id
		WHERE r.warehouse_id = $1
		  AND ri.product_id = ANY($2)
		  AND r.status = 'active'
		  AND r.expires_at > now()
		GROUP BY ri.product_id`,
		warehouseID, productIDs)
	if err != nil {
		return nil, err
	}
	reserved := map[int64]int64{}
	var productID, qty int64
	if _, err := pgx.ForEachRow(rows, []any{&productID, &qty}, func() error {
		reserved[productID] = qty
		return nil
	}); err != nil {
		return nil, err
	}
	return reserved, nil
}
