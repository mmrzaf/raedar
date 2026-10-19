//go:build integration

package aggregate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/store"
)

// testDatabaseURL returns the disposable test database URL.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv("RAEDAR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("RAEDAR_TEST_DATABASE_URL is not set")
	}
	return databaseURL
}

// withSearchPath returns the URL with search_path set to schema.
func withSearchPath(t *testing.T, databaseURL, schema string) string {
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

// testSchema creates a schema and returns a migrated store on it, the
// database URL and the schema name.
func testSchema(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	databaseURL := testDatabaseURL(t)
	schema := fmt.Sprintf("raedar_aggregate_%d", time.Now().UnixNano())
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
	st, err := store.Open(ctx, withSearchPath(t, databaseURL, schema), discardLogger())
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
	conn, err := pgx.Connect(context.Background(), withSearchPath(t, databaseURL, schema))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// insertRoute adds an app and a route and returns the route ID.
func insertRoute(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	appID, err := st.UpsertApp(ctx, "shop", at)
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	routeID, err := st.UpsertRoute(ctx, store.Route{
		Name:   name,
		AppID:  &appID,
		Kind:   "http",
		Origin: "labels",
	}, at)
	if err != nil {
		t.Fatalf("UpsertRoute() error = %v", err)
	}
	return routeID
}

// waitFor polls until check succeeds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(timeout)
	for {
		if check() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("timed out waiting for the condition")
		}
	}
}

func TestFlushWritesMinutesWindowsAndPosition(t *testing.T) {
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	conn := testConn(t, databaseURL, schema)
	routeID := insertRoute(t, st, "shop@docker")
	clock := newTestClock(testBase.Add(30 * time.Second))
	aggregator := New(st, "accesslog", discardLogger(), clock)
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(5 * time.Second), Method: "GET", Path: "/api/orders/1",
		Status: 200, Bytes: 100, LatencyMS: 12, CountLatency: true,
	})
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(40 * time.Second), Method: "POST", Path: "/api/orders/2",
		Status: 404, Bytes: 50, LatencyMS: 1500, CountLatency: true,
	})
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(50 * time.Second), Method: "GET", Path: "/api/orders/1",
		Status: 503, NoBackend: true, Bytes: 10, LatencyMS: 250, CountLatency: true,
	})
	aggregator.SetPosition([]byte(`{"inode":7,"offset":1234}`))
	if err := aggregator.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	var requests, s2xx, s4xx, s5xx, noBackend, bytes int64
	var latency []int32
	err := conn.QueryRow(ctx, `select requests, s2xx, s4xx, s5xx, no_backend, bytes, latency
		from route_minute where route_id = $1 and ts = $2`, routeID, testBase).
		Scan(&requests, &s2xx, &s4xx, &s5xx, &noBackend, &bytes, &latency)
	if err != nil {
		t.Fatalf("read route_minute: %v", err)
	}
	if requests != 3 || s2xx != 1 || s4xx != 1 || s5xx != 1 || noBackend != 1 || bytes != 160 {
		t.Errorf("route_minute = %d requests, %d/%d/%d classes, %d without backend, %d bytes; want 3, 1/1/1, 1, 160",
			requests, s2xx, s4xx, s5xx, noBackend, bytes)
	}
	wantLatency := make([]int32, store.Buckets)
	wantLatency[2], wantLatency[8], wantLatency[13] = 1, 1, 1
	if !reflect.DeepEqual(latency, wantLatency) {
		t.Errorf("latency = %v, want %v", latency, wantLatency)
	}
	rows, err := conn.Query(ctx, `select method, pattern, requests, s4xx, s5xx, latency
		from path_window where route_id = $1 and ts = $2 order by method, pattern`, routeID, testBase)
	if err != nil {
		t.Fatalf("read path_window: %v", err)
	}
	defer rows.Close()
	type window struct {
		method, pattern      string
		requests, s4xx, s5xx int64
		latency              []int32
	}
	var windows []window
	for rows.Next() {
		var w window
		if err := rows.Scan(&w.method, &w.pattern, &w.requests, &w.s4xx, &w.s5xx, &w.latency); err != nil {
			t.Fatalf("scan path_window: %v", err)
		}
		windows = append(windows, w)
	}
	if len(windows) != 2 {
		t.Fatalf("path_window rows = %d, want 2", len(windows))
	}
	if windows[0].method != "GET" || windows[0].pattern != "/api/orders/:n" || windows[0].requests != 2 || windows[0].s5xx != 1 {
		t.Errorf("GET window = %+v, want the normalized pattern with two requests", windows[0])
	}
	if windows[1].method != "POST" || windows[1].pattern != NotFoundPattern || windows[1].requests != 1 || windows[1].s4xx != 1 {
		t.Errorf("POST window = %+v, want (not found) with one 4xx", windows[1])
	}
	var position []byte
	var status string
	err = conn.QueryRow(ctx, `select position, status from source_state where source = 'accesslog'`).Scan(&position, &status)
	if err != nil {
		t.Fatalf("read source_state: %v", err)
	}
	if string(position) != `{"inode": 7, "offset": 1234}` {
		t.Errorf("position = %s, want the written position", position)
	}
	if status != "ok" {
		t.Errorf("status = %q, want ok", status)
	}
}

func TestFlushIsAdditiveAcrossRestart(t *testing.T) {
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	conn := testConn(t, databaseURL, schema)
	routeID := insertRoute(t, st, "shop@docker")
	clock := newTestClock(testBase.Add(time.Minute))
	measurement := func(offset time.Duration) Measurement {
		return Measurement{
			RouteID: routeID, Time: testBase.Add(offset), Method: "GET", Path: "/api/orders/1",
			Status: 200, LatencyMS: 12, CountLatency: true,
		}
	}
	first := New(st, "accesslog", discardLogger(), clock)
	first.Add(measurement(5 * time.Second))
	first.Add(measurement(20 * time.Second))
	first.SetPosition([]byte(`{"inode":7,"offset":100}`))
	if err := first.Flush(ctx); err != nil {
		t.Fatalf("first Flush() error = %v", err)
	}
	// After a restart the source resumes at the stored position; a late line
	// for the same minute must add to the recorded row.
	second := New(st, "accesslog", discardLogger(), clock)
	second.Add(measurement(50 * time.Second))
	if err := second.Flush(ctx); err != nil {
		t.Fatalf("second Flush() error = %v", err)
	}
	// A flush without new content writes nothing.
	if err := second.Flush(ctx); err != nil {
		t.Fatalf("third Flush() error = %v", err)
	}
	var requests int64
	var latency []int32
	err := conn.QueryRow(ctx, `select requests, latency from route_minute where route_id = $1 and ts = $2`,
		routeID, testBase).Scan(&requests, &latency)
	if err != nil {
		t.Fatalf("read route_minute: %v", err)
	}
	if requests != 3 {
		t.Errorf("requests = %d, want 3", requests)
	}
	wantLatency := make([]int32, store.Buckets)
	wantLatency[2] = 3
	if !reflect.DeepEqual(latency, wantLatency) {
		t.Errorf("latency = %v, want %v", latency, wantLatency)
	}
	var windows int64
	if err := conn.QueryRow(ctx, `select sum(requests) from path_window where route_id = $1`, routeID).Scan(&windows); err != nil {
		t.Fatalf("read path_window: %v", err)
	}
	if windows != 3 {
		t.Errorf("window requests = %d, want 3", windows)
	}
}

func TestFlushPlacesLateLinesInTheOldestMinute(t *testing.T) {
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	conn := testConn(t, databaseURL, schema)
	routeID := insertRoute(t, st, "shop@docker")
	clock := newTestClock(testBase.Add(10 * time.Minute))
	aggregator := New(st, "accesslog", discardLogger(), clock)
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(9 * time.Minute), Method: "GET", Path: "/api", Status: 200,
	})
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(4 * time.Minute), Method: "GET", Path: "/api", Status: 200,
	})
	if err := aggregator.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	rows, err := conn.Query(ctx, `select ts, requests from route_minute where route_id = $1 order by ts`, routeID)
	if err != nil {
		t.Fatalf("read route_minute: %v", err)
	}
	defer rows.Close()
	var minutes []time.Time
	var requests []int64
	for rows.Next() {
		var ts time.Time
		var count int64
		if err := rows.Scan(&ts, &count); err != nil {
			t.Fatalf("scan route_minute: %v", err)
		}
		minutes = append(minutes, ts.UTC())
		requests = append(requests, count)
	}
	wantMinutes := []time.Time{testBase.Add(4 * time.Minute), testBase.Add(9 * time.Minute)}
	if !reflect.DeepEqual(minutes, wantMinutes) {
		t.Errorf("minutes = %v, want %v", minutes, wantMinutes)
	}
	if !reflect.DeepEqual(requests, []int64{1, 1}) {
		t.Errorf("requests = %v, want one per minute", requests)
	}
}

func TestFlushDropsOldestAndRecordsGap(t *testing.T) {
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	conn := testConn(t, databaseURL, schema)
	routeID := insertRoute(t, st, "shop@docker")
	broken, err := store.Open(ctx, withSearchPath(t, databaseURL, schema), discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	broken.Close()
	clock := newTestClock(testBase.Add(2 * time.Minute))
	aggregator := New(broken, "accesslog", discardLogger(), clock)
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase, Method: "GET", Path: "/api", Status: 200,
	})
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(time.Minute), Method: "GET", Path: "/api", Status: 200,
	})
	if err := aggregator.Flush(ctx); err == nil {
		t.Fatal("Flush() error = nil, want the closed pool error")
	}
	// The database stays unreachable for longer than the buffer; the buckets
	// are dropped and the gap is recorded on the next successful flush.
	clock.Advance(35 * time.Minute)
	aggregator.store = st
	if err := aggregator.Flush(ctx); err != nil {
		t.Fatalf("Flush() after the outage error = %v", err)
	}
	var minutes int64
	if err := conn.QueryRow(ctx, `select count(*) from route_minute`).Scan(&minutes); err != nil {
		t.Fatalf("count route_minute: %v", err)
	}
	if minutes != 0 {
		t.Errorf("route_minute rows = %d, want the dropped buckets to be gone", minutes)
	}
	var title string
	var severity, subject string
	err = conn.QueryRow(ctx, `select title, severity, subject from events where kind = 'gap'`).Scan(&title, &severity, &subject)
	if err != nil {
		t.Fatalf("read gap event: %v", err)
	}
	if title != "dropped 2 minutes of traffic" {
		t.Errorf("gap title = %q, want the dropped minutes", title)
	}
	if severity != "warning" || subject != "accesslog" {
		t.Errorf("gap event = %q, %q, want a warning for the access log", severity, subject)
	}
}

func TestRunFlushesAfterTheMinute(t *testing.T) {
	st, databaseURL, schema := testSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := testConn(t, databaseURL, schema)
	routeID := insertRoute(t, st, "shop@docker")
	clock := newTestClock(testBase.Add(10 * time.Second))
	aggregator := New(st, "accesslog", discardLogger(), clock)
	aggregator.Add(Measurement{
		RouteID: routeID, Time: testBase.Add(10 * time.Second), Method: "GET", Path: "/api", Status: 200,
	})
	done := make(chan error, 1)
	go func() { done <- aggregator.Run(ctx) }()
	waitFor(t, 5*time.Second, func() bool { return clock.pending() > 0 })
	clock.Advance(65 * time.Second) // 10:01:15, the flush of the minute that ended
	waitFor(t, 5*time.Second, func() bool {
		var count int64
		return conn.QueryRow(ctx, `select count(*) from route_minute`).Scan(&count) == nil && count == 1
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not stop")
	}
}
