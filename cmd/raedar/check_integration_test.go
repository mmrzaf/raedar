//go:build integration

package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// migratedSchema creates and migrates a schema and returns its URL.
func migratedSchema(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	databaseURL, schema := testdb.NewSchema(t, "check")
	schemaURL := testdb.WithSearchPath(t, databaseURL, schema)
	st, err := store.Dial(ctx, schemaURL)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return schemaURL
}

func TestCheckDatabase(t *testing.T) {
	schemaURL := migratedSchema(t)
	var out bytes.Buffer
	result := &report{out: &out}
	checkDatabase(context.Background(), &config.Config{DatabaseURL: schemaURL}, result)
	if result.failed {
		t.Fatalf("checkDatabase() failed: %s", out.String())
	}
	if !regexp.MustCompile(`reachable; schema version \d+`).MatchString(out.String()) {
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
