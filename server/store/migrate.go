package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migrations are embedded into the binary rather than shipped alongside it.
// An operator upgrading CICaDo replaces one file and runs `cicado-server
// migrate up`; there is no second artifact to keep in step with the binary and
// no migration tool to install. That is the whole reason goose is used as a
// library here instead of as a CLI.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

func gooseProvider(db *sql.DB) (*goose.Provider, error) {
	// goose reads migrations from the root of the filesystem it is handed, so
	// the embedded tree has to be rooted at the migrations directory itself.
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: root migrations FS: %w", err)
	}
	// goose does not take a lock on its own — a Provider with no SessionLocker
	// configured runs unlocked. WithSessionLocker is what makes the rolling-
	// deploy safety this package claims actually true: a Postgres session-level
	// advisory lock (pg_try_advisory_lock), held on the one *sql.Conn each
	// migration runs over, so two instances racing Migrate() serialize instead
	// of racing the same migration.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("store: init migration session locker: %w", err)
	}
	return goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
}

// sqlDB opens a database/sql handle over the same DSN. goose speaks
// database/sql; the rest of the server speaks pgx directly.
func (s *Store) sqlDB() (*sql.DB, error) {
	db, err := sql.Open("pgx", s.dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open DSN for migrations: %w", err)
	}
	return db, nil
}

// Migrate applies every pending migration. It is safe to run concurrently from
// several server instances: gooseProvider configures a session-level Postgres
// advisory lock (see lock.NewPostgresSessionLocker above) held for the
// duration, so a rolling deploy cannot race two copies of the same migration.
func (s *Store) Migrate(ctx context.Context) error {
	db, err := s.sqlDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	provider, err := gooseProvider(db)
	if err != nil {
		return fmt.Errorf("store: init migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("store: apply migrations: %w", err)
	}
	for _, r := range results {
		if r.Error != nil {
			return fmt.Errorf("store: migration %s: %w", r.Source.Path, r.Error)
		}
	}
	return nil
}

// MigrateDown rolls back the most recent migration. Development only; it is not
// wired to any server startup path.
func (s *Store) MigrateDown(ctx context.Context) error {
	db, err := s.sqlDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	provider, err := gooseProvider(db)
	if err != nil {
		return fmt.Errorf("store: init migrations: %w", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		return fmt.Errorf("store: roll back migration: %w", err)
	}
	return nil
}

// Version reports the currently applied schema version.
func (s *Store) Version(ctx context.Context) (int64, error) {
	db, err := s.sqlDB()
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()

	provider, err := gooseProvider(db)
	if err != nil {
		return 0, fmt.Errorf("store: init migrations: %w", err)
	}
	v, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return v, nil
}
