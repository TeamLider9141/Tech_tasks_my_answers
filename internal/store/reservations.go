package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusConfirmed Status = "confirmed"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
)

type ReservationItem struct {
	ProductID int64 `json:"product_id"`
	Quantity  int64 `json:"quantity"`
}

type Reservation struct {
	ID          string            `json:"id"`
	WarehouseID int64             `json:"warehouse_id"`
	Status      Status            `json:"status"`
	Items       []ReservationItem `json:"items"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	FinalizedAt *time.Time        `json:"finalized_at,omitempty"`
}

// toUTC normalizes timestamps to UTC before they cross the application
// boundary. PostgreSQL stores timestamptz as an absolute instant; this only
// fixes the serialized representation.
func (r *Reservation) toUTC() {
	r.CreatedAt = r.CreatedAt.UTC()
	r.ExpiresAt = r.ExpiresAt.UTC()
	if r.FinalizedAt != nil {
		u := r.FinalizedAt.UTC()
		r.FinalizedAt = &u
	}
}

type NewReservation struct {
	WarehouseID    int64
	Items          []ReservationItem
	IdempotencyKey string
	// RequestHash fingerprints the payload so a reused idempotency key with
	// different contents is detected instead of silently returning the old
	// reservation.
	RequestHash string
}

// CreateReservation atomically reserves every requested item or nothing.
// The bool result is true when a new reservation was created and false when
// an idempotent replay returned the existing one.
//
// Concurrency: the stock rows of the requested products are locked with
// SELECT ... FOR UPDATE in product_id order. Any two requests touching the
// same product serialize on those row locks — across goroutines and across
// service instances — so the availability check and the insert behave as one
// atomic step and overselling is impossible.
func (s *Store) CreateReservation(ctx context.Context, in NewReservation) (Reservation, bool, error) {
	// Idempotent replay fast path.
	if res, hash, found, err := s.reservationByKey(ctx, in.IdempotencyKey); err != nil {
		return Reservation{}, false, err
	} else if found {
		if hash != in.RequestHash {
			return Reservation{}, false, ErrIdempotencyConflict
		}
		return res, false, nil
	}

	items := slices.Clone(in.Items)
	slices.SortFunc(items, func(a, b ReservationItem) int {
		return cmp.Compare(a.ProductID, b.ProductID)
	})
	ids := make([]int64, len(items))
	qtys := make([]int64, len(items))
	for i, it := range items {
		if it.Quantity <= 0 {
			return Reservation{}, false, ErrInvalidQuantity
		}
		ids[i] = it.ProductID
		qtys[i] = it.Quantity
	}

	var created Reservation
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := s.warehouseExists(ctx, tx, in.WarehouseID); err != nil {
			return err
		}
		if err := productsExist(ctx, tx, ids); err != nil {
			return err
		}

		// Lock stock rows in deterministic (product_id) order. A product
		// with no stock row is simply availability 0 — nothing to lock and
		// nothing that could be oversold.
		physical := map[int64]int64{}
		rows, err := tx.Query(ctx, `
			SELECT product_id, quantity
			FROM stock
			WHERE warehouse_id = $1 AND product_id = ANY($2)
			ORDER BY product_id
			FOR UPDATE`,
			in.WarehouseID, ids)
		if err != nil {
			return err
		}
		var pid, qty int64
		if _, err := pgx.ForEachRow(rows, []any{&pid, &qty}, func() error {
			physical[pid] = qty
			return nil
		}); err != nil {
			return err
		}

		reserved, err := reservedByProduct(ctx, tx, in.WarehouseID, ids)
		if err != nil {
			return err
		}

		var shortages []Shortage
		for _, it := range items {
			available := physical[it.ProductID] - reserved[it.ProductID]
			if it.Quantity > available {
				shortages = append(shortages, Shortage{
					ProductID: it.ProductID,
					Requested: it.Quantity,
					Available: max(available, 0),
				})
			}
		}
		if len(shortages) > 0 {
			return &InsufficientStockError{Shortages: shortages}
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO reservations (warehouse_id, idempotency_key, request_hash, expires_at)
			VALUES ($1, $2, $3, now() + interval '15 minutes')
			RETURNING id, status, created_at, expires_at`,
			in.WarehouseID, in.IdempotencyKey, in.RequestHash,
		).Scan(&created.ID, &created.Status, &created.CreatedAt, &created.ExpiresAt); err != nil {
			return err
		}
		created.WarehouseID = in.WarehouseID
		created.Items = items

		_, err = tx.Exec(ctx, `
			INSERT INTO reservation_items (reservation_id, product_id, quantity)
			SELECT $1, unnest($2::bigint[]), unnest($3::bigint[])`,
			created.ID, ids, qtys)
		return err
	})
	if err == nil {
		created.toUTC()
		return created, true, nil
	}

	// Two concurrent requests with the same fresh key: the loser hits the
	// unique index after the winner commits. Serve the winner's reservation,
	// applying the same payload check as the fast path.
	if isUniqueViolation(err, "reservations_idempotency_key_key") {
		res, hash, found, ferr := s.reservationByKey(ctx, in.IdempotencyKey)
		if ferr != nil {
			return Reservation{}, false, ferr
		}
		if !found {
			return Reservation{}, false, err
		}
		if hash != in.RequestHash {
			return Reservation{}, false, ErrIdempotencyConflict
		}
		return res, false, nil
	}
	return Reservation{}, false, err
}

// GetReservation returns a reservation, first materializing lazy expiration
// so the reported status matches what the database clock says.
func (s *Store) GetReservation(ctx context.Context, id string) (Reservation, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE reservations SET status = 'expired'
		WHERE id = $1 AND status = 'active' AND expires_at <= now()`, id); err != nil {
		return Reservation{}, err
	}

	var (
		res  Reservation
		hash string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, warehouse_id, status, request_hash, created_at, expires_at, finalized_at
		FROM reservations WHERE id = $1`, id,
	).Scan(&res.ID, &res.WarehouseID, &res.Status, &hash, &res.CreatedAt, &res.ExpiresAt, &res.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, &NotFoundError{Kind: "reservation", ID: id}
	}
	if err != nil {
		return Reservation{}, err
	}
	res.Items, err = s.loadItems(ctx, id)
	res.toUTC()
	return res, err
}

// Confirm marks an active, non-expired reservation as confirmed and removes
// the sold quantities from physical stock in the same transaction.
func (s *Store) Confirm(ctx context.Context, id string) (Reservation, error) {
	return s.finalize(ctx, id, "confirm", StatusConfirmed)
}

// Cancel marks an active reservation as cancelled. Its stock is released
// implicitly: a non-active reservation no longer counts against
// availability.
func (s *Store) Cancel(ctx context.Context, id string) (Reservation, error) {
	return s.finalize(ctx, id, "cancel", StatusCancelled)
}

func (s *Store) finalize(ctx context.Context, id, action string, target Status) (Reservation, error) {
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// Materialize expiration before inspecting the row, using the
		// database clock. If the transaction later rolls back this update is
		// lost, which is harmless: availability never depends on it and the
		// next touch re-applies it.
		if _, err := tx.Exec(ctx, `
			UPDATE reservations SET status = 'expired'
			WHERE id = $1 AND status = 'active' AND expires_at <= now()`, id); err != nil {
			return err
		}

		var (
			warehouseID int64
			status      Status
		)
		err := tx.QueryRow(ctx, `
			SELECT warehouse_id, status FROM reservations WHERE id = $1 FOR UPDATE`, id,
		).Scan(&warehouseID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{Kind: "reservation", ID: id}
		}
		if err != nil {
			return err
		}

		if status == target {
			// Repeated confirm/cancel: already in the requested state, do
			// nothing and report success.
			return nil
		}
		if status != StatusActive {
			return &InvalidTransitionError{Action: action, Status: status}
		}

		if target == StatusConfirmed {
			// The sale is final: physical stock leaves the warehouse in the
			// same transaction that frees the reservation, so availability
			// is unchanged while physical drops.
			items, err := s.loadItemsTx(ctx, tx, id)
			if err != nil {
				return err
			}
			for _, it := range items {
				tag, err := tx.Exec(ctx, `
					UPDATE stock SET quantity = quantity - $1, updated_at = now()
					WHERE warehouse_id = $2 AND product_id = $3`,
					it.Quantity, warehouseID, it.ProductID)
				if err != nil {
					return fmt.Errorf("deduct stock for product %d: %w", it.ProductID, err)
				}
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("stock row missing for product %d", it.ProductID)
				}
			}
		}

		_, err = tx.Exec(ctx, `
			UPDATE reservations
			SET status = $2::reservation_status, finalized_at = now()
			WHERE id = $1`, id, target)
		return err
	})
	if err != nil {
		return Reservation{}, err
	}
	return s.GetReservation(ctx, id)
}

func (s *Store) reservationByKey(ctx context.Context, key string) (Reservation, string, bool, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE reservations SET status = 'expired'
		WHERE idempotency_key = $1 AND status = 'active' AND expires_at <= now()`, key); err != nil {
		return Reservation{}, "", false, err
	}

	var (
		res  Reservation
		hash string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, warehouse_id, status, request_hash, created_at, expires_at, finalized_at
		FROM reservations WHERE idempotency_key = $1`, key,
	).Scan(&res.ID, &res.WarehouseID, &res.Status, &hash, &res.CreatedAt, &res.ExpiresAt, &res.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, "", false, nil
	}
	if err != nil {
		return Reservation{}, "", false, err
	}
	res.Items, err = s.loadItems(ctx, res.ID)
	if err != nil {
		return Reservation{}, "", false, err
	}
	res.toUTC()
	return res, hash, true, nil
}

func (s *Store) loadItems(ctx context.Context, reservationID string) ([]ReservationItem, error) {
	rows, err := s.pool.Query(ctx, itemsSQL, reservationID)
	if err != nil {
		return nil, err
	}
	return collectItems(rows)
}

func (s *Store) loadItemsTx(ctx context.Context, tx pgx.Tx, reservationID string) ([]ReservationItem, error) {
	rows, err := tx.Query(ctx, itemsSQL, reservationID)
	if err != nil {
		return nil, err
	}
	return collectItems(rows)
}

const itemsSQL = `
	SELECT product_id, quantity
	FROM reservation_items
	WHERE reservation_id = $1
	ORDER BY product_id`

func collectItems(rows pgx.Rows) ([]ReservationItem, error) {
	var items []ReservationItem
	var it ReservationItem
	if _, err := pgx.ForEachRow(rows, []any{&it.ProductID, &it.Quantity}, func() error {
		items = append(items, it)
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}
