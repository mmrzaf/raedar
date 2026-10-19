//go:build integration

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// testSchema creates a schema and returns a migrated store and a connection.
func testSchema(t *testing.T) (*store.Store, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	databaseURL := os.Getenv("RAEDAR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("RAEDAR_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("raedar_web_%d", time.Now().UnixNano())
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
	st, err := store.Open(ctx, parsed.String(), discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	conn, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect to the schema: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return st, conn
}

// testModelreturns a model with one app, route and slot.
type fakeHostModel struct{ model *discovery.Model }

func (m *fakeHostModel) Model() *discovery.Model { return m.model }

func newIntegrationServer(t *testing.T) (*Server, *store.Store, *pgx.Conn, *discovery.Model) {
	t.Helper()
	st, conn := testSchema(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)
	appID, err := st.UpsertApp(ctx, "shop", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	routeID, err := st.UpsertRoute(ctx, store.Route{
		Name: "shop@docker", AppID: &appID, Kind: "http", Origin: "labels",
		Domains: []string{"shop.example.com"}, TLS: true,
	}, now)
	if err != nil {
		t.Fatalf("UpsertRoute() error = %v", err)
	}
	slotID, err := st.UpsertSlot(ctx, store.Slot{AppID: appID, Key: "api #1", Service: "api", Replica: 1}, now)
	if err != nil {
		t.Fatalf("UpsertSlot() error = %v", err)
	}
	// Minute data in the last hour.
	latency := make([]int32, store.Buckets)
	latency[2] = 40
	if err := st.FlushBuckets(ctx, store.Flush{
		Minutes: []store.RouteMinute{{
			RouteID: routeID, TS: now.Add(-90 * time.Minute), Requests: 40, S2xx: 39, S5xx: 1, Latency: latency,
		}},
		Windows: []store.PathWindow{{
			RouteID: routeID, TS: now.Add(-90 * time.Minute), Method: "POST", Pattern: "/api/checkout",
			Requests: 40, S5xx: 1, Latency: latency,
		}},
	}); err != nil {
		t.Fatalf("FlushBuckets() error = %v", err)
	}
	if err := st.Rollup(ctx, now); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	if _, err := st.InsertIncident(ctx, store.Incident{
		Rule: "route.down", Subject: "shop@docker", AppID: &appID, RouteID: &routeID, Severity: "critical",
		OpenedAt: now.Add(-20 * time.Minute), Summary: "no backend for requests", Details: []byte(`{}`),
	}); err != nil {
		t.Fatalf("InsertIncident() error = %v", err)
	}
	limit := int64(512 << 20)
	for i := 0; i < 3; i++ {
		if err := st.InsertSlotMinute(ctx, store.SlotMinute{
			SlotID: slotID, TS: now.Add(-time.Duration(i) * time.Minute), CPUMillicores: 100 + i*10,
			MemBytes: 128 << 20, MemLimitBytes: &limit, NetRxBytes: 10, NetTxBytes: 20, Running: true,
		}); err != nil {
			t.Fatalf("InsertSlotMinute() error = %v", err)
		}
	}
	if err := st.InsertHostMinute(ctx, store.HostMinute{
		TS: now.Add(-time.Minute), CPUPercent: 12.5, Load1: 0.5, Load5: 0.4, Load15: 0.3,
		MemTotal: 16 << 30, MemAvailable: 8 << 30, SwapTotal: 2 << 30, SwapUsed: 1 << 30,
	}); err != nil {
		t.Fatalf("InsertHostMinute() error = %v", err)
	}
	if err := st.InsertDiskMinute(ctx, store.DiskMinute{
		Disk: "/", TS: now.Add(-time.Minute), SizeBytes: 100, UsedBytes: 50, AvailBytes: 50,
		Inodes: 100, InodesUsed: 10,
	}); err != nil {
		t.Fatalf("InsertDiskMinute() error = %v", err)
	}
	if _, err := st.AddEvent(ctx, store.Event{
		TS: now.Add(-time.Hour), Kind: "deploy", Severity: "info", AppID: &appID, Subject: "shop",
		Title: "deploy shop", Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("AddEvent() error = %v", err)
	}
	if err := st.RecordSourceStatus(ctx, "accesslog", store.StatusOK, "", now); err != nil {
		t.Fatalf("RecordSourceStatus() error = %v", err)
	}
	if err := st.UpsertUnit(ctx, store.Unit{
		Name: "backup.service", ActiveState: "failed", SubState: "failed", Result: "exit-code", LastSeen: now,
	}); err != nil {
		t.Fatalf("UpsertUnit() error = %v", err)
	}
	expiry := now.Add(10 * 24 * time.Hour)
	if err := st.UpsertCert(ctx, store.Cert{
		Domain: "shop.example.com", RouteID: &routeID, CheckedAt: now, Status: "expiring", NotAfter: &expiry,
		Issuer: "Test CA", Subject: "shop.example.com", SANs: []string{"shop.example.com"},
	}); err != nil {
		t.Fatalf("UpsertCert() error = %v", err)
	}
	container := &discovery.Container{ID: "c1", Name: "shop-api-1", ImageRef: "ghcr.io/shop/api:2.4.0", State: "running", Health: "healthy"}
	app := &discovery.App{ID: appID, Name: "shop", Alerts: true,
		Slots: map[string]*discovery.Slot{}, Routes: map[string]*discovery.Route{}}
	slot := &discovery.Slot{ID: slotID, App: app, Key: "api #1", Service: "api", Replica: 1, Container: container}
	app.Slots["api #1"] = slot
	route := &discovery.Route{ID: routeID, Name: "shop@docker", App: app, Kind: "http",
		Domains: []string{"shop.example.com"}, TLS: true}
	app.Routes["shop@docker"] = route
	container.App = app
	container.Slot = slot
	model := &discovery.Model{
		Apps:       map[string]*discovery.App{"shop": app},
		Routes:     map[string]*discovery.Route{"shop@docker": route},
		Containers: map[string]*discovery.Container{"c1": container},
	}
	cfg := testConfig(t)
	server, err := New(st, cfg, nil, &fakeHostModel{model: model}, discardLogger(),
		&testClock{now: now}, Options{Version: "test"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server, st, conn, model
}

// get performs one request and returns the response.
func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestSeriesAPI(t *testing.T) {
	server, _, _, _ := newIntegrationServer(t)
	handler := server.Handler()
	from := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC).Unix()
	to := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC).Unix()
	path := "/api/series?keys=" + url.QueryEscape("route:shop@docker:requests,slot:shop / api #1:cpu,host:cpu,disk:/:used") +
		fmt.Sprintf("&from=%d&to=%d", from, to)
	recorder := get(t, handler, path)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
		Bin    int64     `json:"bin_seconds"`
		Source string    `json:"source"`
		Series []struct {
			Key    string       `json:"key"`
			Label  string       `json:"label"`
			Unit   string       `json:"unit"`
			Points [][2]float64 `json:"points"`
		} `json:"series"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Source != "minutes" {
		t.Errorf("source = %q, want minutes", body.Source)
	}
	if len(body.Series) != 4 {
		t.Fatalf("series = %d, want 4", len(body.Series))
	}
	if len(body.Series[0].Points) == 0 || body.Series[0].Points[0][1] != 40 {
		t.Errorf("route requests = %+v, want the seeded 40 requests", body.Series[0].Points)
	}
	if body.Series[1].Unit != "millicores" || len(body.Series[1].Points) == 0 {
		t.Errorf("slot series = %+v, want CPU in millicores", body.Series[1])
	}
	if body.Series[3].Unit != "percent" {
		t.Errorf("disk unit = %q, want percent", body.Series[3].Unit)
	}
}

func TestBaselineAndPathsAPI(t *testing.T) {
	server, _, conn, _ := newIntegrationServer(t)
	handler := server.Handler()
	now := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)
	if _, err := conn.Exec(context.Background(), `insert into baselines (route_id, hour_of_day, error_rate, p95_ms, days_used, computed_at)
		select id, 14, 0.002, 210, 5, now() from routes`); err != nil {
		t.Fatalf("insert baseline: %v", err)
	}
	from := now.Add(-2 * time.Hour).Unix()
	to := now.Unix()
	recorder := get(t, handler, fmt.Sprintf("/api/baseline?route=shop@docker&from=%d&to=%d", from, to))
	if recorder.Code != http.StatusOK {
		t.Fatalf("baseline status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var baseline struct {
		Route     string `json:"route"`
		Baselines []struct {
			Hour      int      `json:"hour"`
			ErrorRate *float64 `json:"error_rate"`
			P95MS     *float64 `json:"p95_ms"`
			DaysUsed  int      `json:"days_used"`
		} `json:"baselines"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &baseline); err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	if baseline.Route != "shop@docker" || len(baseline.Baselines) == 0 {
		t.Fatalf("baseline = %+v, want the route's hours", baseline)
	}
	found := false
	for _, entry := range baseline.Baselines {
		if entry.Hour == 14 && entry.ErrorRate != nil && *entry.ErrorRate == 0.002 && entry.DaysUsed == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("baseline = %+v, want the seeded hour", baseline.Baselines)
	}
	recorder = get(t, handler, fmt.Sprintf("/api/paths?app=shop&from=%d&to=%d", from, to))
	if recorder.Code != http.StatusOK {
		t.Fatalf("paths status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var paths struct {
		Paths []struct {
			Method   string  `json:"method"`
			Pattern  string  `json:"pattern"`
			Requests int64   `json:"requests"`
			S5xx     int64   `json:"s5xx"`
			P95MS    float64 `json:"p95_ms"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &paths); err != nil {
		t.Fatalf("decode paths: %v", err)
	}
	if len(paths.Paths) != 1 || paths.Paths[0].Pattern != "/api/checkout" || paths.Paths[0].S5xx != 1 {
		t.Errorf("paths = %+v, want the seeded path", paths.Paths)
	}
}

func TestEventsAPI(t *testing.T) {
	server, _, _, _ := newIntegrationServer(t)
	now := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)
	recorder := get(t, server.Handler(), fmt.Sprintf("/api/events?app=shop&from=%d&to=%d", now.Add(-2*time.Hour).Unix(), now.Unix()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Events []struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
			App   string `json:"app"`
		} `json:"events"`
		Incidents []struct {
			Rule string `json:"rule"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].Kind != "deploy" || body.Events[0].App != "shop" {
		t.Errorf("events = %+v, want the deploy of shop", body.Events)
	}
	if len(body.Incidents) != 1 || body.Incidents[0].Rule != "route.down" {
		t.Errorf("incidents = %+v, want the route.down incident", body.Incidents)
	}
}

func TestPatternsAPI(t *testing.T) {
	server, _, _, _ := newIntegrationServer(t)
	recorder := get(t, server.Handler(), "/api/patterns?key="+url.QueryEscape("route:shop@docker:requests")+"&weeks=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Key        string      `json:"key"`
		Heatmap    [][]float64 `json:"heatmap"`
		DayProfile []float64   `json:"day_profile"`
		Trend      []struct {
			Day   string  `json:"day"`
			Value float64 `json:"value"`
		} `json:"trend"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Key != "route:shop@docker:requests" {
		t.Errorf("key = %q", body.Key)
	}
	if len(body.Heatmap) != 7 || len(body.Heatmap[0]) != 24 {
		t.Errorf("heatmap shape = %d x %d, want 7 x 24", len(body.Heatmap), len(body.Heatmap[0]))
	}
	if len(body.DayProfile) != 24 {
		t.Errorf("day profile = %d, want 24 hours", len(body.DayProfile))
	}
	if len(body.Trend) == 0 {
		t.Error("trend is empty, want daily points")
	}
}

func TestPagesAndHealth(t *testing.T) {
	server, _, _, _ := newIntegrationServer(t)
	handler := server.Handler()
	pages := map[string]string{
		"/":            "Apps",
		"/apps/shop":   "Paths",
		"/host":        "Certificates",
		"/patterns":    "Day profile",
		"/events":      "Events",
		"/incidents/1": "Deliveries",
		"/about":       "Notifications",
	}
	for page, marker := range pages {
		recorder := get(t, handler, page)
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 (body %q)", page, recorder.Code, recorder.Body.String())
			continue
		}
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("GET %s body does not contain %q, the page was not rendered fully", page, marker)
		}
	}
	if _, err := server.loadOverview(context.Background()); err != nil {
		t.Fatalf("loadOverview() error = %v", err)
	}
	recorder := get(t, handler, "/healthz")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") {
		t.Errorf("healthz status = %d, body = %q, want ok", recorder.Code, recorder.Body.String())
	}
	recorder = get(t, handler, "/api/overview")
	if recorder.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200", recorder.Code)
	}
	var overview struct {
		HostName string `json:"host_name"`
		Apps     []struct {
			Name      string    `json:"name"`
			Status    string    `json:"status"`
			Sparkline []float64 `json:"sparkline"`
			ErrorRate float64   `json:"error_rate"`
		} `json:"apps"`
		Incidents []struct {
			Rule string `json:"rule"`
		} `json:"incidents"`
		Host struct {
			CPU   float64 `json:"cpu"`
			Disks []struct {
				Name string `json:"name"`
			} `json:"disks"`
		} `json:"host"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.HostName != "host-1" {
		t.Errorf("host = %q, want host-1", overview.HostName)
	}
	if len(overview.Apps) != 1 || overview.Apps[0].Name != "shop" || len(overview.Apps[0].Sparkline) != 24 {
		t.Errorf("apps = %+v, want shop with a 24-hour sparkline", overview.Apps)
	}
	if len(overview.Host.Disks) != 1 || overview.Host.Disks[0].Name != "/" {
		t.Errorf("host = %+v, want the root filesystem", overview.Host)
	}
}
