//go:build integration

package rules

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// testClock is a clock the tests set.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

// fakeModel returns a fixed model.
type fakeModel struct{ model *discovery.Model }

func (m *fakeModel) Model() *discovery.Model { return m.model }

// fakeNotifier records the incident changes and database reports.
type fakeNotifier struct {
	mu           sync.Mutex
	changes      []Change
	databaseDown int
	databaseUp   int
}

func (n *fakeNotifier) IncidentsChanged(ctx context.Context, changes []Change) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.changes = append(n.changes, changes...)
}

// DatabaseDown records a database outage report.
func (n *fakeNotifier) DatabaseDown(ctx context.Context, since time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.databaseDown++
}

// DatabaseUp records a database recovery report.
func (n *fakeNotifier) DatabaseUp(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.databaseUp++
}

// take returns the changes recorded since the last call.
func (n *fakeNotifier) take() []Change {
	n.mu.Lock()
	defer n.mu.Unlock()
	changes := n.changes
	n.changes = nil
	return changes
}

// testSchema creates a schema and returns a migrated store, the database URL
// and the schema name.
func testSchema(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	databaseURL, schema := testdb.NewSchema(t, "rules")
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

// testConn returns a connection to the test schema.
func testConn(t *testing.T, databaseURL, schema string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testdb.WithSearchPath(t, databaseURL, schema))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig returns the configuration with the documented defaults.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(func(name string) (string, bool) {
		if name == "RAEDAR_DATABASE_URL" {
			return "postgres://raedar:raedar@postgres:5432/raedar?sslmode=disable", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg
}

// testEnv is one test's store, model, clock and engine.
type testEnv struct {
	store    *store.Store
	conn     *pgx.Conn
	cfg      *config.Config
	clock    *testClock
	notifier *fakeNotifier
	engine   *Engine
	start    time.Time

	appID     int64
	routeID   int64
	slotID    int64
	app       *discovery.App
	route     *discovery.Route
	slot      *discovery.Slot
	container *discovery.Container
	model     *discovery.Model
}

// newTestEnv builds an environment with one app, route and slot.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 18, 10, 0, 15, 0, time.UTC)
	clock := &testClock{now: start}
	appID, err := st.UpsertApp(ctx, "shop", start)
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	routeID, err := st.UpsertRoute(ctx, store.Route{
		Name: "shop@docker", AppID: &appID, Kind: "http", Origin: "labels",
		Domains: []string{"shop.example.com"}, TLS: true,
	}, start)
	if err != nil {
		t.Fatalf("UpsertRoute() error = %v", err)
	}
	slotID, err := st.UpsertSlot(ctx, store.Slot{AppID: appID, Key: "api #1", Service: "api", Replica: 1}, start)
	if err != nil {
		t.Fatalf("UpsertSlot() error = %v", err)
	}
	env := &testEnv{
		store:   st,
		conn:    testConn(t, databaseURL, schema),
		cfg:     testConfig(t),
		clock:   clock,
		start:   start,
		appID:   appID,
		routeID: routeID,
		slotID:  slotID,
	}
	env.container = &discovery.Container{ID: "c1", State: "running", Health: "healthy", Slot: nil}
	env.app = &discovery.App{
		ID: appID, Name: "shop", Alerts: true,
		Slots: map[string]*discovery.Slot{}, Routes: map[string]*discovery.Route{},
	}
	env.slot = &discovery.Slot{ID: slotID, App: env.app, Key: "api #1", Service: "api", Replica: 1, Container: env.container}
	env.app.Slots["api #1"] = env.slot
	env.route = &discovery.Route{ID: routeID, Name: "shop@docker", App: env.app, Kind: "http",
		Domains: []string{"shop.example.com"}, TLS: true}
	env.app.Routes["shop@docker"] = env.route
	env.container.App = env.app
	env.container.Slot = env.slot
	env.model = &discovery.Model{
		Apps:       map[string]*discovery.App{"shop": env.app},
		Routes:     map[string]*discovery.Route{"shop@docker": env.route},
		Containers: map[string]*discovery.Container{"c1": env.container},
	}
	env.notifier = &fakeNotifier{}
	env.engine = New(st, &fakeModel{model: env.model}, nil, env.cfg, discardLogger(), clock, env.notifier)
	return env
}

// evaluate runs one evaluation at the given offset from the start.
func (env *testEnv) evaluate(t *testing.T, offset time.Duration) {
	t.Helper()
	env.clock.now = env.start.Add(offset)
	if err := env.engine.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
}

// openIncident returns the open incident of a rule and subject, or nil.
func (env *testEnv) openIncident(t *testing.T, rule, subject string) *store.Incident {
	t.Helper()
	open, err := env.store.OpenIncidents(context.Background())
	if err != nil {
		t.Fatalf("OpenIncidents() error = %v", err)
	}
	incident, ok := open[store.IncidentKey{Rule: rule, Subject: subject}]
	if !ok {
		return nil
	}
	return &incident
}

// countIncidents returns how many incidents of a rule exist, open or not.
func (env *testEnv) countIncidents(t *testing.T, rule string) int {
	t.Helper()
	var count int
	if err := env.conn.QueryRow(context.Background(), `select count(*) from incidents where rule = $1`, rule).Scan(&count); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	return count
}

// seedRouteWindow writes one minute of route data at the start of a window.
func (env *testEnv) seedRouteWindow(t *testing.T, window time.Duration, requests, errors, noBackend int64, latencies []float64) {
	t.Helper()
	var histogram store.Histogram
	for _, latency := range latencies {
		histogram.Add(latency)
	}
	at := env.clock.now.Truncate(time.Minute).Add(-time.Minute)
	if err := env.store.FlushBuckets(context.Background(), store.Flush{
		Minutes: []store.RouteMinute{{
			RouteID: env.routeID, TS: at, Requests: requests, S5xx: errors,
			S2xx: requests - errors, NoBackend: noBackend, Latency: histogram.Counts(),
		}},
		Windows: []store.PathWindow{{
			RouteID: env.routeID, TS: at, Method: "POST", Pattern: "/api/checkout",
			Requests: requests, S5xx: errors, Latency: histogram.Counts(),
		}},
	}); err != nil {
		t.Fatalf("FlushBuckets() error = %v", err)
	}
	_ = window
}

// setBaseline writes the baseline of the route's current local hour.
func (env *testEnv) setBaseline(t *testing.T, errorRate, p95 *float64, days int) {
	t.Helper()
	hour := env.clock.now.In(env.cfg.Location).Hour()
	if _, err := env.conn.Exec(context.Background(), `
		insert into baselines (route_id, hour_of_day, error_rate, p95_ms, days_used, computed_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (route_id, hour_of_day) do update set
			error_rate = excluded.error_rate, p95_ms = excluded.p95_ms, days_used = excluded.days_used`,
		env.routeID, hour, errorRate, p95, days, env.clock.now); err != nil {
		t.Fatalf("insert baseline: %v", err)
	}
}

// seedCrash records one crashed event for the slot.
func (env *testEnv) seedCrash(t *testing.T, at time.Time, exitCode int, oom bool) {
	t.Helper()
	slotID := env.slotID
	data, err := json.Marshal(map[string]any{"exit_code": exitCode, "oom": oom})
	if err != nil {
		t.Fatalf("encode crash: %v", err)
	}
	if _, err := env.store.AddEvent(context.Background(), store.Event{
		TS: at, Kind: "crashed", Severity: "warning", SlotID: &slotID, AppID: &env.appID,
		Subject: "api #1", Title: "crashed", Data: data,
	}); err != nil {
		t.Fatalf("add crash event: %v", err)
	}
}

// seedDisk writes disk samples ending at the given offset.
func (env *testEnv) seedDisk(t *testing.T, name string, samples int, used, size int64, inodesUsed, inodes int64) {
	t.Helper()
	for i := 0; i < samples; i++ {
		ts := env.clock.now.Truncate(time.Minute).Add(-time.Duration(i) * time.Minute)
		if err := env.store.InsertDiskMinute(context.Background(), store.DiskMinute{
			Disk: name, TS: ts, SizeBytes: size, UsedBytes: used, AvailBytes: size - used,
			Inodes: inodes, InodesUsed: inodesUsed,
		}); err != nil {
			t.Fatalf("InsertDiskMinute() error = %v", err)
		}
	}
}

// changes returns the notifier changes recorded so far.
func (env *testEnv) changes() []Change {
	return env.notifier.take()
}

// incidentPayload decodes the details of an incident.
func incidentPayload(t *testing.T, incident *store.Incident) map[string]any {
	t.Helper()
	payload := map[string]any{}
	if err := json.Unmarshal(incident.Details, &payload); err != nil {
		t.Fatalf("decode incident details: %v", err)
	}
	return payload
}

func TestRouteErrorsRule(t *testing.T) {
	env := newTestEnv(t)
	// A tenth of the requests fail: above the 2 % minimum but below 3x a 5 %
	// baseline.
	env.clock.now = env.start
	env.seedRouteWindow(t, 5*time.Minute, 100, 10, 0, nil)
	baseline := 0.05
	env.setBaseline(t, &baseline, nil, 5)
	for minute := 0; minute <= 2; minute++ {
		env.clock.now = env.start.Add(time.Duration(minute) * time.Minute)
		env.seedRouteWindow(t, 5*time.Minute, 100, 10, 0, nil)
		if err := env.engine.Evaluate(context.Background()); err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
	}
	if incident := env.openIncident(t, RuleRouteErrors, "shop@docker"); incident != nil {
		t.Fatal("incident opened although the rate is below the baseline factor")
	}
	// With a 1 % baseline, 10 % is more than three times the baseline.
	baseline = 0.01
	env.setBaseline(t, &baseline, nil, 5)
	env.clock.now = env.start.Add(3 * time.Minute)
	env.seedRouteWindow(t, 5*time.Minute, 100, 10, 0, nil)
	if err := env.engine.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if incident := env.openIncident(t, RuleRouteErrors, "shop@docker"); incident != nil {
		t.Fatal("incident opened before the for duration")
	}
	env.clock.now = env.start.Add(5 * time.Minute)
	env.seedRouteWindow(t, 5*time.Minute, 100, 10, 0, nil)
	if err := env.engine.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	incident := env.openIncident(t, RuleRouteErrors, "shop@docker")
	if incident == nil {
		t.Fatal("incident did not open after two minutes")
	}
	if incident.Severity != "warning" || incident.Summary != "5xx errors" {
		t.Errorf("incident = %+v, want a warning 5xx incident", incident)
	}
	payload := incidentPayload(t, incident)
	if payload["path"] != "/api/checkout" {
		t.Errorf("details path = %v, want the path with the most errors", payload["path"])
	}
	changes := env.changes()
	if len(changes) != 1 || changes[0].Kind != "opened" {
		t.Fatalf("changes = %+v, want one opened change", changes)
	}
	// The incident resolves once the condition has been false for the
	// configured delay.
	env.evaluate(t, 10*time.Minute)
	env.evaluate(t, 15*time.Minute)
	if incident := env.openIncident(t, RuleRouteErrors, "shop@docker"); incident != nil {
		t.Fatal("incident did not resolve after the delay")
	}
	changes = env.changes()
	if len(changes) != 1 || changes[0].Kind != "resolved" {
		t.Fatalf("changes = %+v, want one resolved change", changes)
	}
}

func TestRouteErrorsBelowThresholds(t *testing.T) {
	tests := []struct {
		name     string
		requests int64
		errors   int64
	}{
		{name: "rate below the minimum", requests: 100, errors: 1},
		{name: "too few requests", requests: 10, errors: 10},
		{name: "fewer than five errors", requests: 100, errors: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.seedRouteWindow(t, 5*time.Minute, tt.requests, tt.errors, 0, nil)
			env.evaluate(t, 0)
			env.evaluate(t, 5*time.Minute)
			if incident := env.openIncident(t, RuleRouteErrors, "shop@docker"); incident != nil {
				t.Fatalf("incident opened for requests=%d errors=%d", tt.requests, tt.errors)
			}
		})
	}
}

func TestRouteErrorsWithoutBaseline(t *testing.T) {
	env := newTestEnv(t)
	for minute := 0; minute <= 2; minute++ {
		env.clock.now = env.start.Add(time.Duration(minute) * time.Minute)
		env.seedRouteWindow(t, 5*time.Minute, 100, 6, 0, nil)
		if err := env.engine.Evaluate(context.Background()); err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
	}
	if incident := env.openIncident(t, RuleRouteErrors, "shop@docker"); incident == nil {
		t.Fatal("incident did not open with only the fixed minimum")
	}
}

func TestRouteSlowRule(t *testing.T) {
	env := newTestEnv(t)
	// 100 requests in the 300-400 ms bucket: p95 is just below 400 ms.
	latencies := make([]float64, 100)
	for i := range latencies {
		latencies[i] = 350
	}
	env.seedRouteWindow(t, 10*time.Minute, 100, 0, 0, latencies)
	baseline := 500.0
	env.setBaseline(t, nil, &baseline, 5)
	for minute := 0; minute <= 5; minute++ {
		env.clock.now = env.start.Add(time.Duration(minute) * time.Minute)
		env.seedRouteWindow(t, 10*time.Minute, 100, 0, 0, latencies)
		if err := env.engine.Evaluate(context.Background()); err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
	}
	if incident := env.openIncident(t, RuleRouteSlow, "shop@docker"); incident != nil {
		t.Fatal("incident opened although p95 is below twice the baseline")
	}
	baseline = 100
	env.setBaseline(t, nil, &baseline, 5)
	env.clock.now = env.start.Add(6 * time.Minute)
	env.seedRouteWindow(t, 10*time.Minute, 100, 0, 0, latencies)
	if err := env.engine.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if incident := env.openIncident(t, RuleRouteSlow, "shop@docker"); incident != nil {
		t.Fatal("incident opened before the for duration")
	}
	env.clock.now = env.start.Add(11 * time.Minute)
	env.seedRouteWindow(t, 10*time.Minute, 100, 0, 0, latencies)
	if err := env.engine.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	incident := env.openIncident(t, RuleRouteSlow, "shop@docker")
	if incident == nil {
		t.Fatal("incident did not open after five minutes")
	}
	if incident.Summary != "slow responses" {
		t.Errorf("summary = %q, want slow responses", incident.Summary)
	}
	if incidentPayload(t, incident)["path"] != "/api/checkout" {
		t.Error("details do not name the slowest path")
	}
}

func TestRouteDownRule(t *testing.T) {
	env := newTestEnv(t)
	env.seedRouteWindow(t, 2*time.Minute, 10, 6, 6, nil)
	env.evaluate(t, 0)
	env.seedRouteWindow(t, 2*time.Minute, 10, 6, 6, nil)
	incident := env.openIncident(t, RuleRouteDown, "shop@docker")
	if incident == nil {
		t.Fatal("incident did not open for requests without a backend")
	}
	if incident.Severity != "critical" {
		t.Errorf("severity = %q, want critical", incident.Severity)
	}
	// It stays quiet for route.errors on the same route.
	env.evaluate(t, 5*time.Minute)
	if errors := env.openIncident(t, RuleRouteErrors, "shop@docker"); errors != nil {
		t.Error("route.errors opened while route.down is open")
	}
	env.evaluate(t, 10*time.Minute)
	if down := env.openIncident(t, RuleRouteDown, "shop@docker"); down != nil {
		t.Error("route.down did not resolve")
	}
}

func TestContainerCrashedRule(t *testing.T) {
	env := newTestEnv(t)
	env.seedCrash(t, env.start.Add(-time.Minute), 1, false)
	env.evaluate(t, 0)
	if env.countIncidents(t, RuleContainerCrashed) != 1 {
		t.Fatalf("crashed incidents = %d, want 1", env.countIncidents(t, RuleContainerCrashed))
	}
	incident := env.openIncident(t, RuleContainerCrashed, "shop / api #1")
	if incident != nil {
		t.Error("one-shot incident is still open")
	}
	var oneShot bool
	var resolved *time.Time
	if err := env.conn.QueryRow(context.Background(),
		`select one_shot, resolved_at from incidents where rule = $1`, RuleContainerCrashed).Scan(&oneShot, &resolved); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if !oneShot || resolved == nil {
		t.Error("crash incident is not a resolved one-shot")
	}
	changes := env.changes()
	if len(changes) != 1 || changes[0].Kind != "opened" {
		t.Fatalf("changes = %+v, want one opened change", changes)
	}
	// The same crash is not reported again.
	env.evaluate(t, time.Minute)
	if env.countIncidents(t, RuleContainerCrashed) != 1 {
		t.Error("the crash was reported twice")
	}
}

func TestContainerRestartingRule(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 3; i++ {
		env.seedCrash(t, env.start.Add(-time.Duration(3-i)*time.Minute), 1, false)
	}
	env.evaluate(t, 0)
	incident := env.openIncident(t, RuleContainerRestarting, "shop / api #1")
	if incident == nil {
		t.Fatal("restarting incident did not open after three crashes")
	}
	if incident.Severity != "critical" {
		t.Errorf("severity = %q, want critical", incident.Severity)
	}
	// The crash of the same evaluation stays quiet.
	if env.countIncidents(t, RuleContainerCrashed) != 0 {
		t.Error("crashed incident opened while restarting is open")
	}
	// It resolves after ten minutes without a crash.
	env.evaluate(t, 5*time.Minute)
	if env.openIncident(t, RuleContainerRestarting, "shop / api #1") == nil {
		t.Error("restarting resolved too early")
	}
	env.evaluate(t, 11*time.Minute)
	env.evaluate(t, 21*time.Minute)
	if env.openIncident(t, RuleContainerRestarting, "shop / api #1") != nil {
		t.Error("restarting did not resolve after ten minutes without a crash")
	}
}

func TestContainerStoppedRule(t *testing.T) {
	env := newTestEnv(t)
	env.container.State = "exited"
	env.seedCrash(t, env.start.Add(-time.Minute), 2, false)
	env.evaluate(t, 0)
	if env.openIncident(t, RuleContainerStopped, "shop / api #1") != nil {
		t.Error("stopped incident opened before one minute")
	}
	env.evaluate(t, time.Minute)
	if env.openIncident(t, RuleContainerStopped, "shop / api #1") == nil {
		t.Error("stopped incident did not open after one minute")
	}
	env.container.State = "running"
	env.evaluate(t, 2*time.Minute)
	if env.openIncident(t, RuleContainerStopped, "shop / api #1") != nil {
		t.Error("stopped incident did not resolve when the slot runs again")
	}
}

func TestContainerUnhealthyRule(t *testing.T) {
	env := newTestEnv(t)
	env.container.Health = "unhealthy"
	env.evaluate(t, 0)
	env.evaluate(t, time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") != nil {
		t.Error("unhealthy incident opened before two minutes")
	}
	env.evaluate(t, 2*time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") == nil {
		t.Error("unhealthy incident did not open after two minutes")
	}
	env.container.Health = "healthy"
	env.evaluate(t, 3*time.Minute)
	env.evaluate(t, 8*time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") != nil {
		t.Error("unhealthy incident did not resolve")
	}
}

func TestContainerMemoryRule(t *testing.T) {
	env := newTestEnv(t)
	limit := int64(1000)
	sample := func() {
		if err := env.store.InsertSlotMinute(context.Background(), store.SlotMinute{
			SlotID: env.slotID, TS: env.clock.now.Truncate(time.Minute), MemBytes: 950, MemLimitBytes: &limit, Running: true,
		}); err != nil {
			t.Fatalf("InsertSlotMinute() error = %v", err)
		}
	}
	for minute := 0; minute <= 10; minute++ {
		env.clock.now = env.start.Add(time.Duration(minute) * time.Minute)
		sample()
		if err := env.engine.Evaluate(context.Background()); err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
	}
	env.clock.now = env.start.Add(5 * time.Minute)
	_ = sample
	incident := env.openIncident(t, RuleContainerMemory, "shop / api #1")
	if incident == nil {
		t.Fatal("memory incident did not open after ten minutes")
	}
	if incident.Severity != "warning" {
		t.Errorf("severity = %q, want warning", incident.Severity)
	}
}

func TestCriticalAppEscalatesAvailabilityRules(t *testing.T) {
	env := newTestEnv(t)
	env.app.Critical = true
	env.seedRouteWindow(t, 2*time.Minute, 10, 0, 10, nil)
	env.evaluate(t, 0)
	incident := env.openIncident(t, RuleRouteDown, "shop@docker")
	if incident == nil || incident.Severity != "critical" {
		t.Fatalf("incident = %+v, want a critical route.down", incident)
	}
	env.app.Critical = false
	env.evaluate(t, 6*time.Minute)
	env.evaluate(t, 11*time.Minute)
	if env.openIncident(t, RuleRouteDown, "shop@docker") != nil {
		t.Fatal("route.down did not resolve")
	}
	env.seedRouteWindow(t, 5*time.Minute, 100, 10, 0, nil)
	env.evaluate(t, 12*time.Minute)
	env.evaluate(t, 14*time.Minute)
	errors := env.openIncident(t, RuleRouteErrors, "shop@docker")
	if errors == nil || errors.Severity != "warning" {
		t.Fatalf("errors incident = %+v, want a warning", errors)
	}
}

func TestAlertsDisabledAppRecordsQuietly(t *testing.T) {
	env := newTestEnv(t)
	env.app.Alerts = false
	env.seedRouteWindow(t, 2*time.Minute, 10, 0, 10, nil)
	env.evaluate(t, 0)
	if env.openIncident(t, RuleRouteDown, "shop@docker") == nil {
		t.Fatal("incident was not recorded")
	}
	if changes := env.changes(); len(changes) != 0 {
		t.Errorf("changes = %+v, want none for an app without alerts", changes)
	}
}

func TestIncidentsResolveAfterRestart(t *testing.T) {
	env := newTestEnv(t)
	env.seedRouteWindow(t, 2*time.Minute, 10, 0, 10, nil)
	env.evaluate(t, 0)
	if env.openIncident(t, RuleRouteDown, "shop@docker") == nil {
		t.Fatal("incident was not opened")
	}
	// A new engine instance sees the open incident in the store.
	restarted := New(env.store, &fakeModel{model: env.model}, nil, env.cfg, discardLogger(), env.clock, env.notifier)
	env.clock.now = env.start.Add(6 * time.Minute)
	if err := restarted.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	env.clock.now = env.start.Add(11 * time.Minute)
	if err := restarted.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if env.openIncident(t, RuleRouteDown, "shop@docker") != nil {
		t.Error("the incident did not resolve after the restart")
	}
}

func TestReopenWithinThirtyMinutes(t *testing.T) {
	env := newTestEnv(t)
	// A previously resolved incident of the same rule and subject.
	resolvedAt := env.start.Add(-10 * time.Minute)
	id, err := env.store.InsertIncident(context.Background(), store.Incident{
		Rule: RuleRouteDown, Subject: "shop@docker", Severity: "critical", OpenedAt: env.start.Add(-20 * time.Minute),
		ResolvedAt: &resolvedAt, Summary: "no backend for requests", Details: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("InsertIncident() error = %v", err)
	}
	env.seedRouteWindow(t, 2*time.Minute, 10, 0, 10, nil)
	env.evaluate(t, 0)
	incident := env.openIncident(t, RuleRouteDown, "shop@docker")
	if incident == nil {
		t.Fatal("incident did not open")
	}
	if incident.ID != id {
		t.Errorf("incident ID = %d, want the resolved incident %d to be reopened", incident.ID, id)
	}
	changes := env.changes()
	if len(changes) != 1 || !changes[0].Reopened {
		t.Fatalf("changes = %+v, want a reopened change", changes)
	}
	if payload := incidentPayload(t, incident); payload["reopened"] != true {
		t.Errorf("details = %v, want reopened", payload)
	}
}

func TestHostDiskRules(t *testing.T) {
	env := newTestEnv(t)
	// 90 % used: warning after two samples.
	env.seedDisk(t, "/", 2, 90, 100, 1, 100)
	env.evaluate(t, 0)
	if env.openIncident(t, RuleHostDisk, "/") != nil {
		t.Error("disk incident opened before two samples")
	}
	env.evaluate(t, time.Minute)
	incident := env.openIncident(t, RuleHostDisk, "/")
	if incident == nil {
		t.Fatal("disk incident did not open after two samples")
	}
	if incident.Severity != "warning" {
		t.Errorf("severity = %q, want warning", incident.Severity)
	}
	env.changes()
	// 96 % used escalates the same incident.
	env.seedDisk(t, "/", 2, 96, 100, 1, 100)
	env.evaluate(t, 2*time.Minute)
	incident = env.openIncident(t, RuleHostDisk, "/")
	if incident == nil || incident.Severity != "critical" {
		t.Fatalf("incident = %+v, want critical after crossing the second threshold", incident)
	}
	changes := env.changes()
	if len(changes) != 1 || changes[0].Kind != "escalated" {
		t.Fatalf("changes = %+v, want one escalated change", changes)
	}
	// Usage two points below the warning threshold resolves it.
	env.seedDisk(t, "/", 2, 82, 100, 1, 100)
	env.evaluate(t, 3*time.Minute)
	if env.openIncident(t, RuleHostDisk, "/") != nil {
		t.Error("disk incident did not resolve below the hysteresis")
	}
}

func TestHostInodesRule(t *testing.T) {
	env := newTestEnv(t)
	env.seedDisk(t, "/", 2, 10, 1000, 96, 100)
	env.evaluate(t, 0)
	env.evaluate(t, time.Minute)
	incident := env.openIncident(t, RuleHostInodes, "/")
	if incident == nil || incident.Severity != "critical" {
		t.Fatalf("incident = %+v, want a critical inode incident", incident)
	}
}

func TestHostDiskForecastRule(t *testing.T) {
	env := newTestEnv(t)
	now := env.clock.now.Truncate(time.Minute)
	seedGrowth := func(step int64) {
		for hour := 24; hour >= 0; hour-- {
			used := int64(50_000_000_000) + int64(24-hour)*step
			if err := env.store.InsertDiskMinute(context.Background(), store.DiskMinute{
				Disk: "/", TS: now.Add(-time.Duration(hour) * time.Hour), SizeBytes: 100_000_000_000,
				UsedBytes: used, AvailBytes: 100_000_000_000 - used, Inodes: 100, InodesUsed: 1,
			}); err != nil {
				t.Fatalf("InsertDiskMinute() error = %v", err)
			}
		}
	}
	// About 2.4 GB a day: the filesystem fills after the three-day horizon.
	seedGrowth(100_000_000)
	env.evaluate(t, 0)
	if env.openIncident(t, RuleHostDiskForecast, "/") != nil {
		t.Error("forecast opened although the filesystem fills after the horizon")
	}
	// About 14 GB a day: full in under three days.
	seedGrowth(600_000_000)
	env.evaluate(t, time.Minute)
	if env.openIncident(t, RuleHostDiskForecast, "/") == nil {
		t.Error("forecast did not open for a filesystem filling within the horizon")
	}
	// Growth stops.
	seedGrowth(0)
	for minute := 2; minute <= 12; minute++ {
		env.evaluate(t, time.Duration(minute)*time.Minute)
	}
	if env.openIncident(t, RuleHostDiskForecast, "/") != nil {
		t.Error("forecast did not resolve when growth stopped")
	}
}

func TestHostMemoryRule(t *testing.T) {
	env := newTestEnv(t)
	if err := env.store.InsertHostMinute(context.Background(), store.HostMinute{
		TS: env.start.Truncate(time.Minute), CPUPercent: 10, Load1: 1, MemTotal: 1000, MemAvailable: 40,
		SwapTotal: 100, SwapUsed: 1,
	}); err != nil {
		t.Fatalf("InsertHostMinute() error = %v", err)
	}
	env.evaluate(t, 0)
	env.evaluate(t, 4*time.Minute)
	if env.openIncident(t, RuleHostMemory, env.cfg.HostName) != nil {
		t.Error("memory incident opened before five minutes")
	}
	env.evaluate(t, 5*time.Minute)
	if env.openIncident(t, RuleHostMemory, env.cfg.HostName) == nil {
		t.Fatal("memory incident did not open after five minutes")
	}
	if err := env.store.InsertHostMinute(context.Background(), store.HostMinute{
		TS: env.start.Truncate(time.Minute).Add(6 * time.Minute), CPUPercent: 10, Load1: 1, MemTotal: 1000,
		MemAvailable: 500, SwapTotal: 100, SwapUsed: 1,
	}); err != nil {
		t.Fatalf("InsertHostMinute() error = %v", err)
	}
	env.evaluate(t, 10*time.Minute)
	env.evaluate(t, 15*time.Minute)
	if env.openIncident(t, RuleHostMemory, env.cfg.HostName) != nil {
		t.Error("memory incident did not resolve once memory was available")
	}
}

func TestHostUnitFailedRule(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Host.SystemdCritical = []string{"*-backup.service"}
	exit := 1
	if err := env.store.UpsertUnit(context.Background(), store.Unit{
		Name: "nightly-backup.service", ActiveState: "failed", SubState: "failed", Result: "exit-code",
		ExitStatus: &exit, LastSeen: env.start,
	}); err != nil {
		t.Fatalf("UpsertUnit() error = %v", err)
	}
	env.evaluate(t, 0)
	incident := env.openIncident(t, RuleHostUnitFailed, "nightly-backup.service")
	if incident == nil {
		t.Fatal("unit incident did not open")
	}
	if incident.Severity != "critical" {
		t.Errorf("severity = %q, want critical for a matching pattern", incident.Severity)
	}
	env.store.UpsertUnit(context.Background(), store.Unit{
		Name: "nightly-backup.service", ActiveState: "active", SubState: "dead", LastSeen: env.start,
	})
	env.evaluate(t, time.Minute)
	if env.openIncident(t, RuleHostUnitFailed, "nightly-backup.service") != nil {
		t.Error("unit incident did not resolve when the unit left the failed state")
	}
}

func TestCertRules(t *testing.T) {
	env := newTestEnv(t)
	expiring := env.start.Add(7 * 24 * time.Hour)
	if err := env.store.UpsertCert(context.Background(), store.Cert{
		Domain: "shop.example.com", CheckedAt: env.start, Status: "expiring", NotAfter: &expiring,
		Issuer: "Test CA", Subject: "shop.example.com", SANs: []string{"shop.example.com"},
	}); err != nil {
		t.Fatalf("UpsertCert() error = %v", err)
	}
	env.evaluate(t, 0)
	incident := env.openIncident(t, RuleCertExpiry, "shop.example.com")
	if incident == nil || incident.Severity != "warning" {
		t.Fatalf("incident = %+v, want a warning expiry incident", incident)
	}
	// Three days left: the same incident escalates.
	soon := env.start.Add(2 * 24 * time.Hour)
	env.store.UpsertCert(context.Background(), store.Cert{
		Domain: "shop.example.com", CheckedAt: env.start, Status: "expiring", NotAfter: &soon,
		Issuer: "Test CA", Subject: "shop.example.com", SANs: []string{"shop.example.com"},
	})
	env.evaluate(t, time.Minute)
	incident = env.openIncident(t, RuleCertExpiry, "shop.example.com")
	if incident == nil || incident.Severity != "critical" {
		t.Fatalf("incident = %+v, want critical within three days", incident)
	}
	// A default certificate is invalid.
	env.store.UpsertCert(context.Background(), store.Cert{
		Domain: "other.example.com", CheckedAt: env.start, Status: "default",
		Issuer: "TRAEFIK DEFAULT CERT", Subject: "TRAEFIK DEFAULT CERT",
	})
	env.evaluate(t, 2*time.Minute)
	if env.openIncident(t, RuleCertInvalid, "other.example.com") == nil {
		t.Error("cert.invalid did not open for the default certificate")
	}
	// A fine check resolves the expiry incident.
	fine := env.start.Add(90 * 24 * time.Hour)
	env.store.UpsertCert(context.Background(), store.Cert{
		Domain: "shop.example.com", CheckedAt: env.start, Status: "ok", NotAfter: &fine,
		Issuer: "Test CA", Subject: "shop.example.com", SANs: []string{"shop.example.com"},
	})
	env.evaluate(t, 3*time.Minute)
	if env.openIncident(t, RuleCertExpiry, "shop.example.com") != nil {
		t.Error("expiry incident did not resolve after a fine check")
	}
}

func TestPeerSilentRule(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Heartbeat.Peers = []string{"host-2"}
	env.cfg.Heartbeat.Timeout = 3 * time.Minute
	env.engine = New(env.store, &fakeModel{model: env.model}, nil, env.cfg, discardLogger(), env.clock, env.notifier)
	env.evaluate(t, 0)
	if env.openIncident(t, RulePeerSilent, "host-2") != nil {
		t.Error("peer incident opened before the timeout")
	}
	env.evaluate(t, 3*time.Minute)
	if env.openIncident(t, RulePeerSilent, "host-2") == nil {
		t.Fatal("peer incident did not open after the timeout")
	}
	// A heartbeat resolves it.
	if _, err := env.conn.Exec(context.Background(), `
		insert into peers (name, last_heartbeat) values ('host-2', $1)
		on conflict (name) do update set last_heartbeat = excluded.last_heartbeat`, env.clock.now); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	env.evaluate(t, 4*time.Minute)
	if env.openIncident(t, RulePeerSilent, "host-2") != nil {
		t.Error("peer incident did not resolve on the next heartbeat")
	}
}

func TestSourceFailedRule(t *testing.T) {
	env := newTestEnv(t)
	if err := env.store.RecordSourceStatus(context.Background(), "docker", store.StatusUnavailable, "unreachable", env.start.Add(-6*time.Minute)); err != nil {
		t.Fatalf("RecordSourceStatus() error = %v", err)
	}
	env.evaluate(t, 0)
	incident := env.openIncident(t, RuleSourceFailed, "docker")
	if incident == nil || incident.Severity != "warning" {
		t.Fatalf("incident = %+v, want a warning source incident", incident)
	}
	if err := env.store.RecordSourceStatus(context.Background(), "docker", store.StatusOK, "", env.clock.now); err != nil {
		t.Fatalf("RecordSourceStatus() error = %v", err)
	}
	env.evaluate(t, time.Minute)
	if env.openIncident(t, RuleSourceFailed, "docker") != nil {
		t.Error("source incident did not resolve when the source recovered")
	}
}

func TestSourceNeedsFiveMinutes(t *testing.T) {
	env := newTestEnv(t)
	if err := env.store.RecordSourceStatus(context.Background(), "docker", store.StatusUnavailable, "unreachable", env.start.Add(-4*time.Minute)); err != nil {
		t.Fatalf("RecordSourceStatus() error = %v", err)
	}
	env.evaluate(t, 0)
	if env.openIncident(t, RuleSourceFailed, "docker") != nil {
		t.Error("source incident opened before five minutes of failure")
	}
}

func TestIncidentsResolveWhenTheirSubjectDisappears(t *testing.T) {
	env := newTestEnv(t)
	env.container.Health = "unhealthy"
	env.evaluate(t, 0)
	env.evaluate(t, 2*time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") == nil {
		t.Fatal("unhealthy incident did not open")
	}
	env.notifier.take()
	// The container is removed: its slot leaves the model, so no rule reports
	// on it any more.
	delete(env.app.Slots, "api #1")
	env.evaluate(t, 3*time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") == nil {
		t.Error("incident closed at once, want it to wait out the quiet period like any other")
	}
	env.evaluate(t, 9*time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") != nil {
		t.Error("incident of a removed container stayed open")
	}
	var resolved bool
	for _, change := range env.notifier.take() {
		if change.Kind == ChangeResolved && change.Incident.Rule == RuleContainerUnhealthy {
			resolved = true
		}
	}
	if !resolved {
		t.Error("no resolved change was reported for the vanished incident")
	}
}

func TestVanishedIncidentComesBackWithItsSubject(t *testing.T) {
	env := newTestEnv(t)
	env.container.Health = "unhealthy"
	env.evaluate(t, 0)
	env.evaluate(t, 2*time.Minute)
	delete(env.app.Slots, "api #1")
	env.evaluate(t, 3*time.Minute)
	// A redeploy: the slot is back within the quiet period and still unhealthy.
	env.app.Slots["api #1"] = env.slot
	env.evaluate(t, 4*time.Minute)
	env.evaluate(t, 10*time.Minute)
	if env.openIncident(t, RuleContainerUnhealthy, "shop / api #1") == nil {
		t.Error("incident was closed although its container came back unhealthy")
	}
}
