package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

// fakeStore records heartbeats.
type fakeStore struct {
	mu   sync.Mutex
	beat map[string]time.Time
}

func (s *fakeStore) RecordHeartbeat(ctx context.Context, name string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beat == nil {
		s.beat = make(map[string]time.Time)
	}
	s.beat[name] = at
	return nil
}

// TestReceiver checks the peer list, the token and the rate limit.
func TestReceiver(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}
	st := &fakeStore{}
	receiver := NewReceiver([]string{"host-2"}, "token", st, discardLogger(), clock)
	tests := []struct {
		name          string
		method        string
		path          string
		authorization string
		wantStatus    int
	}{
		{name: "accepted", method: http.MethodGet, path: "/heartbeat/host-2", authorization: "Bearer token", wantStatus: http.StatusNoContent},
		{name: "post accepted", method: http.MethodPost, path: "/heartbeat/host-2", authorization: "Bearer token", wantStatus: http.StatusTooManyRequests},
		{name: "unknown peer", method: http.MethodGet, path: "/heartbeat/host-9", authorization: "Bearer token", wantStatus: http.StatusNotFound},
		{name: "missing token", method: http.MethodGet, path: "/heartbeat/host-2", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", method: http.MethodGet, path: "/heartbeat/host-2", authorization: "Bearer other", wantStatus: http.StatusUnauthorized},
		{name: "wrong method", method: http.MethodPut, path: "/heartbeat/host-2", authorization: "Bearer token", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.authorization != "" {
				request.Header.Set("Authorization", tt.authorization)
			}
			recorder := httptest.NewRecorder()
			receiver.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
	if _, ok := st.beat["host-2"]; !ok {
		t.Error("the heartbeat was not recorded")
	}
	// A heartbeat after the peer interval is accepted again.
	clock.now = clock.now.Add(5 * time.Second)
	request := httptest.NewRequest(http.MethodGet, "/heartbeat/host-2", nil)
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	receiver.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Errorf("status = %d, want the heartbeat after the interval to be accepted", recorder.Code)
	}
}

// TestSender pings the URL only while healthy.
func TestSender(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []*http.Request
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Clone(context.Background()))
		mu.Unlock()
	}))
	defer server.Close()
	cfg := testConfig(t)
	cfg.Heartbeat.URL = server.URL + "/ping/secret-token"
	cfg.Heartbeat.Interval = time.Minute
	cfg.Heartbeat.Header = &config.Header{Name: "Authorization", Value: "Bearer token"}
	healthy := true
	sender := NewSender(cfg, server.Client(), discardLogger(), &testClock{now: time.Now()}, func(context.Context) bool { return healthy })
	sender.beat(context.Background())
	mu.Lock()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization = %q, want the configured header", got)
	}
	mu.Unlock()
	healthy = false
	sender.beat(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Errorf("requests = %d, want no heartbeat while unhealthy", len(requests))
	}
}

// TestSafeErrorRemovesTheURL checks that failures stay silent about URLs
// that contain tokens.
func TestSafeErrorRemovesTheURL(t *testing.T) {
	wrapped := &url.Error{Op: "Get", URL: "http://example.com/ping/very-secret-token", Err: errors.New("connection refused")}
	got := SafeError(wrapped)
	if strings.Contains(got.Error(), "very-secret-token") {
		t.Errorf("error = %q, want the URL removed", got.Error())
	}
	if !strings.Contains(got.Error(), "connection refused") {
		t.Errorf("error = %q, want the underlying error", got.Error())
	}
}
