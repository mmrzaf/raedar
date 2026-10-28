//go:build integration

package store

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// connect opens a single connection to the database.
func connect(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	return conn
}

// withDatabase returns the URL with its database replaced by name.
func withDatabase(t *testing.T, databaseURL, name string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURL, err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// newStore returns a store on a fresh schema that is dropped when the test
// ends.
func newStore(t *testing.T) *Store {
	t.Helper()
	databaseURL, schema := testdb.NewSchema(t, "store")
	st, err := Open(context.Background(), testdb.WithSearchPath(t, databaseURL, schema), discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// insertRoute adds an app and an HTTP route and returns the route ID.
func insertRoute(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	ctx := context.Background()
	var appID int64
	err := st.pool.QueryRow(ctx,
		`insert into apps (name, first_seen, last_seen) values ($1, now(), now()) returning id`,
		name+"-app").Scan(&appID)
	if err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var routeID int64
	err = st.pool.QueryRow(ctx,
		`insert into routes (name, app_id, kind, origin, first_seen, last_seen)
		 values ($1, $2, 'http', 'labels', now(), now()) returning id`,
		name, appID).Scan(&routeID)
	if err != nil {
		t.Fatalf("insert route: %v", err)
	}
	return routeID
}

// repeat returns a histogram of n buckets all holding value.
func repeat(n int, value int32) []int32 {
	buckets := make([]int32, n)
	for i := range buckets {
		buckets[i] = value
	}
	return buckets
}

// terminateBackend closes one backend of the test database.
func terminateBackend(t *testing.T, databaseURL string, pid int) {
	t.Helper()
	admin := connect(t, databaseURL)
	defer admin.Close(context.Background())
	if _, err := admin.Exec(context.Background(), "select pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}
}

// testMutexID serializes the tests that use the database-wide instance lock.
// It must match the value used by the serve integration test.
const testMutexID = 0x726165646172ffff

// lockInstanceTests waits until no other test process uses the instance lock
// and releases the mutex when the test ends.
func lockInstanceTests(t *testing.T, databaseURL string) {
	t.Helper()
	ctx := context.Background()
	conn := connect(t, databaseURL)
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", testMutexID); err != nil {
		conn.Close(ctx)
		t.Fatalf("lock the test mutex: %v", err)
	}
	t.Cleanup(func() {
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "select pg_advisory_unlock($1)", testMutexID); err != nil {
			t.Errorf("unlock the test mutex: %v", err)
		}
	})
}

func TestMigrateAppliesToEmptyDatabase(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	rows, err := st.pool.Query(ctx, "select version from schema_migrations order by version")
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("collect versions: %v", err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	want := make([]int, 0, len(migrations))
	for _, m := range migrations {
		want = append(want, m.version)
	}
	if !reflect.DeepEqual(versions, want) {
		t.Errorf("schema_migrations = %v, want every embedded migration %v", versions, want)
	}
	rows, err = st.pool.Query(ctx, `select table_name from information_schema.tables
		where table_schema = current_schema() order by table_name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect tables: %v", err)
	}
	wantTables := []string{
		"apps", "baselines", "certs", "containers", "deliveries", "disk_hour", "disk_minute",
		"docker_disk", "events", "host_hour", "host_minute", "incidents", "pages", "path_window", "peers",
		"route_hour", "route_minute", "routes", "schema_migrations", "silences",
		"slot_hour", "slot_minute", "slots", "source_state", "telegram_inbox", "units",
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Errorf("tables = %v\nwant   %v", tables, wantTables)
	}
	var dataType string
	err = st.pool.QueryRow(ctx, `select data_type from information_schema.columns
		where table_schema = current_schema() and table_name = 'route_minute' and column_name = 'latency'`).Scan(&dataType)
	if err != nil {
		t.Fatalf("query route_minute.latency: %v", err)
	}
	if dataType != "ARRAY" {
		t.Errorf("route_minute.latency data_type = %q, want ARRAY", dataType)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	var count int
	if err := st.pool.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if count != len(migrations) {
		t.Errorf("schema_migrations rows = %d, want one per migration (%d)", count, len(migrations))
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if _, err := st.pool.Exec(ctx, "insert into schema_migrations (version) values (999)"); err != nil {
		t.Fatalf("insert newer version: %v", err)
	}
	err := st.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "newer than this Raedar version") {
		t.Errorf("Migrate() error = %v, want a newer-schema error", err)
	}
}

func TestHistogramAddInSQL(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	first := repeat(21, 0)
	first[0], first[20] = 1, 5
	second := repeat(21, 0)
	second[0], second[10] = 2, 3
	tests := []struct {
		name string
		a    []int32
		b    []int32
		want []int32
	}{
		{name: "three buckets", a: []int32{1, 2, 3}, b: []int32{4, 5, 6}, want: []int32{5, 7, 9}},
		{name: "zeros", a: []int32{0, 0}, b: []int32{7, 8}, want: []int32{7, 8}},
		{name: "twenty one buckets", a: first, b: second, want: []int32{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []int32
			if err := st.pool.QueryRow(ctx, "select raedar_hist_add($1, $2)", tt.a, tt.b).Scan(&got); err != nil {
				t.Fatalf("raedar_hist_add: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("raedar_hist_add = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHistogramSumAggregate(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	routeID := insertRoute(t, st, "shop@docker")
	first := repeat(21, 0)
	first[0], first[20] = 1, 5
	second := repeat(21, 0)
	second[0], second[10] = 2, 3
	ts := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	_, err := st.pool.Exec(ctx,
		`insert into route_minute (route_id, ts, requests, latency) values ($1, $2, 1, $3), ($1, $4, 1, $5)`,
		routeID, ts, first, ts.Add(time.Minute), second)
	if err != nil {
		t.Fatalf("insert route_minute: %v", err)
	}
	var got []int32
	if err := st.pool.QueryRow(ctx, "select raedar_hist_sum(latency) from route_minute where route_id = $1", routeID).Scan(&got); err != nil {
		t.Fatalf("raedar_hist_sum: %v", err)
	}
	want := repeat(21, 0)
	want[0], want[10], want[20] = 3, 3, 5
	if !reflect.DeepEqual(got, want) {
		t.Errorf("raedar_hist_sum = %v, want %v", got, want)
	}
	// A route without minutes sums to an empty histogram, not to NULL.
	if err := st.pool.QueryRow(ctx, "select raedar_hist_sum(latency) from route_minute where route_id = $1", -1).Scan(&got); err != nil {
		t.Fatalf("raedar_hist_sum over no rows: %v", err)
	}
	if !reflect.DeepEqual(got, repeat(21, 0)) {
		t.Errorf("raedar_hist_sum over no rows = %v, want zeros", got)
	}
}

func TestInstanceLock(t *testing.T) {
	lockInstanceTests(t, testdb.URL(t))
	ctx := context.Background()
	first := newStore(t)
	second := newStore(t)
	if err := first.Lock(ctx); err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}
	if err := second.Lock(ctx); !errors.Is(err, ErrLockTaken) {
		t.Errorf("second Lock() error = %v, want ErrLockTaken", err)
	}
	first.Close()
	if err := second.Lock(ctx); err != nil {
		t.Errorf("Lock() after the first instance closed: %v", err)
	}
}

func TestKeepLockReacquiresAfterDisconnect(t *testing.T) {
	lockInstanceTests(t, testdb.URL(t))
	ctx := context.Background()
	st := newStore(t)
	if err := st.Lock(ctx); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	var pid int
	if err := st.lockConn.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	terminateBackend(t, testdb.URL(t), pid)
	if err := st.KeepLock(ctx); err != nil {
		t.Fatalf("KeepLock() error = %v, want the lock taken again", err)
	}
	other := newStore(t)
	if err := other.Lock(ctx); !errors.Is(err, ErrLockTaken) {
		t.Errorf("other Lock() error = %v, want ErrLockTaken", err)
	}
}

func TestKeepLockReportsLockTakenByAnotherInstance(t *testing.T) {
	lockInstanceTests(t, testdb.URL(t))
	ctx := context.Background()
	databaseURL := testdb.URL(t)
	st := newStore(t)
	if err := st.Lock(ctx); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	var pid int
	if err := st.lockConn.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	terminateBackend(t, databaseURL, pid)
	admin := connect(t, databaseURL)
	defer admin.Close(ctx)
	var taken bool
	if err := admin.QueryRow(ctx, "select pg_try_advisory_lock($1)", instanceLockID).Scan(&taken); err != nil || !taken {
		t.Fatalf("take the lock from another instance: taken = %v, err = %v", taken, err)
	}
	defer admin.Exec(ctx, "select pg_advisory_unlock($1)", instanceLockID)
	if err := st.KeepLock(ctx); !errors.Is(err, ErrLockTaken) {
		t.Errorf("KeepLock() error = %v, want ErrLockTaken", err)
	}
}

func TestOpenFailsWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Open(ctx, "postgres://raedar@127.0.0.1:1/raedar?sslmode=disable", discardLogger())
	if err == nil {
		t.Fatal("Open() error = nil, want an error")
	}
}

func TestOpenStopsWhenDatabaseIsMissing(t *testing.T) {
	databaseURL := withDatabase(t, testdb.URL(t), "raedar_missing_database")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := Open(ctx, databaseURL, discardLogger())
	if err == nil {
		t.Fatal("Open() error = nil, want an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Open() took %v, want a fast failure", elapsed)
	}
}

func TestLatestEventByApp(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	var shop, blog int64
	for name, id := range map[string]*int64{"shop": &shop, "blog": &blog} {
		if err := st.pool.QueryRow(ctx,
			`insert into apps (name, first_seen, last_seen) values ($1, now(), now()) returning id`, name).Scan(id); err != nil {
			t.Fatalf("insert app: %v", err)
		}
	}
	base := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	add := func(app *int64, kind EventKind, at time.Time) {
		t.Helper()
		if _, err := st.AddEvent(ctx, Event{TS: at, Kind: kind, Severity: "info", AppID: app, Subject: "x", Title: string(kind), Data: []byte("{}")}); err != nil {
			t.Fatalf("AddEvent() error = %v", err)
		}
	}
	add(&shop, EventDeploy, base)
	add(&shop, EventRecreated, base.Add(48*time.Hour))
	add(&shop, EventCrashed, base.Add(96*time.Hour))
	add(nil, EventDeploy, base.Add(120*time.Hour))
	latest, err := st.LatestEventByApp(ctx, EventDeploy, EventRecreated)
	if err != nil {
		t.Fatalf("LatestEventByApp() error = %v", err)
	}
	if got := latest[shop]; !got.Equal(base.Add(48 * time.Hour)) {
		t.Errorf("latest[shop] = %v, want the recreate at %v", got, base.Add(48*time.Hour))
	}
	if _, ok := latest[blog]; ok {
		t.Errorf("latest has the blog app, which has no matching event: %v", latest)
	}
	if len(latest) != 1 {
		t.Errorf("latest = %v, want only the shop app", latest)
	}
}

func TestAddDeliveryAcceptsEveryKind(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	for _, kind := range []DeliveryKind{DeliveryOpened, DeliveryEscalated, DeliveryResolved, DeliveryDigest, DeliveryTest} {
		_, err := st.AddDelivery(ctx, Delivery{Kind: kind, Channel: "telegram", CreatedAt: time.Now(), Status: "pending"})
		if err != nil {
			t.Errorf("AddDelivery(%s) error = %v", kind, err)
		}
	}
}

func TestAppStatsAcrossApps(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	shop := insertRoute(t, st, "shop@docker")
	blog := insertRoute(t, st, "blog@docker")
	var shopApp, blogApp int64
	for _, row := range []struct {
		route int64
		app   *int64
	}{{shop, &shopApp}, {blog, &blogApp}} {
		if err := st.pool.QueryRow(ctx, `select app_id from routes where id = $1`, row.route).Scan(row.app); err != nil {
			t.Fatalf("read app of route: %v", err)
		}
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	for _, minute := range []struct {
		route    int64
		at       time.Time
		requests int64
		s5xx     int64
	}{
		{shop, base, 10, 1}, {shop, base.Add(time.Minute), 20, 2}, {blog, base, 5, 0},
		{shop, base.Add(3 * time.Hour), 99, 9}, // outside the range
	} {
		if err := st.FlushBuckets(ctx, Flush{Minutes: []RouteMinute{{
			RouteID: minute.route, TS: minute.at, Requests: minute.requests, S5xx: minute.s5xx, Latency: make([]int32, Buckets),
		}}}); err != nil {
			t.Fatalf("FlushBuckets() error = %v", err)
		}
	}
	stats, err := st.AppMinuteStats(ctx, base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("AppMinuteStats() error = %v", err)
	}
	if len(stats) != 2 || stats[shopApp].Requests != 30 || stats[shopApp].S5xx != 3 || stats[blogApp].Requests != 5 {
		t.Errorf("stats = %+v, want 30 requests for shop, 5 for blog", stats)
	}
	for _, hour := range []struct {
		route int64
		at    time.Time
		count int64
	}{{shop, base, 7}, {shop, base.Add(time.Hour), 8}, {blog, base, 1}} {
		if _, err := st.pool.Exec(ctx, `insert into route_hour (route_id, ts, requests) values ($1, $2, $3)`, hour.route, hour.at, hour.count); err != nil {
			t.Fatalf("insert route_hour: %v", err)
		}
	}
	hourly, err := st.AppHourlyRequests(ctx, base)
	if err != nil {
		t.Fatalf("AppHourlyRequests() error = %v", err)
	}
	if hourly[shopApp][base] != 7 || hourly[shopApp][base.Add(time.Hour)] != 8 || hourly[blogApp][base] != 1 {
		t.Errorf("hourly = %v, want the seeded hours per app", hourly)
	}
	if _, ok := hourly[shopApp][base.Add(-time.Hour)]; ok {
		t.Error("an hour before from was returned")
	}
}

func TestLatestEvents(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := st.AddEvent(ctx, Event{TS: base.Add(time.Duration(i) * time.Minute), Kind: EventStarted, Severity: SeverityInfo,
			Subject: "x", Title: "t", Data: []byte("{}")}); err != nil {
			t.Fatalf("AddEvent() error = %v", err)
		}
	}
	events, err := st.LatestEvents(ctx, 3)
	if err != nil {
		t.Fatalf("LatestEvents() error = %v", err)
	}
	if len(events) != 3 || !events[0].TS.Equal(base.Add(4*time.Minute)) || !events[2].TS.Equal(base.Add(2*time.Minute)) {
		t.Errorf("events = %+v, want the newest three, newest first", events)
	}
}

// TestAddDeliveryAcceptsEveryGateStatus verifies that the status constraint
// allows each disposition the paging gate records.
func TestAddDeliveryAcceptsEveryGateStatus(t *testing.T) {
	st := newStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, status := range []string{"pending", "sent", "failed", "silenced", "acked", "held", "cooldown"} {
		if _, err := st.AddDelivery(context.Background(), Delivery{
			Kind: DeliveryOpened, Channel: "telegram", CreatedAt: time.Now(), Status: status,
		}); err != nil {
			t.Errorf("status %q rejected: %v", status, err)
		}
	}
}

// TestFailPendingDeliveries verifies that only pending rows are failed and
// that an earlier error is kept.
func TestFailPendingDeliveries(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	earlier := "timeout"
	pending, _ := st.AddDelivery(ctx, Delivery{Kind: DeliveryDigest, Channel: "telegram", CreatedAt: time.Now(), Status: "pending", Error: &earlier})
	sent, _ := st.AddDelivery(ctx, Delivery{Kind: DeliveryDigest, Channel: "telegram", CreatedAt: time.Now(), Status: "sent"})
	count, err := st.FailPendingDeliveries(ctx, "interrupted by a restart")
	if err != nil || count != 1 {
		t.Fatalf("FailPendingDeliveries = %d, %v; want 1", count, err)
	}
	var status, message string
	if err := st.pool.QueryRow(ctx, `select status, coalesce(error, '') from deliveries where id = $1`, pending).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || message != "timeout; interrupted by a restart" {
		t.Fatalf("pending row = %q %q", status, message)
	}
	if err := st.pool.QueryRow(ctx, `select status from deliveries where id = $1`, sent).Scan(&status); err != nil || status != "sent" {
		t.Fatalf("sent row = %q, %v; want untouched", status, err)
	}
}
