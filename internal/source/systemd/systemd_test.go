package systemd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/store"
)

// testClock is a clock the tests move by hand.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

// fakeReader returns a fixed unit list.
type fakeReader struct {
	mu    sync.Mutex
	units []Unit
	err   error
}

func (f *fakeReader) ListUnits(context.Context) ([]Unit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]Unit(nil), f.units...), nil
}

func (f *fakeReader) set(units ...Unit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.units = units
}

// fakeStore records units, events and the status.
type fakeStore struct {
	mu      sync.Mutex
	units   map[string]store.Unit
	events  []store.Event
	known   []string
	status  string
	lastErr string
}

func (f *fakeStore) UpsertUnit(ctx context.Context, unit store.Unit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.units == nil {
		f.units = make(map[string]store.Unit)
	}
	f.units[unit.Name] = unit
	return nil
}

func (f *fakeStore) UnitNames(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.known...), nil
}

func (f *fakeStore) AddEvent(ctx context.Context, event store.Event) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return int64(len(f.events)), nil
}

func (f *fakeStore) RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.lastErr = status, lastError
	return nil
}

// statusOf returns the recorded status.
func (f *fakeStore) statusOf() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.lastErr
}

// eventKinds returns the recorded event kinds.
func (f *fakeStore) eventKinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	kinds := make([]string, 0, len(f.events))
	for _, event := range f.events {
		kinds = append(kinds, string(event.Kind))
	}
	return kinds
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMatches(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		unit     string
		want     bool
	}{
		{name: "exact", patterns: []string{"man-db.service"}, unit: "man-db.service", want: true},
		{name: "exact miss", patterns: []string{"man-db.service"}, unit: "man-db.timer"},
		{name: "prefix wildcard", patterns: []string{"apt-daily*.service"}, unit: "apt-daily-upgrade.service", want: true},
		{name: "suffix wildcard", patterns: []string{"*-backup.service"}, unit: "nightly-backup.service", want: true},
		{name: "middle wildcard", patterns: []string{"backup-*.service"}, unit: "backup-nightly.service", want: true},
		{name: "no wildcard match", patterns: []string{"apt-daily*.service"}, unit: "man-db.service"},
		{name: "star matches everything", patterns: []string{"*"}, unit: "anything.service", want: true},
		{name: "several patterns", patterns: []string{"a*.service", "b*.service"}, unit: "b1.service", want: true},
		{name: "empty", patterns: nil, unit: "a.service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Matches(tt.patterns, tt.unit); got != tt.want {
				t.Errorf("Matches(%v, %q) = %v, want %v", tt.patterns, tt.unit, got, tt.want)
			}
		})
	}
}

func TestCollectRecordsFailedUnits(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	exit := 1
	since := base.Add(-time.Hour)
	reader := &fakeReader{units: []Unit{
		{Name: "backup.service", ActiveState: "failed", SubState: "failed", Result: "exit-code", ExitStatus: &exit, Since: &since},
		{Name: "web.service", ActiveState: "active", SubState: "running"},
		{Name: "apt-daily.service", ActiveState: "failed", SubState: "failed"},
	}}
	st := &fakeStore{}
	source := New(reader, st, discardLogger(), &testClock{now: base}, []string{"apt-daily*.service"}, nil)
	if err := source.loadKnown(context.Background()); err != nil {
		t.Fatalf("loadKnown() error = %v", err)
	}
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	st.mu.Lock()
	unit, ok := st.units["backup.service"]
	st.mu.Unlock()
	if !ok {
		t.Fatal("failed unit was not recorded")
	}
	if unit.ActiveState != "failed" || unit.Result != "exit-code" || unit.ExitStatus == nil || *unit.ExitStatus != 1 {
		t.Errorf("unit = %+v, want the failed details", unit)
	}
	st.mu.Lock()
	if _, ok := st.units["web.service"]; ok {
		t.Error("healthy unit was recorded")
	}
	if _, ok := st.units["apt-daily.service"]; ok {
		t.Error("ignored unit was recorded")
	}
	st.mu.Unlock()
	if kinds := st.eventKinds(); len(kinds) != 1 || kinds[0] != "unit_failed" {
		t.Fatalf("events = %v, want one unit_failed event", kinds)
	}
	st.mu.Lock()
	event := st.events[0]
	st.mu.Unlock()
	if event.Severity != "warning" || event.Subject != "backup.service" || event.Title != "unit failed: exit-code" {
		t.Errorf("event = %+v, want the failed unit", event)
	}

	// The same state on the next pass does not repeat the event.
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("second collect() error = %v", err)
	}
	if kinds := st.eventKinds(); len(kinds) != 1 {
		t.Errorf("events = %v, want no repeated event", kinds)
	}

	// Recovery updates the row and records one event.
	reader.set(Unit{Name: "backup.service", ActiveState: "active", SubState: "dead"})
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("third collect() error = %v", err)
	}
	st.mu.Lock()
	unit = st.units["backup.service"]
	st.mu.Unlock()
	if unit.ActiveState != "active" {
		t.Errorf("unit = %+v, want the recovered state", unit)
	}
	if kinds := st.eventKinds(); len(kinds) != 2 || kinds[1] != "unit_recovered" {
		t.Errorf("events = %v, want a recovery event", kinds)
	}
}

func TestCollectUsesCriticalSeverity(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	reader := &fakeReader{units: []Unit{{Name: "nightly-backup.service", ActiveState: "failed", SubState: "failed"}}}
	st := &fakeStore{}
	source := New(reader, st, discardLogger(), &testClock{now: base}, nil, []string{"*-backup.service"})
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	st.mu.Lock()
	severity := st.events[0].Severity
	st.mu.Unlock()
	if severity != "critical" {
		t.Errorf("severity = %q, want critical", severity)
	}
}

func TestCollectKeepsKnownUnitsUpdated(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	reader := &fakeReader{units: []Unit{{Name: "old.service", ActiveState: "inactive", SubState: "dead"}}}
	st := &fakeStore{known: []string{"old.service"}}
	source := New(reader, st, discardLogger(), &testClock{now: base}, nil, nil)
	if err := source.loadKnown(context.Background()); err != nil {
		t.Fatalf("loadKnown() error = %v", err)
	}
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	st.mu.Lock()
	unit, ok := st.units["old.service"]
	st.mu.Unlock()
	if !ok || unit.ActiveState != "inactive" {
		t.Errorf("unit = %+v, want the known unit updated", unit)
	}
}

func TestCollectReportsUnavailable(t *testing.T) {
	st := &fakeStore{}
	source := New(&fakeReader{err: errors.New("no bus")}, st, discardLogger(), &testClock{now: time.Now()}, nil, nil)
	if err := source.collect(context.Background()); err == nil {
		t.Fatal("collect() error = nil, want the bus error")
	}
	status, lastError := st.statusOf()
	if status != "unavailable" || lastError != "no bus" {
		t.Errorf("status = %q, %q, want unavailable with the error", status, lastError)
	}
}

func TestEnabled(t *testing.T) {
	tests := []struct {
		name    string
		mode    config.SystemdMode
		socket  bool
		want    bool
		wantErr bool
	}{
		{name: "auto with socket", mode: config.SystemdAuto, socket: true, want: true},
		{name: "auto without socket", mode: config.SystemdAuto},
		{name: "on with socket", mode: config.SystemdOn, socket: true, want: true},
		{name: "on without socket", mode: config.SystemdOn, wantErr: true},
		{name: "off with socket", mode: config.SystemdOff, socket: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, err := Enabled(tt.mode, tt.socket)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Enabled() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Enabled() error = %v", err)
			}
			if enabled != tt.want {
				t.Errorf("Enabled() = %v, want %v", enabled, tt.want)
			}
		})
	}
}
