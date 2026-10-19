package aggregate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/store"
)

const (
	// flushDelay is how long after a minute boundary the buckets are written.
	flushDelay = 15 * time.Second
	// lateWindow is how long a minute or window bucket accepts late lines
	// after it ends.
	lateWindow = 5 * time.Minute
	// bufferWindow is how long unwritten buckets are kept while the database
	// is unreachable.
	bufferWindow = 30 * time.Minute
	// MaxPatterns is the number of distinct path patterns kept per route and
	// 10-minute window; further patterns are counted under OtherPattern.
	MaxPatterns = 300
	// windowSize is the length of a path window.
	windowSize = 10 * time.Minute
)

// Measurement is one access log line reduced to what the aggregator counts.
type Measurement struct {
	// RouteID is the route's row ID.
	RouteID int64
	// Time is when the request completed.
	Time time.Time
	// Method is the request method.
	Method string
	// Path is the request path.
	Path string
	// Status is the downstream response status.
	Status int
	// NoBackend reports whether Traefik answered because it could not reach a
	// backend.
	NoBackend bool
	// Bytes is the response size.
	Bytes int64
	// LatencyMS is the request duration in milliseconds.
	LatencyMS float64
	// CountLatency reports whether the duration is added to the histogram;
	// protocol upgrades and ignored paths are not.
	CountLatency bool
}

// Aggregator keeps open per-minute route buckets and per-path window buckets
// and writes them to the database. It is safe for concurrent use.
type Aggregator struct {
	store  *store.Store
	source string
	logger *slog.Logger
	clock  clock.Clock

	mu          sync.Mutex
	minutes     map[routeMinuteKey]*minuteBucket
	windows     map[pathWindowKey]*windowBucket
	patterns    map[routeWindowKey]map[string]bool
	position    []byte
	positionNew bool
	latePending int64
	gaps        []gap
	writeFailed bool
}

// routeMinuteKey identifies one route's minute bucket.
type routeMinuteKey struct {
	routeID int64
	minute  time.Time
}

// routeWindowKey identifies one route's 10-minute window.
type routeWindowKey struct {
	routeID int64
	window  time.Time
}

// pathWindowKey identifies one pattern's window bucket.
type pathWindowKey struct {
	routeID int64
	window  time.Time
	method  string
	pattern string
}

// minuteBucket is the content of one route minute that has not been written
// yet.
type minuteBucket struct {
	requests, s1xx, s2xx, s3xx, s4xx, s5xx int64
	noBackend, bytes                       int64
	latency                                store.Histogram
	dirty                                  bool
}

// windowBucket is the content of one pattern window that has not been written
// yet.
type windowBucket struct {
	requests, s4xx, s5xx int64
	latency              store.Histogram
	dirty                bool
}

// gap is one run of dropped minutes.
type gap struct {
	from, to time.Time
}

// New returns an aggregator that writes through st for the given source.
func New(st *store.Store, source string, logger *slog.Logger, clock clock.Clock) *Aggregator {
	return &Aggregator{
		store:    st,
		source:   source,
		logger:   logger,
		clock:    clock,
		minutes:  make(map[routeMinuteKey]*minuteBucket),
		windows:  make(map[pathWindowKey]*windowBucket),
		patterns: make(map[routeWindowKey]map[string]bool),
	}
}

// Add counts one measurement. A measurement whose minute closed more than
// lateWindow ago is counted in the oldest open minute.
func (a *Aggregator) Add(measurement Measurement) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now()
	minute := measurement.Time.UTC().Truncate(time.Minute)
	if current := now.Truncate(time.Minute); minute.After(current) {
		minute = current
	}
	if now.Sub(minute) > lateWindow+time.Minute {
		if oldest, ok := a.oldestMinute(); ok {
			minute = oldest
			a.latePending++
		}
	}
	key := routeMinuteKey{routeID: measurement.RouteID, minute: minute}
	bucket := a.minutes[key]
	if bucket == nil {
		bucket = &minuteBucket{}
		a.minutes[key] = bucket
	}
	bucket.add(measurement)
	windowKey := routeWindowKey{routeID: measurement.RouteID, window: minute.Truncate(windowSize)}
	pathKey := pathWindowKey{
		routeID: measurement.RouteID,
		window:  windowKey.window,
		method:  measurement.Method,
		pattern: a.pattern(windowKey, measurement),
	}
	window := a.windows[pathKey]
	if window == nil {
		window = &windowBucket{}
		a.windows[pathKey] = window
	}
	window.add(measurement)
}

// SetPosition records how far the source has read; the position is written
// with the next flush.
func (a *Aggregator) SetPosition(position []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.position = position
	a.positionNew = true
}

// Flush writes the accumulated buckets, the source position and any gap
// events in one transaction and clears what it wrote. Buckets stay open for
// late lines. Buckets whose content stayed unwritten for bufferWindow are
// dropped and recorded as a gap.
func (a *Aggregator) Flush(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now()
	if a.writeFailed {
		a.dropOldest(now)
	}
	flush := store.Flush{
		Source: a.source,
		Gaps:   a.gapEvents(now),
		At:     now,
	}
	if a.positionNew {
		flush.Position = a.position
	}
	for key, bucket := range a.minutes {
		if !bucket.dirty {
			continue
		}
		flush.Minutes = append(flush.Minutes, store.RouteMinute{
			RouteID:   key.routeID,
			TS:        key.minute,
			Requests:  bucket.requests,
			S1xx:      bucket.s1xx,
			S2xx:      bucket.s2xx,
			S3xx:      bucket.s3xx,
			S4xx:      bucket.s4xx,
			S5xx:      bucket.s5xx,
			NoBackend: bucket.noBackend,
			Bytes:     bucket.bytes,
			Latency:   bucket.latency.Counts(),
		})
	}
	for key, bucket := range a.windows {
		if !bucket.dirty {
			continue
		}
		flush.Windows = append(flush.Windows, store.PathWindow{
			RouteID:  key.routeID,
			TS:       key.window,
			Method:   key.method,
			Pattern:  key.pattern,
			Requests: bucket.requests,
			S4xx:     bucket.s4xx,
			S5xx:     bucket.s5xx,
			Latency:  bucket.latency.Counts(),
		})
	}
	sortMinuteRows(flush.Minutes)
	sortWindowRows(flush.Windows)
	if len(flush.Minutes) == 0 && len(flush.Windows) == 0 && len(flush.Position) == 0 && len(flush.Gaps) == 0 {
		return nil
	}
	if err := a.store.FlushBuckets(ctx, flush); err != nil {
		a.writeFailed = true
		return err
	}
	a.writeFailed = false
	for _, bucket := range a.minutes {
		if bucket.dirty {
			bucket.clear()
		}
	}
	for _, bucket := range a.windows {
		if bucket.dirty {
			bucket.clear()
		}
	}
	a.positionNew = false
	a.gaps = nil
	if a.latePending > 0 {
		a.logger.Warn("late access log lines", slog.Int64("count", a.latePending))
		a.latePending = 0
	}
	a.prune(now)
	return nil
}

// Run writes the buckets fifteen seconds after every minute boundary until
// ctx is done.
func (a *Aggregator) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.clock.After(a.nextFlushDelay()):
		}
		if err := a.Flush(ctx); err != nil {
			a.logger.Warn("flush access log buckets", slog.String("error", err.Error()))
		}
	}
}

// nextFlushDelay returns how long to wait for the next flush.
func (a *Aggregator) nextFlushDelay() time.Duration {
	now := a.clock.Now()
	return now.Truncate(time.Minute).Add(time.Minute + flushDelay).Sub(now)
}

// add counts one measurement in the minute bucket.
func (b *minuteBucket) add(measurement Measurement) {
	b.requests++
	b.bytes += measurement.Bytes
	switch statusClass(measurement.Status) {
	case 1:
		b.s1xx++
	case 2:
		b.s2xx++
	case 3:
		b.s3xx++
	case 4:
		b.s4xx++
	case 5:
		b.s5xx++
	}
	if measurement.NoBackend {
		b.noBackend++
	}
	if measurement.CountLatency {
		b.latency.Add(measurement.LatencyMS)
	}
	b.dirty = true
}

// clear resets the content a flush wrote.
func (b *minuteBucket) clear() {
	*b = minuteBucket{}
}

// add counts one measurement in the pattern window bucket.
func (b *windowBucket) add(measurement Measurement) {
	b.requests++
	switch statusClass(measurement.Status) {
	case 4:
		b.s4xx++
	case 5:
		b.s5xx++
	}
	if measurement.CountLatency {
		b.latency.Add(measurement.LatencyMS)
	}
	b.dirty = true
}

// clear resets the content a flush wrote.
func (b *windowBucket) clear() {
	*b = windowBucket{}
}

// statusClass returns the status class of a response, 0 when the status is
// unknown.
func statusClass(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status / 100
}

// pattern returns the pattern a measurement counts under, applying the cap on
// distinct patterns per route and window.
func (a *Aggregator) pattern(key routeWindowKey, measurement Measurement) string {
	if measurement.Status == 404 {
		return NotFoundPattern
	}
	pattern := NormalizePath(measurement.Path)
	known := a.patterns[key]
	if known == nil {
		known = make(map[string]bool)
		a.patterns[key] = known
	}
	if known[pattern] {
		return pattern
	}
	if len(known) >= MaxPatterns {
		return OtherPattern
	}
	known[pattern] = true
	return pattern
}

// oldestMinute returns the oldest open minute bucket.
func (a *Aggregator) oldestMinute() (time.Time, bool) {
	var oldest time.Time
	found := false
	for key := range a.minutes {
		if !found || key.minute.Before(oldest) {
			oldest, found = key.minute, true
		}
	}
	return oldest, found
}

// prune removes the buckets that are closed and fully written.
func (a *Aggregator) prune(now time.Time) {
	for key, bucket := range a.minutes {
		if !bucket.dirty && now.After(key.minute.Add(time.Minute+lateWindow)) {
			delete(a.minutes, key)
		}
	}
	for key, bucket := range a.windows {
		if !bucket.dirty && now.After(key.window.Add(windowSize+lateWindow)) {
			delete(a.windows, key)
		}
	}
	for key := range a.patterns {
		if now.After(key.window.Add(windowSize + lateWindow)) {
			delete(a.patterns, key)
		}
	}
}

// dropOldest drops the minute buckets whose content stayed unwritten for
// bufferWindow and records the gap they leave.
func (a *Aggregator) dropOldest(now time.Time) {
	for key, bucket := range a.minutes {
		if !bucket.dirty || now.Before(key.minute.Add(time.Minute+bufferWindow)) {
			continue
		}
		a.addGap(key.minute)
		delete(a.minutes, key)
	}
	for key, bucket := range a.windows {
		if !bucket.dirty || now.Before(key.window.Add(windowSize+bufferWindow)) {
			continue
		}
		delete(a.windows, key)
	}
}

// addGap extends the pending gap to cover one dropped minute.
func (a *Aggregator) addGap(minute time.Time) {
	if len(a.gaps) == 0 {
		a.gaps = append(a.gaps, gap{from: minute, to: minute})
		return
	}
	last := &a.gaps[len(a.gaps)-1]
	if minute.After(last.to) && minute.Sub(last.to) > time.Minute {
		a.gaps = append(a.gaps, gap{from: minute, to: minute})
		return
	}
	if minute.Before(last.from) {
		last.from = minute
	}
	if minute.After(last.to) {
		last.to = minute
	}
}

// gapEvents returns one event per run of dropped minutes.
func (a *Aggregator) gapEvents(now time.Time) []store.Event {
	events := make([]store.Event, 0, len(a.gaps))
	for _, dropped := range a.gaps {
		minutes := int(dropped.to.Sub(dropped.from)/time.Minute) + 1
		payload, err := json.Marshal(struct {
			From    time.Time `json:"from"`
			To      time.Time `json:"to"`
			Minutes int       `json:"minutes"`
		}{From: dropped.from, To: dropped.to, Minutes: minutes})
		if err != nil {
			a.logger.Warn("encode gap event", slog.String("error", err.Error()))
			continue
		}
		events = append(events, store.Event{
			TS:       now,
			Kind:     store.EventGap,
			Severity: store.SeverityWarning,
			Subject:  a.source,
			Title:    fmt.Sprintf("dropped %s of traffic", minutesText(minutes)),
			Data:     payload,
		})
	}
	return events
}

// minutesText returns "1 minute" or "N minutes".
func minutesText(minutes int) string {
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// sortMinuteRows orders the flush rows by route and minute.
func sortMinuteRows(rows []store.RouteMinute) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TS.Equal(rows[j].TS) {
			return rows[i].RouteID < rows[j].RouteID
		}
		return rows[i].TS.Before(rows[j].TS)
	})
}

// sortWindowRows orders the flush rows by window, route, method and pattern.
func sortWindowRows(rows []store.PathWindow) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].TS.Equal(rows[j].TS) {
			return rows[i].TS.Before(rows[j].TS)
		}
		if rows[i].RouteID != rows[j].RouteID {
			return rows[i].RouteID < rows[j].RouteID
		}
		if rows[i].Method != rows[j].Method {
			return rows[i].Method < rows[j].Method
		}
		return rows[i].Pattern < rows[j].Pattern
	})
}

// Delta is one minute bucket whose content was not written yet.
type Delta struct {
	// RouteID is the route.
	RouteID int64
	// TS is the start of the minute.
	TS time.Time
	// Requests counts all requests.
	Requests int64
	// S1xx to S5xx count the responses by status class.
	S1xx, S2xx, S3xx, S4xx, S5xx int64
	// NoBackend counts requests Traefik answered without a backend.
	NoBackend int64
	// Bytes is the total response size.
	Bytes int64
	// Latency is the latency histogram.
	Latency store.Histogram
}

// Deltas returns the unwritten minute buckets at or after from.
func (a *Aggregator) Deltas(from time.Time) []Delta {
	a.mu.Lock()
	defer a.mu.Unlock()
	var deltas []Delta
	for key, bucket := range a.minutes {
		if !bucket.dirty || key.minute.Before(from) {
			continue
		}
		deltas = append(deltas, Delta{
			RouteID:   key.routeID,
			TS:        key.minute,
			Requests:  bucket.requests,
			S1xx:      bucket.s1xx,
			S2xx:      bucket.s2xx,
			S3xx:      bucket.s3xx,
			S4xx:      bucket.s4xx,
			S5xx:      bucket.s5xx,
			NoBackend: bucket.noBackend,
			Bytes:     bucket.bytes,
			Latency:   bucket.latency,
		})
	}
	sort.Slice(deltas, func(i, j int) bool {
		if deltas[i].TS.Equal(deltas[j].TS) {
			return deltas[i].RouteID < deltas[j].RouteID
		}
		return deltas[i].TS.Before(deltas[j].TS)
	})
	return deltas
}
