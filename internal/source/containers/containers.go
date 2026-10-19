// Package containers samples CPU, memory and network for every running
// container once a minute.
package containers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the container collector in the source state.
const SourceName = "containers"

const (
	// interval is how often containers are sampled.
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

// Model provides the containers to sample.
type Model interface {
	// RunningContainers returns the running containers that are not ignored,
	// ordered by Docker ID.
	RunningContainers() []discovery.RunningContainer
}

// Docker reads one container's statistics.
type Docker interface {
	// Stats returns one statistics sample for a container.
	Stats(ctx context.Context, id string) (docker.Stats, error)
	// Info returns the daemon info, including the host's memory.
	Info(ctx context.Context) (docker.Info, error)
}

// Store records the samples.
type Store interface {
	// InsertSlotMinute records one container sample.
	InsertSlotMinute(ctx context.Context, sample store.SlotMinute) error
	// RecordSourceStatus records the state of one source.
	RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error
}

// Source samples every running container once a minute and records one
// slot_minute row per minute.
type Source struct {
	model  Model
	docker Docker
	store  Store
	logger *slog.Logger
	clock  Clock

	interval time.Duration

	mu          sync.Mutex
	samples     map[int64]*sample
	hostMemory  int64
	status      string
	statusError string
}

// sample is the previous statistics of one slot, used to compute the deltas
// of CPU time and network counters.
type sample struct {
	containerID string
	at          time.Time
	cpu         uint64
	rx, tx      uint64
}

// New returns a container collector.
func New(model Model, client Docker, st Store, logger *slog.Logger, clock Clock) *Source {
	return &Source{
		model:    model,
		docker:   client,
		store:    st,
		logger:   logger,
		clock:    clock,
		interval: interval,
		samples:  make(map[int64]*sample),
		status:   store.StatusUnavailable,
	}
}

// Run samples the containers every interval until ctx is done.
func (s *Source) Run(ctx context.Context) error {
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
			s.logger.Warn("container sample failed", slog.String("error", err.Error()))
			delay = min(delay*2, retryMax)
		} else {
			delay = retryBase
		}
		next = s.clock.After(max(s.interval, delay))
	}
}

// collect samples every running container once.
func (s *Source) collect(ctx context.Context) error {
	containers := s.model.RunningContainers()
	now := s.clock.Now()
	failures := 0
	running := make(map[int64]bool, len(containers))
	for _, container := range containers {
		running[container.SlotID] = true
		if err := s.collectContainer(ctx, container, now); err != nil {
			failures++
			s.logger.Warn("sample container",
				slog.String("container", container.ID), slog.String("error", err.Error()))
		}
	}
	s.recordStopped(ctx, running, now)
	if failures > 0 {
		s.recordStatus(ctx, store.StatusDegraded, errCount(failures, len(containers)))
		return nil
	}
	s.recordStatus(ctx, store.StatusOK, nil)
	return nil
}

// collectContainer records one container's sample.
func (s *Source) collectContainer(ctx context.Context, container discovery.RunningContainer, now time.Time) error {
	stats, err := s.docker.Stats(ctx, container.ID)
	if err != nil {
		return err
	}
	at := stats.Read
	if at.IsZero() {
		at = now
	}
	previous := s.samples[container.SlotID]
	var (
		cpu    int
		rx, tx int64
	)
	if previous != nil && previous.containerID == container.ID {
		if elapsed := at.Sub(previous.at); elapsed > 0 {
			if delta := stats.CPUTotalUsage - previous.cpu; stats.CPUTotalUsage >= previous.cpu {
				cpu = int(float64(delta) * 1000 / float64(elapsed))
			}
		}
		if stats.NetworkRxBytes >= previous.rx {
			rx = int64(stats.NetworkRxBytes - previous.rx)
		}
		if stats.NetworkTxBytes >= previous.tx {
			tx = int64(stats.NetworkTxBytes - previous.tx)
		}
	}
	s.samples[container.SlotID] = &sample{
		containerID: container.ID,
		at:          at,
		cpu:         stats.CPUTotalUsage,
		rx:          stats.NetworkRxBytes,
		tx:          stats.NetworkTxBytes,
	}
	return s.store.InsertSlotMinute(ctx, store.SlotMinute{
		SlotID:        container.SlotID,
		TS:            now.Truncate(time.Minute),
		CPUMillicores: cpu,
		MemBytes:      workingSet(stats),
		MemLimitBytes: s.memoryLimit(ctx, stats),
		NetRxBytes:    rx,
		NetTxBytes:    tx,
		Running:       true,
	})
}

// recordStopped marks the slots that stopped running since the previous pass.
func (s *Source) recordStopped(ctx context.Context, running map[int64]bool, now time.Time) {
	for slotID := range s.samples {
		if running[slotID] {
			continue
		}
		delete(s.samples, slotID)
		if err := s.store.InsertSlotMinute(ctx, store.SlotMinute{
			SlotID:  slotID,
			TS:      now.Truncate(time.Minute),
			Running: false,
		}); err != nil {
			s.logger.Warn("record stopped slot", slog.Int64("slot", slotID), slog.String("error", err.Error()))
		}
	}
}

// workingSet returns the container's memory usage without the page cache, as
// docker stats shows it.
func workingSet(stats docker.Stats) int64 {
	usage := int64(stats.MemoryUsage)
	for _, key := range []string{"inactive_file", "total_inactive_file", "cache"} {
		if cached, ok := stats.MemoryStats[key]; ok {
			usage -= int64(cached)
			break
		}
	}
	if usage < 0 {
		return 0
	}
	return usage
}

// memoryLimit returns the container's memory limit, nil when it has none: a
// limit equal to the host's memory is Docker's way of saying "unlimited".
func (s *Source) memoryLimit(ctx context.Context, stats docker.Stats) *int64 {
	if stats.MemoryLimit == 0 {
		return nil
	}
	hostMemory := s.hostMemorySize(ctx)
	if hostMemory > 0 && int64(stats.MemoryLimit) >= hostMemory {
		return nil
	}
	limit := int64(stats.MemoryLimit)
	return &limit
}

// hostMemorySize returns the host's total memory, reading it once.
func (s *Source) hostMemorySize(ctx context.Context) int64 {
	s.mu.Lock()
	memory := s.hostMemory
	s.mu.Unlock()
	if memory > 0 {
		return memory
	}
	info, err := s.docker.Info(ctx)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	s.hostMemory = info.MemTotal
	s.mu.Unlock()
	return info.MemTotal
}

// errCount describes a pass in which some containers could not be sampled.
func errCount(failed, total int) error {
	return fmt.Errorf("%d of %d containers could not be sampled", failed, total)
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
		s.logger.Warn("record container source status", slog.String("error", err.Error()))
	}
}
