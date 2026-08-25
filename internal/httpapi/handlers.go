package httpapi

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"inventory-service/internal/store"
)

const (
	maxBodyBytes = 1 << 20
	// maxQuantity keeps arithmetic far away from bigint overflow and rejects
	// obviously nonsensical requests.
	maxQuantity = 1_000_000_000
	maxItems    = 100
	maxNameLen  = 200
	maxKeyLen   = 200
)

type createNameReq struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req createNameReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxNameLen {
		writeError(w, validationError{msg: "name is required and must be 1..200 characters"})
		return
	}
	p, err := s.store.CreateProduct(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleCreateWarehouse(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req createNameReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxNameLen {
		writeError(w, validationError{msg: "name is required and must be 1..200 characters"})
		return
	}
	wh, err := s.store.CreateWarehouse(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wh)
}

type addStockReq struct {
	ProductID int64 `json:"product_id"`
	Quantity  int64 `json:"quantity"`
}

func (s *Server) handleAddStock(w http.ResponseWriter, r *http.Request) {
	warehouseID, err := pathID(r, "warehouse_id")
	if err != nil {
		writeError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req addStockReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.ProductID <= 0 {
		writeError(w, validationError{msg: "product_id is required and must be positive"})
		return
	}
	if req.Quantity <= 0 || req.Quantity > maxQuantity {
		writeError(w, validationError{msg: fmt.Sprintf("quantity must be between 1 and %d", maxQuantity)})
		return
	}
	level, err := s.store.AddStock(r.Context(), warehouseID, req.ProductID, req.Quantity)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, level)
}

func (s *Server) handleGetStock(w http.ResponseWriter, r *http.Request) {
	warehouseID, err := pathID(r, "warehouse_id")
	if err != nil {
		writeError(w, err)
		return
	}
	productID, err := pathID(r, "product_id")
	if err != nil {
		writeError(w, err)
		return
	}
	level, err := s.store.GetStock(r.Context(), warehouseID, productID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, level)
}

type reservationItemReq struct {
	ProductID int64 `json:"product_id"`
	Quantity  int64 `json:"quantity"`
}

type createReservationReq struct {
	WarehouseID int64                `json:"warehouse_id"`
	Items       []reservationItemReq `json:"items"`
}

func (s *Server) handleCreateReservation(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > maxKeyLen {
		writeError(w, validationError{msg: "Idempotency-Key header is required and must be 1..200 characters"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req createReservationReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.WarehouseID <= 0 {
		writeError(w, validationError{msg: "warehouse_id is required and must be positive"})
		return
	}
	if len(req.Items) == 0 || len(req.Items) > maxItems {
		writeError(w, validationError{msg: fmt.Sprintf("items must contain between 1 and %d entries", maxItems)})
		return
	}
	seen := map[int64]bool{}
	for _, it := range req.Items {
		if it.ProductID <= 0 {
			writeError(w, validationError{msg: "every item needs a positive product_id"})
			return
		}
		if it.Quantity <= 0 || it.Quantity > maxQuantity {
			writeError(w, validationError{msg: fmt.Sprintf("every item quantity must be between 1 and %d", maxQuantity)})
			return
		}
		if seen[it.ProductID] {
			writeError(w, validationError{msg: fmt.Sprintf("duplicate product_id %d in items", it.ProductID)})
			return
		}
		seen[it.ProductID] = true
	}

	in := store.NewReservation{
		WarehouseID:    req.WarehouseID,
		IdempotencyKey: key,
		RequestHash:    hashRequest(req),
	}
	for _, it := range req.Items {
		in.Items = append(in.Items, store.ReservationItem{ProductID: it.ProductID, Quantity: it.Quantity})
	}

	res, created, err := s.store.CreateReservation(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK // idempotent replay of an earlier request
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

// hashRequest fingerprints the semantic payload (warehouse + sorted items),
// so a retried request matches its original even if JSON key order or
// whitespace differ, while a reused key with different contents is caught.
func hashRequest(req createReservationReq) string {
	items := slices.Clone(req.Items)
	slices.SortFunc(items, func(a, b reservationItemReq) int {
		return cmp.Compare(a.ProductID, b.ProductID)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "w=%d", req.WarehouseID)
	for _, it := range items {
		fmt.Fprintf(&b, ";%d:%d", it.ProductID, it.Quantity)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (s *Server) handleGetReservation(w http.ResponseWriter, r *http.Request) {
	s.reservationOp(w, r, s.store.GetReservation)
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	s.reservationOp(w, r, s.store.Confirm)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.reservationOp(w, r, s.store.Cancel)
}

// reservationOp shares the id-validation and response plumbing of the three
// single-reservation endpoints (get, confirm, cancel).
func (s *Server) reservationOp(w http.ResponseWriter, r *http.Request,
	op func(ctx context.Context, id string) (store.Reservation, error)) {
	id := r.PathValue("reservation_id")
	if !isUUID(id) {
		writeError(w, &store.NotFoundError{Kind: "reservation", ID: id})
		return
	}
	res, err := op(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, &store.NotFoundError{Kind: strings.TrimSuffix(name, "_id"), ID: raw}
	}
	return v, nil
}

// isUUID reports whether s looks like a canonical UUID. A malformed id can
// never name an existing reservation, so handlers turn it into a 404 without
// asking the database.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
