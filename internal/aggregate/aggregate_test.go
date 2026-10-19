package aggregate

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// testClock is a clock the tests move by hand.
type testClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
}

// clockWaiter is one registered wait.
type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

// newTestClock returns a clock stopped at the given time.
func newTestClock(at time.Time) *testClock {
	return &testClock{now: at}
}

// Now returns the current time.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After registers a waiter that fires when the clock passes c.now + d.
func (c *testClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and fires the waiters whose time has come.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.waiters[:0]
	for _, waiter := range c.waiters {
		if waiter.at.After(c.now) {
			remaining = append(remaining, waiter)
			continue
		}
		waiter.ch <- c.now
	}
	c.waiters = remaining
}

// pending returns the number of registered waiters.
func (c *testClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testBase is the base time of the aggregator tests; it is a 10-minute
// boundary.
var testBase = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

// newTestAggregator returns an aggregator with a nil store; only Add, prune
// and the gap bookkeeping may be used.
func newTestAggregator(clock *testClock) *Aggregator {
	return New(nil, "accesslog", discardLogger(), clock)
}

func TestTestClock(t *testing.T) {
	clock := newTestClock(testBase)
	if clock.pending() != 0 {
		t.Fatalf("pending = %d, want 0", clock.pending())
	}
	ready := clock.After(time.Minute)
	if clock.pending() != 1 {
		t.Fatalf("pending = %d, want 1", clock.pending())
	}
	clock.Advance(30 * time.Second)
	select {
	case <-ready:
		t.Fatal("the waiter fired before its time")
	default:
	}
	clock.Advance(30 * time.Second)
	if clock.pending() != 0 {
		t.Fatalf("pending = %d, want 0 after the advance", clock.pending())
	}
	select {
	case at := <-ready:
		if want := testBase.Add(time.Minute); !at.Equal(want) {
			t.Errorf("waiter fired at %v, want %v", at, want)
		}
	default:
		t.Fatal("the waiter did not fire")
	}
	if got := clock.Now(); !got.Equal(testBase.Add(time.Minute)) {
		t.Errorf("Now() = %v, want %v", got, testBase.Add(time.Minute))
	}
}

func TestAddCountsMinuteAndWindow(t *testing.T) {
	clock := newTestClock(testBase.Add(5 * time.Second))
	aggregator := newTestAggregator(clock)
	aggregator.Add(Measurement{
		RouteID:      7,
		Time:         testBase.Add(5 * time.Second),
		Method:       "GET",
		Path:         "/api/orders/123",
		Status:       200,
		Bytes:        1024,
		LatencyMS:    12,
		CountLatency: true,
	})
	minute := aggregator.minutes[routeMinuteKey{routeID: 7, minute: testBase}]
	if minute == nil {
		t.Fatal("no minute bucket")
	}
	if minute.requests != 1 || minute.s2xx != 1 || minute.bytes != 1024 {
		t.Errorf("minute bucket = %+v, want one 200 with 1024 bytes", minute)
	}
	if minute.latency.Count() != 1 {
		t.Errorf("minute histogram count = %d, want 1", minute.latency.Count())
	}
	if !minute.dirty {
		t.Error("minute bucket is not dirty")
	}
	window := aggregator.windows[pathWindowKey{
		routeID: 7, window: testBase, method: "GET", pattern: "/api/orders/:n",
	}]
	if window == nil {
		t.Fatal("no window bucket for the normalized pattern")
	}
	if window.requests != 1 || window.latency.Count() != 1 {
		t.Errorf("window bucket = %+v, want one request with latency", window)
	}
}

func TestAddCountsStatusClasses(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		noBackend      bool
		wantS1xx       int64
		wantS2xx       int64
		wantS3xx       int64
		wantS4xx       int64
		wantS5xx       int64
		wantNoBackend  int64
		wantWindowS4xx int64
		wantWindowS5xx int64
	}{
		{name: "informational", status: 101, wantS1xx: 1},
		{name: "success", status: 200, wantS2xx: 1},
		{name: "redirect", status: 301, wantS3xx: 1},
		{name: "client error", status: 404, wantS4xx: 1, wantWindowS4xx: 1},
		{name: "server error", status: 500, wantS5xx: 1, wantWindowS5xx: 1},
		{name: "no backend", status: 503, noBackend: true, wantS5xx: 1, wantNoBackend: 1, wantWindowS5xx: 1},
		{name: "unknown status", status: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newTestClock(testBase)
			aggregator := newTestAggregator(clock)
			aggregator.Add(Measurement{
				RouteID:   1,
				Time:      testBase,
				Method:    "GET",
				Path:      "/api/orders/1",
				Status:    tt.status,
				NoBackend: tt.noBackend,
			})
			minute := aggregator.minutes[routeMinuteKey{routeID: 1, minute: testBase}]
			if minute.s1xx != tt.wantS1xx || minute.s2xx != tt.wantS2xx || minute.s3xx != tt.wantS3xx ||
				minute.s4xx != tt.wantS4xx || minute.s5xx != tt.wantS5xx || minute.noBackend != tt.wantNoBackend {
				t.Errorf("minute bucket = %+v, want classes %d/%d/%d/%d/%d and %d without backend",
					minute, tt.wantS1xx, tt.wantS2xx, tt.wantS3xx, tt.wantS4xx, tt.wantS5xx, tt.wantNoBackend)
			}
			window := aggregator.windows[pathWindowKey{routeID: 1, window: testBase, method: "GET", pattern: patternOf(tt.status, "/api/orders/1")}]
			if window == nil {
				t.Fatal("no window bucket")
			}
			if window.s4xx != tt.wantWindowS4xx || window.s5xx != tt.wantWindowS5xx {
				t.Errorf("window bucket = %+v, want %d 4xx and %d 5xx", window, tt.wantWindowS4xx, tt.wantWindowS5xx)
			}
		})
	}
}

// patternOf returns the pattern a status and path are counted under.
func patternOf(status int, path string) string {
	if status == 404 {
		return NotFoundPattern
	}
	return NormalizePath(path)
}

func TestAddSkipsExcludedLatency(t *testing.T) {
	clock := newTestClock(testBase)
	aggregator := newTestAggregator(clock)
	aggregator.Add(Measurement{
		RouteID:   1,
		Time:      testBase,
		Method:    "GET",
		Path:      "/ws",
		Status:    101,
		LatencyMS: 45000,
	})
	minute := aggregator.minutes[routeMinuteKey{routeID: 1, minute: testBase}]
	if minute.requests != 1 {
		t.Errorf("requests = %d, want 1", minute.requests)
	}
	if minute.latency.Count() != 0 {
		t.Errorf("histogram count = %d, want 0 for an excluded request", minute.latency.Count())
	}
}

func TestAddCapsPatternsPerWindow(t *testing.T) {
	clock := newTestClock(testBase)
	aggregator := newTestAggregator(clock)
	for i := 0; i < MaxPatterns; i++ {
		aggregator.Add(Measurement{
			RouteID: 1,
			Time:    testBase,
			Method:  "GET",
			Path:    fmt.Sprintf("/pattern-%d", i),
			Status:  200,
		})
	}
	windowKey := func(pattern string) pathWindowKey {
		return pathWindowKey{routeID: 1, window: testBase, method: "GET", pattern: pattern}
	}
	if got := len(aggregator.patterns[routeWindowKey{routeID: 1, window: testBase}]); got != MaxPatterns {
		t.Fatalf("known patterns = %d, want %d", got, MaxPatterns)
	}
	aggregator.Add(Measurement{RouteID: 1, Time: testBase, Method: "GET", Path: "/one-too-many", Status: 200})
	if aggregator.windows[windowKey("/one-too-many")] != nil {
		t.Error("the pattern beyond the cap was recorded")
	}
	other := aggregator.windows[windowKey(OtherPattern)]
	if other == nil || other.requests != 1 {
		t.Errorf("other bucket = %+v, want one request", other)
	}
	// A pattern that was seen before the cap is counted as itself.
	aggregator.Add(Measurement{RouteID: 1, Time: testBase, Method: "GET", Path: "/pattern-0", Status: 200})
	if bucket := aggregator.windows[windowKey("/pattern-0")]; bucket == nil || bucket.requests != 2 {
		t.Errorf("known pattern bucket = %+v, want two requests", bucket)
	}
}

func TestAddCountsNotFoundUnderOnePattern(t *testing.T) {
	clock := newTestClock(testBase)
	aggregator := newTestAggregator(clock)
	for _, path := range []string{"/random/xyz", "/.env", "/wp-login.php"} {
		aggregator.Add(Measurement{RouteID: 1, Time: testBase, Method: "GET", Path: path, Status: 404})
	}
	window := aggregator.windows[pathWindowKey{routeID: 1, window: testBase, method: "GET", pattern: NotFoundPattern}]
	if window == nil || window.requests != 3 {
		t.Errorf("not found bucket = %+v, want three requests", window)
	}
	if len(aggregator.patterns[routeWindowKey{routeID: 1, window: testBase}]) != 0 {
		t.Errorf("patterns = %v, want (not found) to stay outside the cap", aggregator.patterns)
	}
}

func TestAddPlacesLateLines(t *testing.T) {
	clock := newTestClock(testBase.Add(10 * time.Minute))
	aggregator := newTestAggregator(clock)
	at := func(offset time.Duration) Measurement {
		return Measurement{RouteID: 1, Time: testBase.Add(offset), Method: "GET", Path: "/api", Status: 200}
	}
	aggregator.Add(at(9 * time.Minute))  // 10:09, one minute late
	aggregator.Add(at(5 * time.Minute))  // 10:05, five minutes late
	aggregator.Add(at(3 * time.Minute))  // 10:03, seven minutes late
	aggregator.Add(at(30 * time.Minute)) // 10:30, in the future
	if got := len(aggregator.minutes); got != 3 {
		t.Fatalf("buckets = %d, want 3", got)
	}
	want := map[time.Duration]int64{
		9 * time.Minute:  1,
		5 * time.Minute:  2, // the late line and the line clamped to the oldest minute
		10 * time.Minute: 1, // the future line, clamped to the current minute
	}
	for offset, requests := range want {
		key := routeMinuteKey{routeID: 1, minute: testBase.Add(offset)}
		if bucket := aggregator.minutes[key]; bucket == nil || bucket.requests != requests {
			t.Errorf("bucket for offset %v = %+v, want %d requests", offset, bucket, requests)
		}
	}
	if aggregator.latePending != 1 {
		t.Errorf("late lines = %d, want 1", aggregator.latePending)
	}
}

func TestFlushWithoutContentDoesNothing(t *testing.T) {
	clock := newTestClock(testBase)
	aggregator := newTestAggregator(clock)
	if err := aggregator.Flush(t.Context()); err != nil {
		t.Fatalf("Flush() error = %v, want nil", err)
	}
}

func TestDropOldestRecordsGaps(t *testing.T) {
	clock := newTestClock(testBase.Add(2 * time.Minute))
	aggregator := newTestAggregator(clock)
	for minute := 0; minute < 2; minute++ {
		aggregator.Add(Measurement{
			RouteID: 1, Time: testBase.Add(time.Duration(minute) * time.Minute),
			Method: "GET", Path: "/api", Status: 200,
		})
	}
	aggregator.dropOldest(testBase.Add(33 * time.Minute))
	if len(aggregator.minutes) != 0 {
		t.Errorf("buckets = %d, want the old ones dropped", len(aggregator.minutes))
	}
	events := aggregator.gapEvents(testBase.Add(40 * time.Minute))
	if len(events) != 1 {
		t.Fatalf("gap events = %+v, want 1", events)
	}
	if events[0].Title != "dropped 2 minutes of traffic" {
		t.Errorf("title = %q, want the dropped minutes", events[0].Title)
	}
	if events[0].Kind != "gap" || events[0].Severity != "warning" || events[0].Subject != "accesslog" {
		t.Errorf("event = %+v, want a warning gap event for the access log", events[0])
	}
	if events[0].Data == nil {
		t.Error("gap event has no payload")
	}
}

func TestDropOldestKeepsYoungBuckets(t *testing.T) {
	clock := newTestClock(testBase)
	aggregator := newTestAggregator(clock)
	aggregator.Add(Measurement{RouteID: 1, Time: testBase, Method: "GET", Path: "/api", Status: 200})
	aggregator.dropOldest(testBase.Add(20 * time.Minute))
	if len(aggregator.minutes) != 1 {
		t.Errorf("buckets = %d, want the young bucket kept", len(aggregator.minutes))
	}
	if len(aggregator.gaps) != 0 {
		t.Errorf("gaps = %+v, want none", aggregator.gaps)
	}
}

func TestPruneRemovesClosedBuckets(t *testing.T) {
	clock := newTestClock(testBase)
	aggregator := newTestAggregator(clock)
	aggregator.Add(Measurement{RouteID: 1, Time: testBase, Method: "GET", Path: "/api", Status: 200})
	// A written minute bucket is removed once its late window passed; the
	// window bucket lives until its own late window ends.
	aggregator.minutes[routeMinuteKey{routeID: 1, minute: testBase}].dirty = false
	aggregator.windows[pathWindowKey{routeID: 1, window: testBase, method: "GET", pattern: "/api"}].dirty = false
	aggregator.prune(testBase.Add(7 * time.Minute))
	if len(aggregator.minutes) != 0 {
		t.Errorf("buckets = %d, want the written bucket removed", len(aggregator.minutes))
	}
	if len(aggregator.windows) != 1 {
		t.Errorf("windows = %d, want the window kept until its late window ends", len(aggregator.windows))
	}
	aggregator.prune(testBase.Add(16 * time.Minute))
	if len(aggregator.windows) != 0 {
		t.Errorf("windows = %d, want the closed window removed", len(aggregator.windows))
	}
	if len(aggregator.patterns) != 0 {
		t.Errorf("patterns = %d, want the closed window removed", len(aggregator.patterns))
	}

	// A bucket with unwritten content stays.
	clock = newTestClock(testBase.Add(11 * time.Minute))
	aggregator = newTestAggregator(clock)
	aggregator.Add(Measurement{RouteID: 1, Time: testBase.Add(10 * time.Minute), Method: "GET", Path: "/api", Status: 200})
	aggregator.prune(testBase.Add(17 * time.Minute))
	if len(aggregator.minutes) != 1 {
		t.Errorf("buckets = %d, want the unwritten bucket kept", len(aggregator.minutes))
	}
}

func TestNextFlushDelay(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{name: "at the boundary", now: testBase, want: time.Minute + flushDelay},
		{name: "before the flush", now: testBase.Add(30 * time.Second), want: 45 * time.Second},
		{name: "after the flush", now: testBase.Add(20 * time.Second), want: 55 * time.Second},
		{name: "at the flush", now: testBase.Add(flushDelay), want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aggregator := New(nil, "accesslog", discardLogger(), newTestClock(tt.now))
			if got := aggregator.nextFlushDelay(); got != tt.want {
				t.Errorf("nextFlushDelay() = %v, want %v", got, tt.want)
			}
		})
	}
}
