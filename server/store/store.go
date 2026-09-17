// Package store owns the database: connection lifecycle, schema migrations,
// and the low-level helpers everything else builds on.
//
// Postgres is the only stateful dependency CICaDo has (ADR-002). It is the
// metadata store and the job queue both, which is why the queue's claim path
// lives next door in package queue rather than behind a network hop.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is a handle on the database.
type Store struct {
	pool *pgxpool.Pool
	dsn  string
}

// Open connects to Postgres and verifies the connection is usable. It does not
// run migrations; call Migrate for that.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("store: empty database DSN")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse DSN: %w", err)
	}

	// The control plane is mostly short transactions plus a handful of
	// long-lived LISTEN connections. Keep a floor of idle connections so a
	// burst of webhook traffic does not pay connection setup on every job.
	cfg.MinConns = 2
	cfg.MaxConns = 16
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	return &Store{pool: pool, dsn: dsn}, nil
}

// Pool exposes the underlying connection pool for packages that issue queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// DSN returns the connection string this store was opened with. The queue's
// LISTEN loop needs its own dedicated connection rather than a pooled one.
func (s *Store) DSN() string { return s.dsn }

// Close releases all connections.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}
