package web

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// testClock is a clock the tests set.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
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
	cfg.HostName = "host-1"
	return cfg
}

// newTestServer returns a server without a store for parameter tests.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(nil, testConfig(t), nil, nil, discardLogger(),
		&testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}, Options{Version: "dev"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server
}

func TestParameterValidation(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().Unix()
	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{name: "series without a series", path: "/api/series", status: 400, want: "series is required"},
		{name: "series with a bad range", path: "/api/series?series=host//cpu&from=x&to=y", status: 400, want: "from and to"},
		{name: "series with a reversed range", path: "/api/series?series=host//cpu&from=100&to=50", status: 400, want: "after"},
		{name: "series with too many points", path: "/api/series?series=host//cpu&from=100&to=200&points=99999", status: 400, want: "points"},
		{name: "series with an unknown kind", path: "/api/series?series=foo/bar/baz&from=100&to=200", status: 400, want: "unknown kind"},
		{name: "series with an unknown metric", path: "/api/series?series=host//p95&from=100&to=200", status: 400, want: "no metric"},
		{name: "series with a malformed reference", path: "/api/series?series=host:cpu&from=100&to=200", status: 400, want: "want kind/subject/metric"},
		{name: "baseline without a route", path: "/api/baseline", status: 400, want: "route"},
		{name: "paths without a subject", path: "/api/paths?from=100&to=200", status: 400, want: "app or route"},
		{name: "paths with a bad range", path: "/api/paths?app=x", status: 400, want: "from and to"},
		{name: "events with a bad range", path: "/api/events?from=1", status: 400, want: "from and to"},
		{name: "patterns without a series", path: "/api/patterns", status: 400, want: "series is required"},
		{name: "patterns with too many weeks", path: "/api/patterns?series=host//load1&weeks=100", status: 400, want: "weeks"},
		{name: "logs without a container", path: "/api/logs", status: 400, want: "container"},
		{name: "logs without since", path: "/api/logs?container=x", status: 400, want: "since"},
		{name: "health without a database", path: "/healthz", status: 503, want: "database"},
		{name: "unknown page", path: "/nope", status: 404},
	}
	_ = now
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d (body %q)", recorder.Code, tt.status, recorder.Body.String())
			}
			if tt.want != "" && !strings.Contains(recorder.Body.String(), tt.want) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tt.want)
			}
		})
	}
}

func TestStaticFilesAndHeartbeat(t *testing.T) {
	server, err := New(nil, testConfig(t), nil, nil, discardLogger(), &testClock{now: time.Now()}, Options{
		Version:   "dev",
		Heartbeat: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	handler := server.Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/style.css", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/css") {
		t.Errorf("static status = %d, type = %q, want the stylesheet", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/uplot.min.js", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("uplot status = %d, type = %q, want the chart library", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/heartbeat/host-2", nil))
	if recorder.Code != http.StatusNoContent {
		t.Errorf("heartbeat status = %d, want the receiver", recorder.Code)
	}
}

func TestStaticWithoutHeartbeat(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/heartbeat/host-2", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("heartbeat status = %d, want 404 without peers", recorder.Code)
	}
}

func TestReadOnlyInterface(t *testing.T) {
	server := newTestServer(t)
	paths := []string{"/", "/apps/x", "/host", "/patterns", "/events", "/incidents/1", "/about"}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want 405", path, recorder.Code)
		}
	}
}

func TestMaskURL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: ""},
		{name: "with password", value: "postgres://raedar:secret@postgres:5432/raedar", want: "postgres://raedar:••••••@postgres:5432/raedar"},
		{name: "without password", value: "postgres://raedar@postgres/raedar", want: "postgres://raedar@postgres/raedar"},
		{name: "plain", value: "host=postgres user=raedar", want: "host=postgres user=raedar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskURL(tt.value); got != tt.want {
				t.Errorf("maskURL(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestSeriesRefRoundTrip(t *testing.T) {
	tests := []seriesRef{
		{Kind: subjectRoute, Subject: "shop@docker", Metric: "p95"},
		{Kind: subjectApp, Subject: "my/app: a,b #1", Metric: "requests"},
		{Kind: subjectSlot, Subject: "42", Metric: "cpu"},
		{Kind: subjectHost, Metric: "load1"},
		{Kind: subjectDisk, Subject: "/", Metric: "used"},
	}
	for _, want := range tests {
		t.Run(want.String(), func(t *testing.T) {
			got, err := parseSeriesRef(want.String())
			if err != nil {
				t.Fatalf("parseSeriesRef(%q) error = %v", want.String(), err)
			}
			if got != want {
				t.Errorf("parseSeriesRef(%q) = %+v, want %+v", want.String(), got, want)
			}
		})
	}
}

func TestParseSeriesRefRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown kind":     "foo/bar/baz",
		"too few parts":    "host/cpu",
		"too many parts":   "app/a/b/p95",
		"unknown metric":   "app/shop/cpu",
		"metric of a kind": "host//requests",
		"bad escape":       "app/%zz/p95",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSeriesRef(raw); !errors.Is(err, errBadSeries) {
				t.Errorf("parseSeriesRef(%q) error = %v, want errBadSeries", raw, err)
			}
		})
	}
}

func TestPatternMetricNames(t *testing.T) {
	if got := patternMetricNames(subjectDisk); !reflect.DeepEqual(got, []string{"used", "inodes"}) {
		t.Errorf("patternMetricNames(disk) = %v, want used and inodes", got)
	}
	for _, kind := range []subjectKind{subjectApp, subjectRoute, subjectSlot, subjectHost, subjectDisk} {
		for _, name := range patternMetricNames(kind) {
			if !metricExists(kind, name) {
				t.Errorf("pattern metric %s/%s is not in the registry", kind, name)
			}
		}
	}
	if dailyAggregation(seriesRef{Kind: subjectApp, Metric: "requests"}) != aggregateSum ||
		dailyAggregation(seriesRef{Kind: subjectDisk, Metric: "used"}) != aggregateMax ||
		dailyAggregation(seriesRef{Kind: subjectRoute, Metric: "p95"}) != aggregateMean {
		t.Error("dailyAggregation() must sum counts, take the maximum of fill levels and average the rest")
	}
}

func TestRegistryUnitsAreKnownToTheUI(t *testing.T) {
	known := map[string]bool{"count": true, "percent": true, "ms": true, "bytes": true, "millicores": true, "load": true}
	check := func(kind string, name, unit string) {
		if !known[unit] {
			t.Errorf("%s metric %s has the unit %q, which format.js does not know", kind, name, unit)
		}
	}
	for _, m := range routeMetrics {
		check("route", m.name, m.unit)
	}
	for _, m := range slotMetrics {
		check("slot", m.name, m.unit)
	}
	for _, m := range hostMetrics {
		check("host", m.name, m.unit)
	}
	for _, m := range diskMetrics {
		check("disk", m.name, m.unit)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "zero", want: "never"},
		{name: "seconds", at: now.Add(-10 * time.Second), want: "just now"},
		{name: "minutes", at: now.Add(-5 * time.Minute), want: "5 min ago"},
		{name: "hours", at: now.Add(-3 * time.Hour), want: "3 h ago"},
		{name: "days", at: now.Add(-50 * time.Hour), want: "2 d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relativeTime(now, tt.at); got != tt.want {
				t.Errorf("relativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamLinesFollowsPastTheWindow(t *testing.T) {
	server := newTestServer(t)
	var written strings.Builder
	logs := strings.NewReader("2026-10-18T12:00:00Z first\n2026-10-18T12:03:00Z later\n")
	server.streamLines(&stringBuilderWriter{&written}, logs)
	for _, want := range []string{"first", "later"} {
		if !strings.Contains(written.String(), want) {
			t.Errorf("events = %q, want the %s line", written.String(), want)
		}
	}
}

func TestTrimLines(t *testing.T) {
	until := time.Date(2026, 10, 18, 12, 2, 0, 0, time.UTC)
	body := []byte("2026-10-18T12:00:00Z first\n2026-10-18T12:03:00Z after\n")
	got := string(trimLines(body, until))
	if !strings.Contains(got, "first") || strings.Contains(got, "after") {
		t.Errorf("trimLines() = %q, want only the line inside the window", got)
	}
	if got := trimLines(body, time.Time{}); string(got) != string(body) {
		t.Errorf("trimLines() without a limit = %q, want the whole body", got)
	}
}

// stringBuilderWriter lets the streaming test capture the events.
type stringBuilderWriter struct{ builder *strings.Builder }

func (w *stringBuilderWriter) Write(data []byte) (int, error) { return w.builder.Write(data) }

func TestHealthzWithDatabase(t *testing.T) {
	// A healthy database is covered by the integration tests; here only the
	// missing-store path matters.
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
}

var _ = context.Background

func TestNotificationView(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Notify.Telegram.Token = "123:token"
	server.cfg.Notify.Telegram.ChatIDs = []string{"42"}
	server.cfg.Notify.SMS.HTTP.URL = "https://sms.example.com/send"
	server.cfg.Notify.SMS.From = "+10001234567"
	server.cfg.Notify.SMS.To = []string{"+989120000000"}
	server.cfg.Notify.OutboundProxy = "http://proxy.example.com:3128"
	server.cfg.Notify.ProxyFor = []string{"telegram"}
	view := server.notificationView()
	if len(view.Channels) != 3 {
		t.Fatalf("channels = %d, want 3", len(view.Channels))
	}
	if !view.Channels[0].Configured || view.Channels[0].Route != "proxy" {
		t.Errorf("telegram = %+v, want configured through the proxy", view.Channels[0])
	}
	if !view.Channels[1].Configured || view.Channels[1].Route != "direct" {
		t.Errorf("sms = %+v, want configured direct", view.Channels[1])
	}
	if view.Channels[2].Configured {
		t.Errorf("webhook = %+v, want it unconfigured", view.Channels[2])
	}
	if !strings.Contains(view.Proxy, "telegram") || strings.Contains(view.Proxy, "sms") {
		t.Errorf("proxy = %q, want only the selected request", view.Proxy)
	}
}

func TestSecurityHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/style.css", nil))
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, want default-src 'self'", got)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestPagesLoadNothingExternal keeps the interface self-contained: no CDN, no
// external fonts, and no inline handlers or styles the CSP would block.
func TestPagesLoadNothingExternal(t *testing.T) {
	external := regexp.MustCompile(`(?:https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)
	inline := regexp.MustCompile(`\s(?:on[a-z]+|style)=`)
	err := fs.WalkDir(embedded, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.Contains(name, "uplot.min") || !strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".css") && !strings.HasSuffix(name, ".js") {
			return err
		}
		body, err := fs.ReadFile(embedded, name)
		if err != nil {
			return err
		}
		text := strings.ReplaceAll(string(body), "http://www.w3.org/2000/svg", "")
		if match := external.FindString(text); match != "" {
			t.Errorf("%s references %q, want only same-origin resources", name, match)
		}
		if strings.HasSuffix(name, ".html") {
			if match := inline.FindString(text); match != "" {
				t.Errorf("%s has the inline attribute %q, which the CSP blocks", name, strings.TrimSpace(match))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded files: %v", err)
	}
}

func TestEventKinds(t *testing.T) {
	events := []eventView{{Kind: "started"}, {Kind: "deploy"}, {Kind: "started"}, {Kind: "removed"}}
	want := []string{"deploy", "removed", "started"}
	if got := eventKinds(events); !reflect.DeepEqual(got, want) {
		t.Errorf("eventKinds() = %v, want %v", got, want)
	}
	if got := eventKinds(nil); len(got) != 0 {
		t.Errorf("eventKinds(nil) = %v, want none", got)
	}
}

func TestDiskOf(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	sample := func(hours int, used, avail int64) store.DiskMinute {
		return store.DiskMinute{Disk: "/", TS: start.Add(time.Duration(hours) * time.Hour), SizeBytes: used + avail + 10, UsedBytes: used, AvailBytes: avail}
	}
	t.Run("growing fills up in days", func(t *testing.T) {
		// 10 GB used grows by 1 GB in 10 hours; 24 GB are left: 240 hours.
		got := diskOf("/", []store.DiskMinute{sample(0, 10<<30, 25<<30), sample(10, 11<<30, 24<<30)})
		if !got.Growing || got.DaysUntilFull < 9.9 || got.DaysUntilFull > 10.1 {
			t.Errorf("diskOf() = %+v, want growing and full in about 10 days", got)
		}
		if got.Free != 24<<30 {
			t.Errorf("Free = %d, want the available space the forecast uses", got.Free)
		}
		if want := 100 * float64(11<<30) / float64(35<<30); got.Usage < want-0.01 || got.Usage > want+0.01 {
			t.Errorf("Usage = %v, want %v", got.Usage, want)
		}
	})
	t.Run("shrinking is not forecast", func(t *testing.T) {
		got := diskOf("/", []store.DiskMinute{sample(0, 11<<30, 24<<30), sample(10, 10<<30, 25<<30)})
		if got.Growing {
			t.Errorf("diskOf() = %+v, want no forecast for a shrinking disk", got)
		}
	})
	t.Run("too short a period gives no forecast", func(t *testing.T) {
		got := diskOf("/", []store.DiskMinute{sample(0, 10<<30, 25<<30), {TS: start.Add(30 * time.Minute), UsedBytes: 20 << 30, AvailBytes: 15 << 30}})
		if got.Growing {
			t.Errorf("diskOf() = %+v, want no forecast from under an hour of samples", got)
		}
	})
	t.Run("single sample", func(t *testing.T) {
		got := diskOf("/", []store.DiskMinute{sample(0, 10<<30, 25<<30)})
		if got.Growing || got.Name != "/" {
			t.Errorf("diskOf() = %+v, want just the usage", got)
		}
	})
}

func TestAppStatus(t *testing.T) {
	app := &discovery.App{ID: 7, Slots: map[string]*discovery.Slot{}}
	add := func(key string, container discovery.Container) {
		app.Slots[key] = &discovery.Slot{Key: key, Container: &container}
	}
	appID := int64(7)
	otherID := int64(8)
	warning := store.Incident{AppID: &appID, Severity: store.SeverityWarning}
	critical := store.Incident{AppID: &appID, Severity: store.SeverityCritical}
	elsewhere := store.Incident{AppID: &otherID, Severity: store.SeverityCritical}
	tests := []struct {
		name       string
		incidents  map[store.IncidentKey]store.Incident
		containers map[string]discovery.Container
		want       appHealth
	}{
		{name: "quiet", containers: map[string]discovery.Container{"api #1": {State: "running"}}, want: healthOK},
		{name: "incident of another app", incidents: map[store.IncidentKey]store.Incident{{Rule: "r", Subject: "s"}: elsewhere}, want: healthOK},
		{name: "warning incident", incidents: map[store.IncidentKey]store.Incident{{Rule: "r", Subject: "s"}: warning}, want: healthWarning},
		{name: "critical wins", incidents: map[store.IncidentKey]store.Incident{{Rule: "a", Subject: "s"}: warning, {Rule: "b", Subject: "s"}: critical}, want: healthCritical},
		{name: "unhealthy container", containers: map[string]discovery.Container{"api #1": {State: "running", Health: "unhealthy"}}, want: healthWarning},
		{name: "stopped container", containers: map[string]discovery.Container{"api #1": {State: "exited", ExitCode: 1, RestartPolicy: "always"}}, want: healthWarning},
		{name: "finished job is fine", containers: map[string]discovery.Container{"migrate #1": {State: "exited", RestartPolicy: "no"}}, want: healthOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app.Slots = map[string]*discovery.Slot{}
			for key, container := range tt.containers {
				add(key, container)
			}
			if got := appStatus(tt.incidents, app); got != tt.want {
				t.Errorf("appStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSparkline(t *testing.T) {
	from := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC) // a local hour start in Asia/Tehran
	requests := map[time.Time]int64{
		from:                     5,
		from.Add(23 * time.Hour): 7,
		from.Add(24 * time.Hour): 99, // beyond the 24 hours
		from.Add(-time.Hour):     99, // before them
		from.Add(2 * time.Hour):  3,
	}
	got := sparkline(requests, from)
	if len(got) != 24 || got[0] != 5 || got[2] != 3 || got[23] != 7 {
		t.Errorf("sparkline() = %v, want 5, 3 and 7 in their hours of 24", got)
	}
	var total float64
	for _, value := range got {
		total += value
	}
	if total != 15 {
		t.Errorf("sparkline() sums to %v, want 15 without the hours outside the window", total)
	}
}

func TestFaviconIsLinkedAndServed(t *testing.T) {
	server := newTestServer(t)
	// Pages need a database, but every page's head comes from this template.
	body, err := fs.ReadFile(embedded, "templates/base.html")
	if err != nil {
		t.Fatalf("read base template: %v", err)
	}
	if !strings.Contains(string(body), `rel="icon" href="/static/favicon.ico"`) {
		t.Error("base.html does not link the favicon")
	}
	icon := httptest.NewRecorder()
	server.Handler().ServeHTTP(icon, httptest.NewRequest(http.MethodGet, "/static/favicon.ico", nil))
	if icon.Code != http.StatusOK || icon.Header().Get("Content-Type") != "image/x-icon" || icon.Body.Len() == 0 {
		t.Errorf("favicon status = %d, type = %q, size = %d, want the icon", icon.Code, icon.Header().Get("Content-Type"), icon.Body.Len())
	}
}

// TestTemplatesReferenceExistingStaticFiles catches a page that links a script,
// stylesheet or icon the binary does not embed.
func TestTemplatesReferenceExistingStaticFiles(t *testing.T) {
	reference := regexp.MustCompile(`(?:src|href)="/static/([^"]+)"`)
	templates, err := fs.Glob(embedded, "templates/*.html")
	if err != nil || len(templates) == 0 {
		t.Fatalf("Glob() = %v, %v, want the templates", templates, err)
	}
	for _, name := range templates {
		body, err := fs.ReadFile(embedded, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, match := range reference.FindAllStringSubmatch(string(body), -1) {
			if _, err := fs.Stat(embedded, "static/"+match[1]); err != nil {
				t.Errorf("%s references /static/%s, which is not embedded", name, match[1])
			}
		}
	}
}

// TestModulesImportExistingFiles does the same for the script modules.
func TestModulesImportExistingFiles(t *testing.T) {
	imported := regexp.MustCompile(`from "\./([^"]+)"`)
	modules, err := fs.Glob(embedded, "static/*.js")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	for _, name := range modules {
		body, err := fs.ReadFile(embedded, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, match := range imported.FindAllStringSubmatch(string(body), -1) {
			if _, err := fs.Stat(embedded, "static/"+match[1]); err != nil {
				t.Errorf("%s imports ./%s, which is not embedded", name, match[1])
			}
		}
	}
}
