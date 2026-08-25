// Package httpapi is the transport layer: routing, JSON encoding/decoding,
// request validation and mapping of domain errors to HTTP responses. All
// business rules live in the store package.
package httpapi

import (
	"net/http"

	"inventory-service/internal/store"
)

type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

func New(st *store.Store) *Server {
	s := &Server{store: st, mux: http.NewServeMux()}

	// Products and warehouses are not part of the required API surface, but
	// without a way to create them the service cannot be exercised without
	// hand-written SQL, so minimal create endpoints are provided.
	s.mux.HandleFunc("POST /products", s.handleCreateProduct)
	s.mux.HandleFunc("POST /warehouses", s.handleCreateWarehouse)

	s.mux.HandleFunc("POST /warehouses/{warehouse_id}/stock", s.handleAddStock)
	s.mux.HandleFunc("GET /warehouses/{warehouse_id}/products/{product_id}/stock", s.handleGetStock)

	s.mux.HandleFunc("POST /reservations", s.handleCreateReservation)
	s.mux.HandleFunc("GET /reservations/{reservation_id}", s.handleGetReservation)
	s.mux.HandleFunc("POST /reservations/{reservation_id}/confirm", s.handleConfirm)
	s.mux.HandleFunc("POST /reservations/{reservation_id}/cancel", s.handleCancel)

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}
