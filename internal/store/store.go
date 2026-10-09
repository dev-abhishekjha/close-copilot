package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Store wraps a pgx connection pool and provides typed query operations
// for Close Copilot's persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// Open creates a new connection pool to the database at the given URL
// and verifies the connection with a ping.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parse db config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("store: create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool returns the underlying *pgxpool.Pool.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Ping checks if the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// WithTx executes the given function inside a database transaction.
// If the function returns an error or panics, the transaction is rolled back;
// otherwise it is committed.
func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		} else if err != nil {
			_ = tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
		}
	}()

	err = fn(tx)
	return err
}

// DB returns a standard library *sql.DB backed by the store's pool.
// The caller should not close the returned *sql.DB directly.
func (s *Store) DB() *sql.DB {
	return stdlib.OpenDBFromPool(s.pool)
}

// Migrate applies migrations from the filesystem directory using goose.
func (s *Store) Migrate(ctx context.Context, migrationsDir string) error {
	db := s.DB()
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: set goose dialect: %w", err)
	}

	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		if errors.Is(err, goose.ErrNoNextVersion) {
			return nil
		}
		return fmt.Errorf("store: run migrations from %s: %w", migrationsDir, err)
	}
	return nil
}

// MigrateFS applies migrations from an embedded or virtual fs.FS using goose.
func (s *Store) MigrateFS(ctx context.Context, fsys fs.FS, dir string) error {
	db := s.DB()
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: set goose dialect: %w", err)
	}

	goose.SetBaseFS(fsys)
	defer goose.SetBaseFS(nil)

	if err := goose.UpContext(ctx, db, dir); err != nil {
		if errors.Is(err, goose.ErrNoNextVersion) {
			return nil
		}
		return fmt.Errorf("store: run migrations from fs %s: %w", dir, err)
	}
	return nil
}
