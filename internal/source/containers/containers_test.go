package containers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/docker"
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

// fakeDocker returns canned statistics per container.
type fakeDocker struct {
	mu    sync.Mutex
	stats map[string][]docker.Stats
	info  docker.Info
	errs  map[string]error
}

func (f *fakeDocker) Stats(ctx context.Context, id string) (docker.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[id]; err != nil {
		return docker.Stats{}, err
	}
	samples := f.stats[id]
	if len(samples) == 0 {
		return docker.Stats{}, errors.New("no sample")
	}
	next := samples[0]
	f.stats[id] = samples[1:]
	return next, nil
}

func (f *fakeDocker) Info(context.Context) (docker.Info, error) { return f.info, nil }

// fakeStore records what the source writes.
type fakeStore struct {
	mu      sync.Mutex
	samples []store.SlotMinute
	status  string
	lastErr string
}

func (f *fakeStore) InsertSlotMinute(ctx context.Context, sample store.SlotMinute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, sample)
	return nil
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

// sampleAt returns the newest sample of one slot.
func (f *fakeStore) sampleAt(t *testing.T, slotID int64) store.SlotMinute {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.samples) - 1; i >= 0; i-- {
		if f.samples[i].SlotID == slotID {
			return f.samples[i]
		}
	}
	t.Fatalf("no sample for slot %d", slotID)
	return store.SlotMinute{}
}

// fakeModel returns a fixed list of running containers.
type fakeModel struct {
	mu         sync.Mutex
	containers []discovery.RunningContainer
}

func (m *fakeModel) RunningContainers() []discovery.RunningContainer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]discovery.RunningContainer(nil), m.containers...)
}

func (m *fakeModel) set(containers ...discovery.RunningContainer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers = containers
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stats returns one statistics sample.
func stats(at time.Time, cpu uint64, memory uint64, limit uint64, rx, tx uint64) docker.Stats {
	return docker.Stats{
		Read:           at,
		CPUTotalUsage:  cpu,
		MemoryUsage:    memory,
		MemoryLimit:    limit,
		MemoryStats:    map[string]uint64{"inactive_file": 50_000_000},
		NetworkRxBytes: rx,
		NetworkTxBytes: tx,
	}
}

func TestCollectComputesDeltas(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	client := &fakeDocker{
		info: docker.Info{MemTotal: 16 << 30},
		stats: map[string][]docker.Stats{
			"api1": {
				stats(base, 1_000_000_000, 200_000_000, 512<<20, 1000, 2000),
				stats(base.Add(time.Minute), 7_000_000_000, 210_000_000, 512<<20, 4000, 6000),
			},
		},
		errs: map[string]error{},
	}
	st := &fakeStore{}
	model := &fakeModel{containers: []discovery.RunningContainer{{ID: "api1", SlotID: 7}}}
	clock := &testClock{now: base}
	source := New(model, client, st, discardLogger(), clock)

	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("first collect error = %v", err)
	}
	first := st.sampleAt(t, 7)
	if first.TS != base || first.CPUMillicores != 0 || first.MemBytes != 150_000_000 {
		t.Errorf("first sample = %+v, want no CPU delta and a 150 MB working set", first)
	}
	if first.MemLimitBytes == nil || *first.MemLimitBytes != 512<<20 {
		t.Errorf("memory limit = %v, want 512 MiB", first.MemLimitBytes)
	}
	if !first.Running {
		t.Error("running = false, want true")
	}

	clock.now = base.Add(time.Minute)
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("second collect error = %v", err)
	}
	second := st.sampleAt(t, 7)
	if second.CPUMillicores != 100 {
		t.Errorf("CPU = %d millicores, want 100", second.CPUMillicores)
	}
	if second.NetRxBytes != 3000 || second.NetTxBytes != 4000 {
		t.Errorf("network = %d/%d, want 3000/4000", second.NetRxBytes, second.NetTxBytes)
	}
	if status, _ := st.statusOf(); status != "ok" {
		t.Errorf("status = %q, want ok", status)
	}
}

func TestCollectTreatsHostMemoryAsNoLimit(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	client := &fakeDocker{
		info: docker.Info{MemTotal: 16 << 30},
		stats: map[string][]docker.Stats{
			"api1": {stats(base, 0, 1000, 16<<30, 0, 0)},
		},
		errs: map[string]error{},
	}
	st := &fakeStore{}
	source := New(&fakeModel{containers: []discovery.RunningContainer{{ID: "api1", SlotID: 1}}}, client, st, discardLogger(), &testClock{now: base})
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("collect error = %v", err)
	}
	if sample := st.sampleAt(t, 1); sample.MemLimitBytes != nil {
		t.Errorf("memory limit = %v, want nil for a limit equal to the host memory", *sample.MemLimitBytes)
	}
}

func TestCollectResetsDeltasWhenTheContainerChanges(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	client := &fakeDocker{
		info: docker.Info{MemTotal: 16 << 30},
		stats: map[string][]docker.Stats{
			"old": {stats(base, 5_000_000_000, 1000, 0, 9000, 9000)},
			"new": {stats(base.Add(time.Minute), 1_000_000, 1000, 0, 500, 500)},
		},
		errs: map[string]error{},
	}
	st := &fakeStore{}
	model := &fakeModel{containers: []discovery.RunningContainer{{ID: "old", SlotID: 3}}}
	source := New(model, client, st, discardLogger(), &testClock{now: base})
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("first collect error = %v", err)
	}
	// The slot now runs a recreated container with fresh counters.
	model.set(discovery.RunningContainer{ID: "new", SlotID: 3})
	source.clock = &testClock{now: base.Add(time.Minute)}
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("second collect error = %v", err)
	}
	st.mu.Lock()
	last := st.samples[len(st.samples)-1]
	st.mu.Unlock()
	if last.CPUMillicores != 0 || last.NetRxBytes != 0 || last.NetTxBytes != 0 {
		t.Errorf("recreated container sample = %+v, want zero deltas", last)
	}
}

func TestCollectMarksStoppedSlots(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	client := &fakeDocker{
		info:  docker.Info{MemTotal: 16 << 30},
		stats: map[string][]docker.Stats{"api1": {stats(base, 0, 1000, 0, 0, 0)}},
		errs:  map[string]error{},
	}
	st := &fakeStore{}
	model := &fakeModel{containers: []discovery.RunningContainer{{ID: "api1", SlotID: 5}}}
	source := New(model, client, st, discardLogger(), &testClock{now: base})
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("first collect error = %v", err)
	}
	model.set()
	source.clock = &testClock{now: base.Add(time.Minute)}
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("second collect error = %v", err)
	}
	st.mu.Lock()
	last := st.samples[len(st.samples)-1]
	st.mu.Unlock()
	if last.SlotID != 5 || last.Running {
		t.Errorf("stopped sample = %+v, want the slot recorded as not running", last)
	}
}

func TestCollectReportsDegraded(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	client := &fakeDocker{
		info:  docker.Info{MemTotal: 16 << 30},
		stats: map[string][]docker.Stats{"api1": {stats(base, 0, 1000, 0, 0, 0)}},
		errs:  map[string]error{"db1": errors.New("stats unavailable")},
	}
	st := &fakeStore{}
	model := &fakeModel{containers: []discovery.RunningContainer{{ID: "api1", SlotID: 1}, {ID: "db1", SlotID: 2}}}
	source := New(model, client, st, discardLogger(), &testClock{now: base})
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("collect error = %v", err)
	}
	status, lastError := st.statusOf()
	if status != "degraded" {
		t.Errorf("status = %q, want degraded", status)
	}
	if lastError == "" {
		t.Error("last error is empty, want the failed container")
	}
	if sample := st.sampleAt(t, 1); sample.SlotID != 1 {
		t.Errorf("healthy container was not sampled: %+v", sample)
	}
}
