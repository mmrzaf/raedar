//go:build integration

package accesslog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/aggregate"
	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// testSchema creates a schema and returns a migrated store on it.
func testSchema(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	databaseURL, schema := testdb.NewSchema(t, "accesslog")
	st, err := store.Open(ctx, testdb.WithSearchPath(t, databaseURL, schema), discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return st, databaseURL, schema
}

// testConn returns a connection to the test schema for direct queries.
func testConn(t *testing.T, databaseURL, schema string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testdb.WithSearchPath(t, databaseURL, schema))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func TestSourceFeedsTheDatabase(t *testing.T) {
	lines := sampleLines(t, "sample.log")
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	st, databaseURL, schema := testSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := testConn(t, databaseURL, schema)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	discoverer := discovery.New("unix:///var/run/docker.sock", "", "", st, logger, clock.System())
	aggregator := aggregate.New(st, SourceName, logger, clock.System())
	source := New(Options{Path: path}, discoverer, st, aggregator, logger, clock.System())
	source.pollEvery = time.Millisecond
	go func() { source.Run(ctx) }()

	// The source follows the file as it is written, like Traefik does. It
	// starts at the end of the file, so the file is written once it is open.
	waitFor(t, 10*time.Second, func() bool {
		var count int
		err := conn.QueryRow(ctx, "select count(*) from source_state where source = $1", SourceName).Scan(&count)
		return err == nil && count == 1
	})
	appendFile(t, path, lines...)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Flush until the saved position is the end of the file.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(20 * time.Second)
	var position []byte
	var flushErr error
	for {
		if flushErr = aggregator.Flush(ctx); flushErr == nil {
			if err := conn.QueryRow(ctx, "select position from source_state where source = $1", SourceName).Scan(&position); err == nil {
				var saved filePosition
				if json.Unmarshal(position, &saved) == nil && saved.Offset == info.Size() {
					break
				}
			}
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out: position = %s, flush error = %v\nlogs:\n%s", position, flushErr, logs.String())
		}
	}

	// Every line is counted once, under the routes it names.
	var requests int64
	if err := conn.QueryRow(ctx, "select sum(requests) from route_minute").Scan(&requests); err != nil {
		t.Fatalf("sum requests: %v", err)
	}
	if requests != int64(len(lines)) {
		t.Errorf("requests = %d, want %d", requests, len(lines))
	}
	rows, err := conn.Query(ctx, `select name, origin, app_id from routes order by name collate "C"`)
	if err != nil {
		t.Fatalf("read routes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name, origin string
		var appID *int64
		if err := rows.Scan(&name, &origin, &appID); err != nil {
			t.Fatalf("scan route: %v", err)
		}
		names = append(names, name)
		if origin != "accesslog" {
			t.Errorf("route %s origin = %q, want accesslog", name, origin)
		}
		if appID != nil {
			t.Errorf("route %s app = %d, want no app without Docker", name, *appID)
		}
	}
	want := []string{"(unrouted)", "api@docker", "dashboard@internal", "git@file", "shop-api@docker"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("routes = %v, want %v", names, want)
	}
	// The status lines of the sample: 502 responses without a backend.
	var noBackend int64
	if err := conn.QueryRow(ctx, "select sum(no_backend) from route_minute").Scan(&noBackend); err != nil {
		t.Fatalf("sum no_backend: %v", err)
	}
	if noBackend != 4 {
		t.Errorf("no_backend = %d, want 4", noBackend)
	}
}
