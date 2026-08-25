// Integration tests exercising the HTTP API against a real PostgreSQL
// database, as required by the assignment. They cover business behaviour:
// atomic multi-item reservations, no-oversell under concurrency, idempotent
// retries, expiration, and state transitions.
//
// Run `make db-up` first (or set TEST_DATABASE_URL); see README.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-service/internal/httpapi"
	"inventory-service/internal/store"
)

func testDBURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://inventory:inventory@localhost:5434/inventory_test?sslmode=disable"
}

type env struct {
	t    *testing.T
	base string
	pool *pgxpool.Pool // direct DB access for fixture surgery (forcing expiry)
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()

	st, err := store.New(ctx, testDBURL())
	if err != nil {
		t.Fatalf("connect to test database %q: %v\n(is the database up? run `make db-up`)", testDBURL(), err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pool, err := pgxpool.New(ctx, testDBURL())
	if err != nil {
		t.Fatalf("open fixture pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `
		TRUNCATE reservation_items, reservations, stock, products, warehouses
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	ts := httptest.NewServer(httpapi.New(st))
	t.Cleanup(ts.Close)

	return &env{t: t, base: ts.URL, pool: pool}
}

// do sends a JSON request and decodes the JSON response into out (if non-nil).
func (e *env) do(method, path string, headers map[string]string, body, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.base+path, rd)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode
}

type idResp struct {
	ID int64 `json:"id"`
}

type stockResp struct {
	Physical  int64 `json:"physical"`
	Reserved  int64 `json:"reserved"`
	Available int64 `json:"available"`
}

type reservationResp struct {
	ID          string `json:"id"`
	WarehouseID int64  `json:"warehouse_id"`
	Status      string `json:"status"`
	Items       []struct {
		ProductID int64 `json:"product_id"`
		Quantity  int64 `json:"quantity"`
	} `json:"items"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	FinalizedAt *time.Time `json:"finalized_at"`
}

type errResp struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Status    string `json:"status"`
		Shortages []struct {
			ProductID int64 `json:"product_id"`
			Requested int64 `json:"requested"`
			Available int64 `json:"available"`
		} `json:"shortages"`
	} `json:"error"`
}

func (e *env) createProduct(name string) int64 {
	e.t.Helper()
	var p idResp
	if code := e.do("POST", "/products", nil, map[string]any{"name": name}, &p); code != 201 {
		e.t.Fatalf("create product: got %d", code)
	}
	return p.ID
}

func (e *env) createWarehouse(name string) int64 {
	e.t.Helper()
	var w idResp
	if code := e.do("POST", "/warehouses", nil, map[string]any{"name": name}, &w); code != 201 {
		e.t.Fatalf("create warehouse: got %d", code)
	}
	return w.ID
}

func (e *env) addStock(warehouseID, productID, qty int64) {
	e.t.Helper()
	code := e.do("POST", fmt.Sprintf("/warehouses/%d/stock", warehouseID), nil,
		map[string]any{"product_id": productID, "quantity": qty}, nil)
	if code != 200 {
		e.t.Fatalf("add stock: got %d", code)
	}
}

func (e *env) getStock(warehouseID, productID int64) stockResp {
	e.t.Helper()
	var s stockResp
	code := e.do("GET", fmt.Sprintf("/warehouses/%d/products/%d/stock", warehouseID, productID), nil, nil, &s)
	if code != 200 {
		e.t.Fatalf("get stock: got %d", code)
	}
	return s
}

type item struct {
	ProductID int64 `json:"product_id"`
	Quantity  int64 `json:"quantity"`
}

func (e *env) reserve(key string, warehouseID int64, items []item, out any) int {
	e.t.Helper()
	return e.do("POST", "/reservations", map[string]string{"Idempotency-Key": key},
		map[string]any{"warehouse_id": warehouseID, "items": items}, out)
}

// forceExpire rewinds a reservation's deadline so it is expired according to
// the database clock, without waiting 15 minutes.
func (e *env) forceExpire(reservationID string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE reservations SET expires_at = now() - interval '1 second'
		WHERE id = $1`, reservationID); err != nil {
		e.t.Fatalf("force expire: %v", err)
	}
}

func TestCreateMultiItemReservation(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	pa := e.createProduct("keyboard")
	pb := e.createProduct("mouse")
	e.addStock(w, pa, 10)
	e.addStock(w, pb, 5)

	var res reservationResp
	code := e.reserve("key-1", w, []item{{pa, 3}, {pb, 2}}, &res)
	if code != 201 {
		t.Fatalf("expected 201, got %d", code)
	}
	if res.Status != "active" || len(res.Items) != 2 || res.ID == "" {
		t.Fatalf("unexpected reservation: %+v", res)
	}
	if got := res.ExpiresAt.Sub(res.CreatedAt); got != 15*time.Minute {
		t.Fatalf("expected 15m TTL, got %v", got)
	}
	// Timestamps must cross the API boundary in UTC.
	if res.CreatedAt.Location() != time.UTC || res.ExpiresAt.Location() != time.UTC {
		t.Fatalf("timestamps not in UTC: created=%v expires=%v", res.CreatedAt, res.ExpiresAt)
	}

	sa, sb := e.getStock(w, pa), e.getStock(w, pb)
	if sa.Physical != 10 || sa.Reserved != 3 || sa.Available != 7 {
		t.Fatalf("stock A wrong: %+v", sa)
	}
	if sb.Physical != 5 || sb.Reserved != 2 || sb.Available != 3 {
		t.Fatalf("stock B wrong: %+v", sb)
	}
}

func TestReservationIsAllOrNothing(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	pa := e.createProduct("keyboard")
	pb := e.createProduct("mouse")
	e.addStock(w, pa, 10)
	e.addStock(w, pb, 1)

	var er errResp
	code := e.reserve("key-1", w, []item{{pa, 3}, {pb, 2}}, &er)
	if code != 409 || er.Error.Code != "insufficient_stock" {
		t.Fatalf("expected 409 insufficient_stock, got %d %+v", code, er)
	}
	if len(er.Error.Shortages) != 1 || er.Error.Shortages[0].ProductID != pb ||
		er.Error.Shortages[0].Requested != 2 || er.Error.Shortages[0].Available != 1 {
		t.Fatalf("unexpected shortages: %+v", er.Error.Shortages)
	}

	// The item that DID have stock must not be partially reserved.
	if sa := e.getStock(w, pa); sa.Reserved != 0 || sa.Available != 10 {
		t.Fatalf("product A must be untouched, got %+v", sa)
	}
}

func TestConfirmReservation(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	p := e.createProduct("keyboard")
	e.addStock(w, p, 10)

	var res reservationResp
	if code := e.reserve("key-1", w, []item{{p, 3}}, &res); code != 201 {
		t.Fatalf("reserve: %d", code)
	}

	var confirmed reservationResp
	code := e.do("POST", "/reservations/"+res.ID+"/confirm", nil, nil, &confirmed)
	if code != 200 || confirmed.Status != "confirmed" || confirmed.FinalizedAt == nil {
		t.Fatalf("confirm: %d %+v", code, confirmed)
	}

	// Physical stock left the warehouse together with the confirmation.
	if s := e.getStock(w, p); s.Physical != 7 || s.Reserved != 0 || s.Available != 7 {
		t.Fatalf("stock after confirm: %+v", s)
	}

	// Repeating the confirm is a no-op, not a second deduction.
	code = e.do("POST", "/reservations/"+res.ID+"/confirm", nil, nil, &confirmed)
	if code != 200 || confirmed.Status != "confirmed" {
		t.Fatalf("repeat confirm: %d %+v", code, confirmed)
	}
	if s := e.getStock(w, p); s.Physical != 7 {
		t.Fatalf("repeat confirm must not deduct again: %+v", s)
	}
}

func TestCancelReleasesStock(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	p := e.createProduct("keyboard")
	e.addStock(w, p, 10)

	var res reservationResp
	if code := e.reserve("key-1", w, []item{{p, 4}}, &res); code != 201 {
		t.Fatalf("reserve: %d", code)
	}
	if s := e.getStock(w, p); s.Available != 6 {
		t.Fatalf("stock after reserve: %+v", s)
	}

	var cancelled reservationResp
	code := e.do("POST", "/reservations/"+res.ID+"/cancel", nil, nil, &cancelled)
	if code != 200 || cancelled.Status != "cancelled" {
		t.Fatalf("cancel: %d %+v", code, cancelled)
	}
	if s := e.getStock(w, p); s.Physical != 10 || s.Reserved != 0 || s.Available != 10 {
		t.Fatalf("stock after cancel: %+v", s)
	}

	// Repeated cancel: idempotent success.
	if code := e.do("POST", "/reservations/"+res.ID+"/cancel", nil, nil, &cancelled); code != 200 {
		t.Fatalf("repeat cancel: %d", code)
	}

	// Confirming a cancelled reservation is an invalid transition.
	var er errResp
	code = e.do("POST", "/reservations/"+res.ID+"/confirm", nil, nil, &er)
	if code != 409 || er.Error.Code != "invalid_state" || er.Error.Status != "cancelled" {
		t.Fatalf("confirm after cancel: %d %+v", code, er)
	}
}

func TestExpiredReservation(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	p := e.createProduct("keyboard")
	e.addStock(w, p, 10)

	var res reservationResp
	if code := e.reserve("key-1", w, []item{{p, 4}}, &res); code != 201 {
		t.Fatalf("reserve: %d", code)
	}
	e.forceExpire(res.ID)

	// Expired reservations stop reducing available stock even before any
	// endpoint has materialized the status change.
	if s := e.getStock(w, p); s.Reserved != 0 || s.Available != 10 {
		t.Fatalf("expired reservation still holds stock: %+v", s)
	}

	// Confirming after expiry must fail.
	var er errResp
	code := e.do("POST", "/reservations/"+res.ID+"/confirm", nil, nil, &er)
	if code != 409 || er.Error.Code != "reservation_expired" {
		t.Fatalf("confirm expired: %d %+v", code, er)
	}
	if s := e.getStock(w, p); s.Physical != 10 {
		t.Fatalf("confirm of expired reservation must not touch stock: %+v", s)
	}

	// GET reports the expired status.
	var got reservationResp
	if code := e.do("GET", "/reservations/"+res.ID, nil, nil, &got); code != 200 || got.Status != "expired" {
		t.Fatalf("get expired: %d %+v", code, got)
	}
}

func TestIdempotentReplay(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	p := e.createProduct("keyboard")
	e.addStock(w, p, 10)

	var first reservationResp
	if code := e.reserve("key-1", w, []item{{p, 3}}, &first); code != 201 {
		t.Fatalf("first reserve: %d", code)
	}

	// Same key, same payload: the original reservation comes back and no
	// additional stock is held.
	var replay reservationResp
	if code := e.reserve("key-1", w, []item{{p, 3}}, &replay); code != 200 {
		t.Fatalf("replay: expected 200, got %d", code)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay returned a different reservation: %s vs %s", replay.ID, first.ID)
	}
	if s := e.getStock(w, p); s.Reserved != 3 || s.Available != 7 {
		t.Fatalf("replay must not reserve twice: %+v", s)
	}

	// Same key, different payload: rejected.
	var er errResp
	if code := e.reserve("key-1", w, []item{{p, 5}}, &er); code != 409 || er.Error.Code != "idempotency_key_conflict" {
		t.Fatalf("key reuse: %d %+v", code, er)
	}
}

func TestConcurrentReservationsDoNotOversell(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	p := e.createProduct("keyboard")
	e.addStock(w, p, 3)

	const workers = 10
	codes := make([]int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.do("POST", "/reservations",
				map[string]string{"Idempotency-Key": fmt.Sprintf("key-%d", i)},
				map[string]any{"warehouse_id": w, "items": []item{{p, 1}}}, nil)
		}(i)
	}
	wg.Wait()

	created, rejected := 0, 0
	for _, c := range codes {
		switch c {
		case 201:
			created++
		case 409:
			rejected++
		default:
			t.Fatalf("unexpected status %d (all: %v)", c, codes)
		}
	}
	// Exactly the available stock is reserved, never more.
	if created != 3 || rejected != 7 {
		t.Fatalf("expected 3 created / 7 rejected, got %d / %d", created, rejected)
	}
	if s := e.getStock(w, p); s.Reserved != 3 || s.Available != 0 {
		t.Fatalf("stock after concurrent burst: %+v", s)
	}
}

func TestValidationAndInvalidTransitions(t *testing.T) {
	e := setup(t)
	w := e.createWarehouse("main")
	p := e.createProduct("keyboard")
	e.addStock(w, p, 5)

	t.Run("non-positive stock quantity", func(t *testing.T) {
		for _, qty := range []int64{0, -5} {
			code := e.do("POST", fmt.Sprintf("/warehouses/%d/stock", w), nil,
				map[string]any{"product_id": p, "quantity": qty}, nil)
			if code != 400 {
				t.Fatalf("qty %d: expected 400, got %d", qty, code)
			}
		}
	})

	t.Run("non-positive reservation quantity", func(t *testing.T) {
		var er errResp
		if code := e.reserve("k1", w, []item{{p, 0}}, &er); code != 400 {
			t.Fatalf("expected 400, got %d", code)
		}
	})

	t.Run("missing idempotency key", func(t *testing.T) {
		code := e.do("POST", "/reservations", nil,
			map[string]any{"warehouse_id": w, "items": []item{{p, 1}}}, nil)
		if code != 400 {
			t.Fatalf("expected 400, got %d", code)
		}
	})

	t.Run("duplicate product in items", func(t *testing.T) {
		if code := e.reserve("k2", w, []item{{p, 1}, {p, 2}}, nil); code != 400 {
			t.Fatalf("expected 400, got %d", code)
		}
	})

	t.Run("unknown warehouse and product", func(t *testing.T) {
		if code := e.reserve("k3", 9999, []item{{p, 1}}, nil); code != 404 {
			t.Fatalf("unknown warehouse: expected 404, got %d", code)
		}
		if code := e.reserve("k4", w, []item{{9999, 1}}, nil); code != 404 {
			t.Fatalf("unknown product: expected 404, got %d", code)
		}
	})

	t.Run("malformed reservation id", func(t *testing.T) {
		if code := e.do("GET", "/reservations/not-a-uuid", nil, nil, nil); code != 404 {
			t.Fatalf("expected 404, got %d", code)
		}
	})

	t.Run("cancel after confirm", func(t *testing.T) {
		var res reservationResp
		if code := e.reserve("k5", w, []item{{p, 1}}, &res); code != 201 {
			t.Fatalf("reserve: %d", code)
		}
		if code := e.do("POST", "/reservations/"+res.ID+"/confirm", nil, nil, nil); code != 200 {
			t.Fatalf("confirm: %d", code)
		}
		var er errResp
		code := e.do("POST", "/reservations/"+res.ID+"/cancel", nil, nil, &er)
		if code != 409 || er.Error.Code != "invalid_state" || er.Error.Status != "confirmed" {
			t.Fatalf("cancel after confirm: %d %+v", code, er)
		}
	})

	t.Run("unknown JSON fields rejected", func(t *testing.T) {
		code := e.do("POST", "/reservations", map[string]string{"Idempotency-Key": "k6"},
			map[string]any{"warehouse_id": w, "items": []item{{p, 1}}, "surprise": true}, nil)
		if code != 400 {
			t.Fatalf("expected 400, got %d", code)
		}
	})
}
