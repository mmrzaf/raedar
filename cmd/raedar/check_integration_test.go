//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/store"
)

// migratedSchema creates and migrates a schema and returns its URL.
func migratedSchema(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	databaseURL := os.Getenv("RAEDAR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("RAEDAR_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("raedar_check_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Errorf("connect to drop schema: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	st, err := store.Dial(ctx, parsed.String())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return parsed.String()
}

func TestCheckDatabase(t *testing.T) {
	schemaURL := migratedSchema(t)
	var out bytes.Buffer
	result := &report{out: &out}
	checkDatabase(context.Background(), &config.Config{DatabaseURL: schemaURL}, result)
	if result.failed {
		t.Fatalf("checkDatabase() failed: %s", out.String())
	}
	if !strings.Contains(out.String(), "reachable; schema version 1") {
		t.Errorf("output = %q, want the schema version", out.String())
	}
}

func TestCheckDatabaseReportsFailures(t *testing.T) {
	var out bytes.Buffer
	result := &report{out: &out}
	checkDatabase(context.Background(), &config.Config{DatabaseURL: "postgres://127.0.0.1:1/nope"}, result)
	if !result.failed {
		t.Fatal("checkDatabase() did not fail for an unreachable database")
	}
	if !strings.HasPrefix(out.String(), "fail") {
		t.Errorf("output = %q, want a failure line", out.String())
	}
}
