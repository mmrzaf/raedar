package systemd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the systemd reader in the source state.
const SourceName = "systemd"

const (
	// interval is how often the units are read.
	interval = 60 * time.Second
	// retryBase and retryMax bound the delay after a failed pass.
	retryBase = time.Second
	retryMax  = 30 * time.Second
)

// Clock provides timers to the source, so that tests run without waiting.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the clock used outside tests.
func SystemClock() Clock {
	return systemClock{}
}

// systemClock is the clock of the running process.
type systemClock struct{}

// Now returns the current time in UTC.
func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

// After returns a channel that receives the current time after d.
func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// Store records unit state and events.
type Store interface {
	// UpsertUnit records the state of one unit.
	UpsertUnit(ctx context.Context, unit store.Unit) error
	// UnitNames returns the names of the units that are recorded.
	UnitNames(ctx context.Context) ([]string, error)
	// AddEvent records one event.
	AddEvent(ctx context.Context, event store.Event) (int64, error)
	// RecordSourceStatus records the state of one source.
	RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error
}

// Source reads systemd unit state once a minute and records the units that
// are failed or failed within retention.
type Source struct {
	reader   Reader
	store    Store
	logger   *slog.Logger
	clock    Clock
	ignore   []string
	critical []string
	interval time.Duration

	mu          sync.Mutex
	known       map[string]bool
	failed      map[string]bool
	status      string
	statusError string
}

// New returns a systemd reader source.
func New(reader Reader, st Store, logger *slog.Logger, clock Clock, ignore, critical []string) *Source {
	return &Source{
		reader:   reader,
		store:    st,
		logger:   logger,
		clock:    clock,
		ignore:   ignore,
		critical: critical,
		interval: interval,
		known:    make(map[string]bool),
		failed:   make(map[string]bool),
		status:   store.StatusUnavailable,
	}
}

// Run reads the units every interval until ctx is done.
func (s *Source) Run(ctx context.Context) error {
	if err := s.loadKnown(ctx); err != nil {
		return err
	}
	delay := retryBase
	next := s.clock.After(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-next:
		}
		err := s.collect(ctx)
		if err != nil {
			s.logger.Warn("systemd read failed", slog.String("error", err.Error()))
			delay = min(delay*2, retryMax)
		} else {
			delay = retryBase
		}
		next = s.clock.After(max(s.interval, delay))
	}
}

// loadKnown reads the units that already have a row, so that units which left
// the failed state are kept up to date.
func (s *Source) loadKnown(ctx context.Context) error {
	names, err := s.store.UnitNames(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		s.known[name] = true
	}
	return nil
}

// collect reads the units once.
func (s *Source) collect(ctx context.Context) error {
	units, err := s.reader.ListUnits(ctx)
	if err != nil {
		s.recordStatus(ctx, store.StatusUnavailable, err)
		return err
	}
	now := s.clock.Now()
	s.mu.Lock()
	wasFailed := make(map[string]bool, len(s.failed))
	for name := range s.failed {
		wasFailed[name] = true
	}
	s.mu.Unlock()
	failedNow := make(map[string]bool)
	for _, unit := range units {
		if Matches(s.ignore, unit.Name) {
			continue
		}
		if unit.ActiveState == "failed" {
			failedNow[unit.Name] = true
		}
		s.mu.Lock()
		record := unit.ActiveState == "failed" || s.known[unit.Name]
		if unit.ActiveState == "failed" {
			s.known[unit.Name] = true
		}
		s.mu.Unlock()
		if !record {
			continue
		}
		if err := s.store.UpsertUnit(ctx, store.Unit{
			Name:        unit.Name,
			ActiveState: unit.ActiveState,
			SubState:    unit.SubState,
			Result:      unit.Result,
			ExitStatus:  unit.ExitStatus,
			Since:       unit.Since,
			LastSeen:    now,
		}); err != nil {
			s.recordStatus(ctx, store.StatusUnavailable, err)
			return err
		}
	}
	for name := range failedNow {
		if wasFailed[name] {
			continue
		}
		s.recordFailedEvent(ctx, units, name, now)
	}
	for name := range wasFailed {
		if failedNow[name] {
			continue
		}
		s.recordRecoveredEvent(ctx, name, now)
	}
	s.mu.Lock()
	s.failed = failedNow
	s.mu.Unlock()
	s.recordStatus(ctx, store.StatusOK, nil)
	return nil
}

// recordFailedEvent records that a unit entered the failed state.
func (s *Source) recordFailedEvent(ctx context.Context, units []Unit, name string, now time.Time) {
	event := store.Event{
		TS:       now,
		Kind:     "unit_failed",
		Severity: "warning",
		Subject:  name,
		Title:    "unit failed",
	}
	if Matches(s.critical, name) {
		event.Severity = "critical"
	}
	var data struct {
		Result     string `json:"result,omitempty"`
		ExitStatus *int   `json:"exit_status,omitempty"`
	}
	for _, unit := range units {
		if unit.Name != name {
			continue
		}
		data.Result = unit.Result
		data.ExitStatus = unit.ExitStatus
		if unit.Result != "" {
			event.Title = "unit failed: " + unit.Result
		}
	}
	event.Data = marshal(data)
	if _, err := s.store.AddEvent(ctx, event); err != nil {
		s.logger.Warn("record failed unit event", slog.String("unit", name), slog.String("error", err.Error()))
	}
}

// recordRecoveredEvent records that a unit left the failed state.
func (s *Source) recordRecoveredEvent(ctx context.Context, name string, now time.Time) {
	event := store.Event{
		TS:       now,
		Kind:     "unit_recovered",
		Severity: "info",
		Subject:  name,
		Title:    "unit recovered",
		Data:     marshal(struct{}{}),
	}
	if _, err := s.store.AddEvent(ctx, event); err != nil {
		s.logger.Warn("record recovered unit event", slog.String("unit", name), slog.String("error", err.Error()))
	}
}

// marshal encodes an event payload, returning an empty object on failure.
func marshal(payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// recordStatus stores the state of the source when it changed.
func (s *Source) recordStatus(ctx context.Context, status string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	s.mu.Lock()
	changed := s.status != status || s.statusError != message
	s.status = status
	s.statusError = message
	s.mu.Unlock()
	if !changed {
		return
	}
	if err := s.store.RecordSourceStatus(ctx, SourceName, status, message, s.clock.Now()); err != nil {
		s.logger.Warn("record systemd source status", slog.String("error", err.Error()))
	}
}

// Enabled reports whether the systemd source should run for a mode and the
// presence of the D-Bus socket.
func Enabled(mode config.SystemdMode, socketExists bool) (bool, error) {
	switch mode {
	case config.SystemdOn:
		if !socketExists {
			return false, fmt.Errorf("RAEDAR_SYSTEMD is on but the D-Bus socket is missing")
		}
		return true, nil
	case config.SystemdOff:
		return false, nil
	default:
		return socketExists, nil
	}
}
