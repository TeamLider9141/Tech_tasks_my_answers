package store

import (
	"errors"
	"fmt"
)

var (
	// ErrIdempotencyConflict means an idempotency key was reused with a
	// different request payload.
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different payload")

	// ErrInvalidQuantity guards against non-positive quantities reaching SQL.
	ErrInvalidQuantity = errors.New("quantity must be a positive integer")
)

// NotFoundError identifies which entity was missing.
type NotFoundError struct {
	Kind string // "warehouse", "product", "reservation"
	ID   any
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %v not found", e.Kind, e.ID)
}

// AlreadyExistsError is returned when a unique name is taken.
type AlreadyExistsError struct {
	Kind string
	Name string
}

func (e *AlreadyExistsError) Error() string {
	return fmt.Sprintf("%s %q already exists", e.Kind, e.Name)
}

// Shortage describes one product that could not be reserved.
type Shortage struct {
	ProductID int64 `json:"product_id"`
	Requested int64 `json:"requested"`
	Available int64 `json:"available"`
}

// InsufficientStockError carries every shortage so the client can see the
// full picture in one response. Its presence means the reservation was NOT
// created (all-or-nothing).
type InsufficientStockError struct {
	Shortages []Shortage
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient stock for %d product(s)", len(e.Shortages))
}

// InvalidTransitionError is returned when confirm/cancel is requested for a
// reservation whose current state does not allow it (e.g. confirming an
// expired or cancelled reservation).
type InvalidTransitionError struct {
	Action string // "confirm" or "cancel"
	Status Status // current reservation status
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("cannot %s a reservation in status %q", e.Action, e.Status)
}
