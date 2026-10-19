// Package store keeps Raedar's history in PostgreSQL: the model tables, the
// measurements, migrations, rollups, baselines and retention.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxConnections is the number of connections the pool may open.
const maxConnections = 4

// ErrLockTaken reports that another instance already holds the instance lock
// on this database.
var ErrLockTaken = errors.New("another instance holds the lock")

// Store is Raedar's access to its database. It owns a pool of at most four
// connections; one of them may be held for the instance lock.
type Store struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	lockConn *pgxpool.Conn
}

// Open connects to PostgreSQL, retrying until the server is reachable or ctx
// is done, and returns a store with a pool of at most four connections.
func Open(ctx context.Context, databaseURL string, logger *slog.Logger) (*Store, error) {
	return openStore(ctx, databaseURL, logger, true)
}

// Dial connects to PostgreSQL once, without retrying. It is used by commands
// that must report a failure instead of waiting for a database.
func Dial(ctx context.Context, databaseURL string) (*Store, error) {
	return openStore(ctx, databaseURL, nil, false)
}

// openStore connects to PostgreSQL, retrying when retrying is enabled.
func openStore(ctx context.Context, databaseURL string, logger *slog.Logger, retries bool) (*Store, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	poolConfig.MaxConns = maxConnections
	poolConfig.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}
	s := &Store{pool: pool, logger: logger}
	if !retries {
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
		}
		return s, nil
	}
	err = retry(ctx, systemClock{}, retryBase, retryMax, logger, func(ctx context.Context) error {
		pingErr := pool.Ping(ctx)
		if pingErr != nil && unrecoverable(pingErr) {
			return fatalError{err: pingErr}
		}
		return pingErr
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	return s, nil
}

// Close releases the instance lock and closes every connection.
func (s *Store) Close() {
	s.releaseLock()
	s.pool.Close()
}

// unrecoverable reports whether waiting cannot make the connection succeed,
// for example when the credentials are wrong or the database is missing.
func unrecoverable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "28000", "28P01", "3D000": // invalid authorization, invalid password, missing database
		return true
	}
	return false
}
