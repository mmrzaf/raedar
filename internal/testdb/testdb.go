// Package testdb gives the integration tests an isolated PostgreSQL schema on
// the database named by RAEDAR_TEST_DATABASE_URL.
//
// It is only imported by tests. The integration tests are built with the
// integration tag, so a missing database is a failure, not a reason to skip:
// a run that silently tested nothing would look green.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// URL returns the disposable test database URL and fails the test when it is
// not set.
func URL(t testing.TB) string {
	t.Helper()
	databaseURL := os.Getenv("RAEDAR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("RAEDAR_TEST_DATABASE_URL is not set; the integration tests need a disposable PostgreSQL database")
	}
	return databaseURL
}

// WithSearchPath returns the URL with its search_path set to schema.
func WithSearchPath(t testing.TB, databaseURL, schema string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURL, err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// NewSchema creates a schema that is unique to the test, drops it when the test
// ends, and returns the database URL and the schema name. prefix names the
// package, which keeps leftover schemas easy to attribute.
func NewSchema(t testing.TB, prefix string) (databaseURL, schema string) {
	t.Helper()
	ctx := context.Background()
	databaseURL = URL(t)
	schema = fmt.Sprintf("raedar_%s_%d", prefix, time.Now().UnixNano())
	exec(ctx, t, databaseURL, "create schema "+pgx.Identifier{schema}.Sanitize())
	t.Cleanup(func() {
		exec(ctx, t, databaseURL, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade")
	})
	return databaseURL, schema
}

// exec runs one statement on its own connection.
func exec(ctx context.Context, t testing.TB, databaseURL, statement string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Errorf("connect to PostgreSQL: %v", err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Errorf("%s: %v", statement, err)
	}
}
