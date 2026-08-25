package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"inventory-service/internal/store"
)

// validationError marks client-side input problems (malformed JSON, missing
// or out-of-range fields) that map to 400.
type validationError struct{ msg string }

func (e validationError) Error() string { return e.msg }

type errorPayload struct {
	Code      string           `json:"code"`
	Message   string           `json:"message"`
	Status    string           `json:"status,omitempty"`    // current reservation status on state conflicts
	Shortages []store.Shortage `json:"shortages,omitempty"` // per-product detail on insufficient stock
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	var (
		notFound     *store.NotFoundError
		exists       *store.AlreadyExistsError
		insufficient *store.InsufficientStockError
		transition   *store.InvalidTransitionError
		validation   validationError
	)
	switch {
	case errors.As(err, &validation):
		writeErr(w, http.StatusBadRequest, errorPayload{Code: "invalid_request", Message: validation.msg})
	case errors.Is(err, store.ErrInvalidQuantity):
		writeErr(w, http.StatusBadRequest, errorPayload{Code: "invalid_quantity", Message: err.Error()})
	case errors.As(err, &notFound):
		writeErr(w, http.StatusNotFound, errorPayload{Code: "not_found", Message: notFound.Error()})
	case errors.As(err, &exists):
		writeErr(w, http.StatusConflict, errorPayload{Code: "already_exists", Message: exists.Error()})
	case errors.As(err, &insufficient):
		writeErr(w, http.StatusConflict, errorPayload{
			Code:      "insufficient_stock",
			Message:   insufficient.Error(),
			Shortages: insufficient.Shortages,
		})
	case errors.As(err, &transition):
		code := "invalid_state"
		if transition.Status == store.StatusExpired {
			// Called out separately because "the reservation you are trying
			// to confirm has expired" is an expected business outcome, not a
			// client programming error.
			code = "reservation_expired"
		}
		writeErr(w, http.StatusConflict, errorPayload{
			Code:    code,
			Message: transition.Error(),
			Status:  string(transition.Status),
		})
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeErr(w, http.StatusConflict, errorPayload{Code: "idempotency_key_conflict", Message: err.Error()})
	default:
		slog.Error("internal error", "err", err)
		writeErr(w, http.StatusInternalServerError, errorPayload{Code: "internal_error", Message: "internal server error"})
	}
}

func writeErr(w http.ResponseWriter, status int, p errorPayload) {
	writeJSON(w, status, map[string]errorPayload{"error": p})
}

// decodeJSON decodes a request body strictly: unknown fields and trailing
// garbage are rejected, and the body size is capped by the caller.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return validationError{msg: "invalid JSON body: " + err.Error()}
	}
	if dec.More() {
		return validationError{msg: "invalid JSON body: unexpected trailing data"}
	}
	return nil
}
