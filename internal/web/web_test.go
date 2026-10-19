package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
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
		{name: "series without keys", path: "/api/series", status: 400, want: "keys"},
		{name: "series with a bad range", path: "/api/series?keys=host:cpu&from=x&to=y", status: 400, want: "from and to"},
		{name: "series with a reversed range", path: "/api/series?keys=host:cpu&from=100&to=50", status: 400, want: "after"},
		{name: "series with too many points", path: "/api/series?keys=host:cpu&from=100&to=200&points=99999", status: 400, want: "points"},
		{name: "series with an unknown kind", path: "/api/series?keys=foo:bar:baz&from=100&to=200", status: 400, want: "unknown kind"},
		{name: "baseline without a route", path: "/api/baseline", status: 400, want: "route"},
		{name: "paths without a subject", path: "/api/paths?from=100&to=200", status: 400, want: "app or route"},
		{name: "paths with a bad range", path: "/api/paths?app=x", status: 400, want: "from and to"},
		{name: "events with a bad range", path: "/api/events?from=1", status: 400, want: "from and to"},
		{name: "patterns without a key", path: "/api/patterns", status: 400, want: "key"},
		{name: "patterns with too many weeks", path: "/api/patterns?key=host:load1:load1&weeks=100", status: 400, want: "weeks"},
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

func TestParseSeriesKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		want    seriesKey
		wantErr bool
	}{
		{name: "route", key: "route:shop@docker:p95", want: seriesKey{kind: "route", subject: "shop@docker", metric: "p95"}},
		{name: "slot", key: "slot:shop / api #1:cpu", want: seriesKey{kind: "slot", subject: "shop / api #1", metric: "cpu"}},
		{name: "host", key: "host:load1", want: seriesKey{kind: "host", subject: "", metric: "load1"}},
		{name: "disk", key: "disk:/:used", want: seriesKey{kind: "disk", subject: "/", metric: "used"}},
		{name: "unknown kind", key: "foo:bar:baz", wantErr: true},
		{name: "too short", key: "host", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSeriesKey(tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseSeriesKey() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSeriesKey() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("parseSeriesKey(%q) = %+v, want %+v", tt.key, got, tt.want)
			}
		})
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

func TestStreamLines(t *testing.T) {
	server := newTestServer(t)
	var written strings.Builder
	until := time.Date(2026, 10, 18, 12, 2, 0, 0, time.UTC)
	logs := strings.NewReader("2026-10-18T12:00:00Z first\n2026-10-18T12:03:00Z after\n")
	server.streamLines(&stringBuilderWriter{&written}, logs, until)
	if !strings.Contains(written.String(), "first") {
		t.Errorf("events = %q, want the first line", written.String())
	}
	if strings.Contains(written.String(), "after") {
		t.Error("a line after the window was streamed")
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
	server.cfg.Notify.SMS.Provider = "kavenegar"
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
