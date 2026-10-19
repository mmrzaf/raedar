package host

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

	"github.com/mmrzaf/raedar/internal/config"
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

// fakeStore records what the source writes.
type fakeStore struct {
	mu      sync.Mutex
	hosts   []store.HostMinute
	disks   []store.DiskMinute
	docker  []store.DockerDisk
	status  string
	lastErr string
}

func (f *fakeStore) InsertHostMinute(ctx context.Context, sample store.HostMinute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = append(f.hosts, sample)
	return nil
}

func (f *fakeStore) InsertDiskMinute(ctx context.Context, sample store.DiskMinute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks = append(f.disks, sample)
	return nil
}

func (f *fakeStore) InsertDockerDisk(ctx context.Context, usage store.DockerDisk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docker = append(f.docker, usage)
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

// fakeDocker counts the disk usage calls.
type fakeDocker struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeDocker) DiskUsage(context.Context) (docker.DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return docker.DiskUsage{ImagesBytes: 100, ImagesReclaimableBytes: 20, BuildCacheBytes: 10, VolumesBytes: 30, ContainersBytes: 5}, nil
}

// count returns the number of disk usage calls.
func (f *fakeDocker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// procDir writes the /proc files used by the tests and returns the root.
func procDir(t *testing.T, stat, meminfo string) string {
	t.Helper()
	root := t.TempDir()
	writeProc(t, root, "stat", stat)
	writeProc(t, root, "meminfo", meminfo)
	writeProc(t, root, "loadavg", "0.52 0.58 0.59 1/456 7890\n")
	writeProc(t, root, "uptime", "123456.78 987654.32\n")
	return root
}

// writeProc writes one file under the fake /proc root.
func writeProc(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const (
	statBefore = "cpu  1000 0 1000 8000 1000 0 0 0 0 0\ncpu0 500 0 500 4000 500 0 0 0 0 0\n"
	statAfter  = "cpu  2000 0 2000 16000 2000 0 0 0 0 0\ncpu0 1000 0 1000 8000 1000 0 0 0 0 0\n"
	meminfo    = "MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    8000000 kB\nBuffers:          100000 kB\nCached:          2000000 kB\nSwapTotal:       2000000 kB\nSwapFree:        1500000 kB\n"
)

func TestParseCPU(t *testing.T) {
	cpu, err := parseCPU(strings.NewReader(statBefore))
	if err != nil {
		t.Fatalf("parseCPU() error = %v", err)
	}
	// user 1000 + system 1000 + idle 8000 + iowait 1000 = 11000 total, 9000 idle.
	if cpu.total != 11000 || cpu.idle != 9000 {
		t.Errorf("parseCPU() = %+v, want total 11000 and idle 9000", cpu)
	}
}

func TestParseCPURejectsGarbage(t *testing.T) {
	for _, content := range []string{"", "cpu\n", "cpu a b c d\n", "intr 1 2 3\n"} {
		if _, err := parseCPU(strings.NewReader(content)); err == nil {
			t.Errorf("parseCPU(%q) error = nil, want an error", content)
		}
	}
}

func TestParseLoad(t *testing.T) {
	load, err := parseLoad(strings.NewReader("0.52 0.58 0.59 1/456 7890\n"))
	if err != nil {
		t.Fatalf("parseLoad() error = %v", err)
	}
	if load != [3]float64{0.52, 0.58, 0.59} {
		t.Errorf("parseLoad() = %v, want 0.52 0.58 0.59", load)
	}
}

func TestParseMemory(t *testing.T) {
	memory, err := parseMemory(strings.NewReader(meminfo))
	if err != nil {
		t.Fatalf("parseMemory() error = %v", err)
	}
	if memory.total != 16_000_000*1024 {
		t.Errorf("total = %d, want 16000000 KiB", memory.total)
	}
	if memory.available != 8_000_000*1024 {
		t.Errorf("available = %d, want 8000000 KiB", memory.available)
	}
	if memory.swapTotal != 2_000_000*1024 || memory.swapFree != 1_500_000*1024 {
		t.Errorf("swap = %d/%d, want 2000000/1500000 KiB", memory.swapTotal, memory.swapFree)
	}
}

func TestParseUptime(t *testing.T) {
	uptime, err := parseUptime(strings.NewReader("123456.78 987654.32\n"))
	if err != nil {
		t.Fatalf("parseUptime() error = %v", err)
	}
	if uptime != 123456*time.Second+780*time.Millisecond {
		t.Errorf("uptime = %v, want 123456.78s", uptime)
	}
}

func TestCollectWritesHostAndDisk(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	root := procDir(t, statBefore, meminfo)
	diskDir := t.TempDir()
	st := &fakeStore{}
	clock := &testClock{now: base}
	source := New([]config.Disk{{Name: "/", Path: diskDir}}, root, &fakeDocker{}, st, discardLogger(), clock)
	// The first pass only establishes the CPU baseline.
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("first collect error = %v", err)
	}
	st.mu.Lock()
	hosts := len(st.hosts)
	st.mu.Unlock()
	if hosts != 0 {
		t.Fatalf("host samples = %d, want none before the second sample", hosts)
	}
	writeProc(t, root, "stat", statAfter)
	clock.now = base.Add(time.Minute)
	if err := source.collect(context.Background()); err != nil {
		t.Fatalf("second collect error = %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.hosts) != 1 {
		t.Fatalf("host samples = %d, want 1", len(st.hosts))
	}
	host := st.hosts[0]
	if host.TS != base.Add(time.Minute) {
		t.Errorf("TS = %v, want the second minute", host.TS)
	}
	// The CPU was busy 2000 of the 11000 jiffies that passed: 18.18 %.
	if host.CPUPercent < 18 || host.CPUPercent > 19 {
		t.Errorf("CPU = %v %%, want about 18.2", host.CPUPercent)
	}
	if host.MemTotal != 16_000_000*1024 || host.MemAvailable != 8_000_000*1024 {
		t.Errorf("memory = %d/%d, want the parsed values", host.MemTotal, host.MemAvailable)
	}
	if host.SwapUsed != 500_000*1024 {
		t.Errorf("swap used = %d, want 500000 KiB", host.SwapUsed)
	}
	if len(st.disks) != 2 {
		t.Fatalf("disk samples = %d, want one per pass", len(st.disks))
	}
	disk := st.disks[1]
	if disk.Disk != "/" || disk.SizeBytes <= 0 || disk.Inodes <= 0 {
		t.Errorf("disk sample = %+v, want a valid filesystem sample", disk)
	}
	if disk.UsedBytes+disk.AvailBytes > disk.SizeBytes {
		t.Errorf("disk sample = %+v, want used plus available within the size", disk)
	}
}

func TestCollectReadsDockerDiskEveryFifteenMinutes(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	root := procDir(t, statBefore, meminfo)
	client := &fakeDocker{}
	st := &fakeStore{}
	clock := &testClock{now: base}
	source := New(nil, root, client, st, discardLogger(), clock)
	for i := 0; i < 16; i++ {
		clock.now = base.Add(time.Duration(i) * time.Minute)
		if err := source.collect(context.Background()); err != nil {
			t.Fatalf("collect %d error = %v", i, err)
		}
	}
	if got := client.count(); got != 2 {
		t.Errorf("Docker disk usage calls = %d, want 2 in fifteen minutes", got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.docker) != 2 {
		t.Fatalf("docker_disk rows = %d, want 2", len(st.docker))
	}
	if st.docker[0].ImagesBytes != 100 || st.docker[0].VolumesBytes != 30 {
		t.Errorf("docker_disk row = %+v, want the daemon's numbers", st.docker[0])
	}
}

func TestCollectReportsUnavailable(t *testing.T) {
	st := &fakeStore{}
	source := New(nil, t.TempDir(), &fakeDocker{}, st, discardLogger(), &testClock{now: time.Now()})
	if err := source.collect(context.Background()); err == nil {
		t.Fatal("collect error = nil, want the missing /proc error")
	}
	status, lastError := st.statusOf()
	if status != "unavailable" {
		t.Errorf("status = %q, want unavailable", status)
	}
	if !strings.Contains(lastError, "/proc/stat") {
		t.Errorf("last error = %q, want the missing file", lastError)
	}
}
