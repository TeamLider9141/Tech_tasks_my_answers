// Package store owns all PostgreSQL access and the business rules that must
// hold inside database transactions (atomic reservation, no oversell,
// idempotency, lazy expiration). Handlers stay thin on top of it.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"inventory-service/migrations"
)

// migrationLockKey serializes concurrent instances applying migrations.
const migrationLockKey = 727274

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

// Migrate applies embedded SQL migrations that have not been applied yet.
// Safe to run from multiple instances at once: an advisory lock makes sure
// only one of them applies each file.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	return s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}

		applied := map[string]bool{}
		rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return err
		}
		var v string
		if _, err := pgx.ForEachRow(rows, []any{&v}, func() error {
			applied[v] = true
			return nil
		}); err != nil {
			return err
		}

		for _, name := range files {
			if applied[name] {
				continue
			}
			sql, err := migrations.FS.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// withTx runs fn inside a transaction, committing on nil and rolling back on
// error.
func (s *Store) withTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

type Product struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Warehouse struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// CreateProduct and CreateWarehouse exist so the API can be exercised
// end-to-end without seeding the database by hand.

func (s *Store) CreateProduct(ctx context.Context, name string) (Product, error) {
	var p Product
	err := s.pool.QueryRow(ctx,
		`INSERT INTO products (name) VALUES ($1) RETURNING id, name`, name,
	).Scan(&p.ID, &p.Name)
	if isUniqueViolation(err, "products_name_key") {
		return Product{}, &AlreadyExistsError{Kind: "product", Name: name}
	}
	return p, err
}

func (s *Store) CreateWarehouse(ctx context.Context, name string) (Warehouse, error) {
	var w Warehouse
	err := s.pool.QueryRow(ctx,
		`INSERT INTO warehouses (name) VALUES ($1) RETURNING id, name`, name,
	).Scan(&w.ID, &w.Name)
	if isUniqueViolation(err, "warehouses_name_key") {
		return Warehouse{}, &AlreadyExistsError{Kind: "warehouse", Name: name}
	}
	return w, err
}

func (s *Store) warehouseExists(ctx context.Context, tx pgx.Tx, id int64) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM warehouses WHERE id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return &NotFoundError{Kind: "warehouse", ID: id}
	}
	return nil
}
