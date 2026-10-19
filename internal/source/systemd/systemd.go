package systemd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/sourcestatus"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the systemd reader in the source state.
const SourceName = "systemd"

const (
	// interval is how often the units are read.
	interval = 60 * time.Second
)

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
	status   *sourcestatus.Reporter
	clock    clock.Clock
	ignore   []string
	critical []string
	interval time.Duration

	mu     sync.Mutex
	known  map[string]bool
	failed map[string]bool
}

// New returns a systemd reader source.
func New(reader Reader, st Store, logger *slog.Logger, clock clock.Clock, ignore, critical []string) *Source {
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
		status:   sourcestatus.New(SourceName, st, clock, logger),
	}
}

// Run reads the units every interval until ctx is done.
func (s *Source) Run(ctx context.Context) error {
	if err := s.loadKnown(ctx); err != nil {
		return err
	}
	next := s.clock.After(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-next:
		}
		if err := s.collect(ctx); err != nil {
			s.logger.Warn("systemd read failed", slog.String("error", err.Error()))
		}
		next = s.clock.After(s.interval)
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
		s.status.Report(ctx, store.StatusUnavailable, err)
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
			s.status.Report(ctx, store.StatusUnavailable, err)
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
	s.status.Report(ctx, store.StatusOK, nil)
	return nil
}

// recordFailedEvent records that a unit entered the failed state.
func (s *Source) recordFailedEvent(ctx context.Context, units []Unit, name string, now time.Time) {
	event := store.Event{
		TS:       now,
		Kind:     store.EventUnitFailed,
		Severity: store.SeverityWarning,
		Subject:  name,
		Title:    "unit failed",
	}
	if Matches(s.critical, name) {
		event.Severity = store.SeverityCritical
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
		Kind:     store.EventUnitRecovered,
		Severity: store.SeverityInfo,
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

// BusSocket is where the host's system D-Bus socket is mounted.
const BusSocket = "/run/dbus/system_bus_socket"

// BusAvailable reports whether the system D-Bus socket exists.
func BusAvailable() bool {
	_, err := os.Stat(BusSocket)
	return err == nil
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
