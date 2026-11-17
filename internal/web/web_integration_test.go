//go:build integration

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// testSchema creates a schema and returns a migrated store and a connection.
func testSchema(t *testing.T) (*store.Store, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	databaseURL, schema := testdb.NewSchema(t, "web")
	schemaURL := testdb.WithSearchPath(t, databaseURL, schema)
	st, err := store.Open(ctx, schemaURL, discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	conn, err := pgx.Connect(ctx, schemaURL)
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
	if err := st.Rollup(ctx, now, time.UTC); err != nil {
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
	server, _, _, model := newIntegrationServer(t)
	handler := server.Handler()
	from := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC).Unix()
	to := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC).Unix()
	slot := model.Apps["shop"].Slots["api #1"]
	params := url.Values{"series": {
		seriesRef{Kind: subjectRoute, Subject: "shop@docker", Metric: "requests"}.String(),
		seriesRef{Kind: subjectSlot, Subject: strconv.FormatInt(slot.ID, 10), Metric: "cpu"}.String(),
		seriesRef{Kind: subjectHost, Metric: "cpu"}.String(),
		seriesRef{Kind: subjectDisk, Subject: "/", Metric: "used"}.String(),
	}}
	path := "/api/series?" + params.Encode() + fmt.Sprintf("&from=%d&to=%d", from, to)
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
			Series string        `json:"series"`
			Metric string        `json:"metric"`
			Label  string        `json:"label"`
			Unit   string        `json:"unit"`
			Points [][2]*float64 `json:"points"`
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
	if len(body.Series[0].Points) == 0 || body.Series[0].Points[0][1] == nil || *body.Series[0].Points[0][1] != 40 {
		t.Errorf("route requests = %+v, want the seeded 40 requests", body.Series[0].Points)
	}
	if body.Series[1].Unit != "millicores" || len(body.Series[1].Points) == 0 {
		t.Errorf("slot series = %+v, want CPU in millicores", body.Series[1])
	}
	if body.Series[1].Label != "api #1" || body.Series[1].Metric != "cpu" {
		t.Errorf("slot series label = %q, metric = %q, want the slot key and metric name", body.Series[1].Label, body.Series[1].Metric)
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
			Method   string `json:"method"`
			Pattern  string `json:"pattern"`
			Requests int64  `json:"requests"`
			S5xx     int64  `json:"s5xx"`
			P95      struct {
				MS float64 `json:"ms"`
			} `json:"p95"`
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
	ref := seriesRef{Kind: subjectRoute, Subject: "shop@docker", Metric: "requests"}.String()
	recorder := get(t, server.Handler(), "/api/patterns?series="+url.QueryEscape(ref)+"&weeks=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Series     string       `json:"series"`
		Unit       string       `json:"unit"`
		Heatmap    [][]*float64 `json:"heatmap"`
		DayProfile []*float64   `json:"day_profile"`
		Trend      []struct {
			TS    int64   `json:"ts"`
			Value float64 `json:"value"`
		} `json:"trend"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Series != ref || body.Unit != "count" {
		t.Errorf("series = %q, unit = %q, want the request echoed with its unit", body.Series, body.Unit)
	}
	if len(body.Heatmap) != 7 || len(body.Heatmap[0]) != 24 || len(body.DayProfile) != 24 {
		t.Fatalf("heatmap shape = %d x %d, day profile = %d, want 7 x 24 and 24 hours", len(body.Heatmap), len(body.Heatmap[0]), len(body.DayProfile))
	}
	// One hour of data was seeded: every other cell and hour has no data and
	// must be null, not zero.
	filled := 0
	for _, row := range body.Heatmap {
		for _, cell := range row {
			if cell != nil {
				filled++
			}
		}
	}
	if filled != 1 {
		t.Errorf("heatmap has %d cells with data, want the one seeded hour", filled)
	}
	profileHours := 0
	for _, value := range body.DayProfile {
		if value != nil {
			profileHours++
		}
	}
	if profileHours != 1 {
		t.Errorf("day profile has %d hours with data, want 1", profileHours)
	}
	if len(body.Trend) != 1 || body.Trend[0].Value != 40 {
		t.Fatalf("trend = %+v, want one day with the seeded 40 requests", body.Trend)
	}
	local := time.Date(2026, 10, 18, 0, 0, 0, 0, server.cfg.Location)
	if body.Trend[0].TS != local.Unix() {
		t.Errorf("trend ts = %d, want the start of the local day %d", body.Trend[0].TS, local.Unix())
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
		"/silences":    "Silences",
		"/deliveries":  "Deliveries",
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

func TestPatternsUseLocalHoursInHalfHourZones(t *testing.T) {
	server, st, conn, _ := newIntegrationServer(t)
	tehran, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	server.cfg.Location = tehran
	routeID := int64(0)
	if err := conn.QueryRow(context.Background(), "select id from routes where name = 'shop@docker'").Scan(&routeID); err != nil {
		t.Fatalf("read route: %v", err)
	}
	// 12:31 UTC is 16:01 in Tehran. Bins that do not start on a local hour
	// would put it into the previous local hour.
	if err := st.FlushBuckets(context.Background(), store.Flush{Minutes: []store.RouteMinute{{
		RouteID: routeID, TS: time.Date(2026, 10, 18, 12, 31, 0, 0, time.UTC), Requests: 10,
	}}}); err != nil {
		t.Fatalf("FlushBuckets() error = %v", err)
	}
	ref := seriesRef{Kind: subjectRoute, Subject: "shop@docker", Metric: "requests"}.String()
	recorder := get(t, server.Handler(), "/api/patterns?series="+url.QueryEscape(ref)+"&weeks=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		DayProfile []*float64 `json:"day_profile"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var hours []int
	for hour, value := range body.DayProfile {
		if value != nil {
			hours = append(hours, hour)
		}
	}
	if len(hours) != 1 || hours[0] != 16 {
		t.Errorf("hours with data = %v, want only local hour 16", hours)
	}
}

// TestSilenceFormCreatesAndDeletes covers the operator flow: a
// same-origin POST creates a silence, the page lists it, and a second POST
// removes it. A cross-origin POST is refused.
func TestSilenceFormCreatesAndDeletes(t *testing.T) {
	server, _, _, _ := newIntegrationServer(t)
	handler := server.Handler()

	form := url.Values{
		"reason":   {"release 2.4.0"},
		"rule":     {"route.down"},
		"subject":  {"fallback@file"},
		"duration": {"30m"},
	}
	request := httptest.NewRequest(http.MethodPost, "/silences", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://"+request.Host)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303 (body %q)", recorder.Code, recorder.Body.String())
	}

	page := get(t, handler, "/silences")
	if !strings.Contains(page.Body.String(), "release 2.4.0") {
		t.Fatalf("silences page does not list the new silence")
	}
	silences, err := server.store.ListSilences(context.Background(), time.Now(), 10)
	if err != nil || len(silences) != 1 {
		t.Fatalf("ListSilences = %v, %v; want one", silences, err)
	}

	remove := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/silences/%d/delete", silences[0].ID), nil)
	remove.Header.Set("Origin", "http://"+remove.Host)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, remove)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", recorder.Code)
	}
	if left, _ := server.store.ListSilences(context.Background(), time.Now(), 10); len(left) != 0 {
		t.Fatalf("silences after delete = %d, want 0", len(left))
	}

	cross := httptest.NewRequest(http.MethodPost, "/silences", strings.NewReader(form.Encode()))
	cross.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cross.Header.Set("Origin", "https://evil.example.com")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, cross)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", recorder.Code)
	}
}

// TestSilenceFormRejectsUnsafeInput covers the bounds of the silence form: a
// silence must name a rule or a subject, and may not exceed the maximum.
func TestSilenceFormRejectsUnsafeInput(t *testing.T) {
	server, _, _, _ := newIntegrationServer(t)
	handler := server.Handler()

	cases := map[string]url.Values{
		"wildcard":     {"reason": {"x"}, "duration": {"1h"}},
		"too long":     {"reason": {"x"}, "rule": {"route.down"}, "duration": {"31d"}},
		"no reason":    {"rule": {"route.down"}, "duration": {"1h"}},
		"bad duration": {"reason": {"x"}, "rule": {"route.down"}, "duration": {"soon"}},
	}
	for name, form := range cases {
		request := httptest.NewRequest(http.MethodPost, "/silences", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://"+request.Host)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, recorder.Code)
		}
	}
	if silences, err := server.store.ListSilences(context.Background(), time.Now(), 10); err != nil || len(silences) != 0 {
		t.Fatalf("silences after rejected forms = %v, %v; want none", silences, err)
	}
}

// addApp adds an app with one route and slot, one minute of traffic 90
// minutes before the fixture's clock, and a matching model entry.
func addApp(t *testing.T, st *store.Store, model *discovery.Model, name string, requests, errors int64, bucket int) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC).Add(-90 * time.Minute)
	appID, err := st.UpsertApp(ctx, name, at)
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	routeID, err := st.UpsertRoute(ctx, store.Route{Name: name + "@docker", AppID: &appID, Kind: "http", Origin: "labels",
		Domains: []string{name + ".example.com"}}, at)
	if err != nil {
		t.Fatalf("UpsertRoute() error = %v", err)
	}
	slotID, err := st.UpsertSlot(ctx, store.Slot{AppID: appID, Key: "web #1", Service: "web", Replica: 1}, at)
	if err != nil {
		t.Fatalf("UpsertSlot() error = %v", err)
	}
	latency := make([]int32, store.Buckets)
	latency[bucket] = int32(requests)
	if err := st.FlushBuckets(ctx, store.Flush{Minutes: []store.RouteMinute{{
		RouteID: routeID, TS: at, Requests: requests, S2xx: requests - errors, S5xx: errors, Latency: latency,
	}}}); err != nil {
		t.Fatalf("FlushBuckets() error = %v", err)
	}
	if err := st.InsertSlotMinute(ctx, store.SlotMinute{SlotID: slotID, TS: at, CPUMillicores: 50, MemBytes: 64 << 20, Running: true}); err != nil {
		t.Fatalf("InsertSlotMinute() error = %v", err)
	}
	app := &discovery.App{ID: appID, Name: name, Group: "shops", Slots: map[string]*discovery.Slot{}, Routes: map[string]*discovery.Route{}}
	app.Slots["web #1"] = &discovery.Slot{ID: slotID, App: app, Key: "web #1", Service: "web", Replica: 1}
	app.Routes[name+"@docker"] = &discovery.Route{ID: routeID, Name: name + "@docker", App: app, Kind: "http"}
	model.Apps[name] = app
	model.Routes[name+"@docker"] = app.Routes[name+"@docker"]
}

func TestCombinedAppsUseSummedCountsAndMergedHistograms(t *testing.T) {
	server, st, _, model := newIntegrationServer(t)
	addApp(t, st, model, "blog", 60, 5, 8)
	handler := server.Handler()
	from := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC).Unix()
	to := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC).Unix()
	query := func(kind subjectKind, subject, metric string) []*float64 {
		path := "/api/series?series=" + url.QueryEscape(seriesRef{Kind: kind, Subject: subject, Metric: metric}.String()) +
			fmt.Sprintf("&from=%d&to=%d&points=10", from, to)
		recorder := get(t, handler, path)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body %q)", path, recorder.Code, recorder.Body.String())
		}
		var body struct {
			Series []struct {
				Points [][2]*float64 `json:"points"`
			} `json:"series"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var values []*float64
		for _, point := range body.Series[0].Points {
			if point[1] != nil {
				values = append(values, point[1])
			}
		}
		return values
	}
	// shop: 1 of 40 (2.5%), blog: 5 of 60 (8.33%). The combined rate is 6 of
	// 100, not the mean of the two percentages (5.42%).
	rate := query(subjectApps, "shop,blog", "error_rate")
	if len(rate) != 1 || *rate[0] != 6 {
		t.Errorf("combined error rate = %v, want one value of 6", rate)
	}
	requests := query(subjectApps, "shop,blog", "requests")
	if len(requests) != 1 || *requests[0] != 100 {
		t.Errorf("combined requests = %v, want 100", requests)
	}
	var merged store.Histogram
	shopHist, blogHist := store.Histogram{}, store.Histogram{}
	shopHist[2], blogHist[8] = 40, 60
	merged.Merge(&shopHist)
	merged.Merge(&blogHist)
	want := percentileOf(&merged, 95).MS
	p95 := query(subjectApps, "shop,blog", "p95")
	if len(p95) != 1 || *p95[0] != want {
		t.Errorf("combined p95 = %v, want %v from the merged histogram", p95, want)
	}
	// Resources add: shop has 100-120 mc samples at other times, blog 50 mc at
	// the traffic minute; only the minute where blog has data is checked here.
	cpu := query(subjectApps, "blog", "cpu")
	if len(cpu) != 1 || *cpu[0] != 50 {
		t.Errorf("blog cpu = %v, want 50", cpu)
	}
	if missing := get(t, handler, "/api/series?series="+url.QueryEscape("apps/nope/requests")+fmt.Sprintf("&from=%d&to=%d", from, to)); missing.Code != http.StatusNotFound {
		t.Errorf("unknown app status = %d, want 404", missing.Code)
	}
}

func TestComparePageShowsExactTotals(t *testing.T) {
	server, st, _, model := newIntegrationServer(t)
	addApp(t, st, model, "blog", 60, 5, 8)
	model.Apps["shop"].Group = "shops"
	handler := server.Handler()
	recorder := get(t, handler, "/compare?apps=shop,blog&range=24h")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"Combined", "6.00%", "data-compare=\"blog,shop\"", "shops"} {
		if !strings.Contains(body, want) {
			t.Errorf("compare page lacks %q", want)
		}
	}
	// The group link carries the member names and is escaped exactly once.
	link := regexp.MustCompile(`href="\?apps=([^&"]+)`).FindStringSubmatch(body)
	if link == nil {
		t.Fatal("compare page has no group link")
	}
	decoded, err := url.QueryUnescape(link[1])
	if err != nil || decoded != "blog,shop" {
		t.Errorf("group link apps = %q (decoded %q), want blog,shop", link[1], decoded)
	}
	// A group selects all its members.
	recorder = get(t, handler, "/compare?group=shops")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "data-compare=\"blog,shop\"") {
		t.Errorf("group selection failed: %d", recorder.Code)
	}
	// Without any parameter the page still renders with the busiest apps.
	if recorder = get(t, handler, "/compare"); recorder.Code != http.StatusOK {
		t.Errorf("default compare status = %d", recorder.Code)
	}
}

func TestAppPageShowsActiveChecksOnlyWhenEnabled(t *testing.T) {
	server, st, _, model := newIntegrationServer(t)
	handler := server.Handler()
	if body := get(t, handler, "/apps/shop").Body.String(); strings.Contains(body, "Availability checks") {
		t.Error("active checks are off, but the page shows them")
	}
	server.cfg.Probe.Enabled = true
	route := model.Routes["shop@docker"]
	route.Rule = "Host(`shop.example.com`)"
	now := time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)
	code, ms := 200, 12
	if err := st.SaveProbeState(context.Background(), store.ProbeState{RouteID: route.ID, State: store.ProbeHealthy, Since: now,
		LastResult: store.ProbeHealthy, LastChecked: now, StatusCode: &code, LatencyMS: &ms, Target: "shop.example.com/"}); err != nil {
		t.Fatalf("SaveProbeState() error = %v", err)
	}
	if err := st.AddProbeResult(context.Background(), store.ProbeMinute{RouteID: route.ID, TS: now.Add(-10 * time.Minute), Result: store.ProbeHealthy}); err != nil {
		t.Fatalf("AddProbeResult() error = %v", err)
	}
	body := get(t, handler, "/apps/shop").Body.String()
	for _, want := range []string{"Availability checks", "shop.example.com/", "healthy", "synthetic"} {
		if !strings.Contains(body, want) {
			t.Errorf("app page lacks %q", want)
		}
	}
	// A route that cannot be checked says why.
	route.Rule = "Host(`shop.example.com`) && Header(`X-A`, `b`)"
	if body := get(t, handler, "/apps/shop").Body.String(); !strings.Contains(body, "not checked") {
		t.Error("an ineligible route must be listed with a reason")
	}
}
