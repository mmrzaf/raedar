package accesslog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/aggregate"
	"github.com/mmrzaf/raedar/internal/discovery"
)

// testBase is the time the replay test data was recorded at.
var testBase = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

// testClock is a clock that fires waits at once.
type testClock struct {
	now time.Time
}

// Now returns the current time.
func (c *testClock) Now() time.Time { return c.now }

// After returns a ready channel.
func (c *testClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// testModel resolves every router name to the same route.
type testModel struct {
	mu      sync.Mutex
	routeID int64
	ignore  []string
	names   []string
	traefik *discovery.Container
}

// LookupRoute records the name and returns the fixed route.
func (m *testModel) LookupRoute(ctx context.Context, routerName string) (discovery.RouteLookup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names = append(m.names, routerName)
	return discovery.RouteLookup{ID: m.routeID, LatencyIgnore: m.ignore}, nil
}

// TraefikContainer returns the fake Traefik container.
func (m *testModel) TraefikContainer() *discovery.Container { return m.traefik }

// seen returns the router names that were looked up.
func (m *testModel) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.names...)
}

// testState keeps the reader position in memory.
type testState struct {
	mu        sync.Mutex
	position  []byte
	status    string
	lastError string
}

// SourcePosition returns the saved position.
func (s *testState) SourcePosition(context.Context, string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position, nil
}

// RecordSourceStatus records the state.
func (s *testState) RecordSourceStatus(_ context.Context, _, status, lastError string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.lastError = status, lastError
	return nil
}

// statusOf returns the recorded state.
func (s *testState) statusOf() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.lastError
}

// testCounter records the measurements and the last position.
type testCounter struct {
	mu           sync.Mutex
	measurements []aggregate.Measurement
	position     []byte
}

// Add records one measurement.
func (c *testCounter) Add(measurement aggregate.Measurement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.measurements = append(c.measurements, measurement)
}

// SetPosition records the reader position.
func (c *testCounter) SetPosition(position []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.position = position
}

// counted returns the measurements recorded so far.
func (c *testCounter) counted() []aggregate.Measurement {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]aggregate.Measurement(nil), c.measurements...)
}

// lastPosition returns the last reader position.
func (c *testCounter) lastPosition() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.position
}

// waitFor polls until check succeeds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
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

// writeFile writes lines and returns the path.
func writeFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access.log")
	appendFile(t, path, lines...)
	return path
}

// appendFile appends lines to a file.
func appendFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// newSource returns a file-mode source for a test.
func newSource(path string, model Model, state State, counter Counter) *Source {
	source := New(Options{Path: path}, model, state, counter, discardLogger(), &testClock{now: testBase})
	source.pollEvery = time.Millisecond
	return source
}

// sampleLines reads a replay file and keeps the given count of lines.
func sampleLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "accesslog", name))
	if err != nil {
		t.Fatalf("read testdata/accesslog/%s: %v", name, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
