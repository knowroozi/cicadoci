// Package testdb gives tests a migrated, isolated Postgres database.
//
// Tests that need a database are skipped unless CICADO_TEST_DATABASE_URL is set.
// `make test` therefore stays hermetic and fast; `make test-all` and CI set the
// variable and run everything. scripts/with-postgres.sh will start a throwaway
// cluster for you if you do not have one.
package testdb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cicadoci.com/cicado/server/store"
)

// EnvVar names the connection string tests use.
const EnvVar = "CICADO_TEST_DATABASE_URL"

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

// New returns a Store on a freshly migrated database, unique to this test, and
// registers cleanup that drops it.
//
// Each test gets its own database rather than a shared one with truncation
// between tests. That keeps parallel tests genuinely independent — which
// matters here, because the queue tests are specifically about what concurrent
// transactions do to each other, and a neighbouring test's rows would be
// indistinguishable from a bug.
//
// The schema is migrated once into a template database and then cloned with
// CREATE DATABASE ... TEMPLATE, so the per-test cost is a file copy rather than
// a migration run.
func New(t *testing.T) *store.Store {
	t.Helper()

	baseDSN := os.Getenv(EnvVar)
	if baseDSN == "" {
		t.Skipf("%s is not set; skipping database test (see scripts/with-postgres.sh)", EnvVar)
	}

	templateOnce.Do(func() { templateName, templateErr = buildTemplate(baseDSN) })
	if templateErr != nil {
		t.Fatalf("testdb: build template database: %v", templateErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbName := "cicado_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		"CREATE DATABASE %s TEMPLATE %s", quote(dbName), quote(templateName))); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("testdb: create database: %v", err)
	}
	_ = admin.Close(ctx)

	dsn := replaceDatabase(baseDSN, dbName)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb: open %s: %v", dbName, err)
	}

	t.Cleanup(func() {
		st.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupCtx, baseDSN)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
		_, _ = conn.Exec(cleanupCtx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quote(dbName)))
	})

	return st
}

// DSN is New plus the connection string, for tests that need their own
// connection (the LISTEN/NOTIFY path, which cannot use a pooled one).
func DSN(t *testing.T, st *store.Store) string {
	t.Helper()
	return st.DSN()
}

func buildTemplate(baseDSN string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	name := "cicado_tmpl_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return "", err
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
		return "", err
	}

	st, err := store.Open(ctx, replaceDatabase(baseDSN, name))
	if err != nil {
		return "", err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return "", err
	}
	return name, nil
}

// quote renders an identifier safely. Names here are generated from UUIDs, but
// building SQL by concatenation without quoting is a habit worth not having.
func quote(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// replaceDatabase swaps the database name in a postgres:// URL.
func replaceDatabase(dsn, dbName string) string {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return dsn
	}
	hostPart, query, hasQuery := strings.Cut(rest, "?")
	authority, _, hasPath := strings.Cut(hostPart, "/")
	if !hasPath {
		authority = hostPart
	}
	out := scheme + "://" + authority + "/" + dbName
	if hasQuery {
		out += "?" + query
	}
	return out
}
