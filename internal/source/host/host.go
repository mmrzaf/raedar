// Package host samples CPU, load, memory, swap and filesystem usage of the
// host, plus Docker's own disk usage.
package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/sourcestatus"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the host collector in the source state.
const SourceName = "host"

const (
	// interval is how often the host is sampled.
	interval = 60 * time.Second
	// diskUsageInterval is how often Docker's disk usage is read.
	diskUsageInterval = 15 * time.Minute
)

// Docker reads the daemon's disk usage.
type Docker interface {
	// DiskUsage reads Docker's own disk usage report.
	DiskUsage(ctx context.Context) (docker.DiskUsage, error)
}

// Store records the samples.
type Store interface {
	// InsertHostMinute records one host sample.
	InsertHostMinute(ctx context.Context, sample store.HostMinute) error
	// InsertDiskMinute records one filesystem sample.
	InsertDiskMinute(ctx context.Context, sample store.DiskMinute) error
	// InsertDockerDisk records one Docker disk usage report.
	InsertDockerDisk(ctx context.Context, usage store.DockerDisk) error
	// RecordSourceStatus records the state of one source.
	RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error
}

// Source samples the host once a minute and Docker's disk usage every fifteen
// minutes.
type Source struct {
	disks    []config.Disk
	procRoot string
	docker   Docker
	store    Store
	logger   *slog.Logger
	status   *sourcestatus.Reporter
	clock    clock.Clock

	interval  time.Duration
	diskEvery time.Duration

	mu          sync.Mutex
	previous    *cpuSample
	uptime      time.Duration
	nextDiskUse time.Time
}

// cpuSample is the previous /proc/stat reading, used for the CPU delta.
type cpuSample struct {
	at    time.Time
	total uint64
	idle  uint64
}

// New returns a host collector. disks are the filesystems to watch, procRoot
// is where the process sees /proc.
func New(disks []config.Disk, procRoot string, client Docker, st Store, logger *slog.Logger, clock clock.Clock) *Source {
	return &Source{
		disks:     disks,
		procRoot:  procRoot,
		docker:    client,
		store:     st,
		logger:    logger,
		clock:     clock,
		interval:  interval,
		diskEvery: diskUsageInterval,
		status:    sourcestatus.New(SourceName, st, clock, logger),
	}
}

// Run samples the host every interval until ctx is done.
func (s *Source) Run(ctx context.Context) error {
	next := s.clock.After(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-next:
		}
		if err := s.collect(ctx); err != nil {
			s.logger.Warn("host sample failed", slog.String("error", err.Error()))
		}
		next = s.clock.After(s.interval)
	}
}

// collect reads one host sample plus the filesystems and, every fifteen
// minutes, Docker's disk usage.
func (s *Source) collect(ctx context.Context) error {
	now := s.clock.Now()
	moment := now.Truncate(time.Minute)
	if err := s.collectHost(ctx, moment); err != nil {
		s.status.Report(ctx, store.StatusUnavailable, err)
		return err
	}
	failures := 0
	for _, disk := range s.disks {
		sample, err := diskSample(disk, moment)
		if err != nil {
			failures++
			s.logger.Warn("read filesystem", slog.String("disk", disk.Path), slog.String("error", err.Error()))
			continue
		}
		if err := s.store.InsertDiskMinute(ctx, sample); err != nil {
			s.status.Report(ctx, store.StatusUnavailable, err)
			return err
		}
	}
	if !now.Before(s.nextDiskUse) {
		s.nextDiskUse = now.Add(s.diskEvery)
		s.collectDockerDisk(ctx, now)
	}
	if failures > 0 {
		s.status.Report(ctx, store.StatusDegraded, fmt.Errorf("%d of %d filesystems could not be read", failures, len(s.disks)))
		return nil
	}
	s.status.Report(ctx, store.StatusOK, nil)
	return nil
}

// collectHost reads and records the host's own statistics.
func (s *Source) collectHost(ctx context.Context, moment time.Time) error {
	stat, err := readCPU(s.procRoot)
	if err != nil {
		return err
	}
	load, err := readLoad(s.procRoot)
	if err != nil {
		return err
	}
	memory, err := readMemory(s.procRoot)
	if err != nil {
		return err
	}
	uptime, err := readUptime(s.procRoot)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	s.mu.Lock()
	previous := s.previous
	lastUptime := s.uptime
	s.previous = &cpuSample{at: now, total: stat.total, idle: stat.idle}
	s.uptime = uptime
	s.mu.Unlock()
	if lastUptime > 0 && uptime < lastUptime {
		s.logger.Warn("host rebooted", slog.Duration("uptime", uptime))
	}
	sample := store.HostMinute{
		TS:           moment,
		Load1:        load[0],
		Load5:        load[1],
		Load15:       load[2],
		MemTotal:     memory.total,
		MemAvailable: memory.available,
		SwapTotal:    memory.swapTotal,
		SwapUsed:     memory.swapTotal - memory.swapFree,
	}
	if previous != nil {
		if total := stat.total - previous.total; stat.total >= previous.total && total > 0 {
			idle := stat.idle - previous.idle
			sample.CPUPercent = 100 * float64(total-min(idle, total)) / float64(total)
		}
	} else {
		// Without two samples there is no CPU delta; the first minute is
		// skipped so that no made-up value is stored.
		return nil
	}
	return s.store.InsertHostMinute(ctx, sample)
}

// collectDockerDisk records Docker's disk usage; a daemon that does not allow
// /system/df leaves the source degraded without failing the pass.
func (s *Source) collectDockerDisk(ctx context.Context, now time.Time) {
	usage, err := s.docker.DiskUsage(ctx)
	if err != nil {
		s.logger.Warn("Docker disk usage is unavailable", slog.String("error", err.Error()))
		return
	}
	if err := s.store.InsertDockerDisk(ctx, store.DockerDisk{
		TS:                     now.Truncate(time.Minute),
		ImagesBytes:            usage.ImagesBytes,
		ImagesReclaimableBytes: usage.ImagesReclaimableBytes,
		BuildCacheBytes:        usage.BuildCacheBytes,
		VolumesBytes:           usage.VolumesBytes,
		ContainersBytes:        usage.ContainersBytes,
	}); err != nil {
		s.logger.Warn("record Docker disk usage", slog.String("error", err.Error()))
	}
}

// cpu is one /proc/stat CPU line.
type cpu struct {
	total uint64
	idle  uint64
}

// readCPU reads the first line of /proc/stat.
func readCPU(procRoot string) (cpu, error) {
	file, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return cpu{}, fmt.Errorf("open /proc/stat: %w", err)
	}
	defer file.Close()
	return parseCPU(file)
}

// parseCPU parses the aggregate CPU line of /proc/stat.
func parseCPU(reader io.Reader) (cpu, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var values [10]uint64
		for i, field := range fields[1:] {
			if i >= len(values) {
				break
			}
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpu{}, fmt.Errorf("parse /proc/stat: %w", err)
			}
			values[i] = value
		}
		var total uint64
		for _, value := range values {
			total += value
		}
		return cpu{total: total, idle: values[3] + values[4]}, nil
	}
	if err := scanner.Err(); err != nil {
		return cpu{}, fmt.Errorf("read /proc/stat: %w", err)
	}
	return cpu{}, fmt.Errorf("read /proc/stat: no CPU line")
}

// readLoad reads the load averages from /proc/loadavg.
func readLoad(procRoot string) ([3]float64, error) {
	file, err := os.Open(filepath.Join(procRoot, "loadavg"))
	if err != nil {
		return [3]float64{}, fmt.Errorf("open /proc/loadavg: %w", err)
	}
	defer file.Close()
	return parseLoad(file)
}

// parseLoad parses the load averages of /proc/loadavg.
func parseLoad(reader io.Reader) ([3]float64, error) {
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return [3]float64{}, fmt.Errorf("read /proc/loadavg: %w", err)
		}
		return [3]float64{}, fmt.Errorf("read /proc/loadavg: empty file")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 3 {
		return [3]float64{}, fmt.Errorf("read /proc/loadavg: want three load averages")
	}
	var load [3]float64
	for i := range load {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return [3]float64{}, fmt.Errorf("parse /proc/loadavg: %w", err)
		}
		load[i] = value
	}
	return load, nil
}

// memory is the part of /proc/meminfo the host collector uses.
type memory struct {
	total     int64
	available int64
	swapTotal int64
	swapFree  int64
}

// readMemory reads /proc/meminfo.
func readMemory(procRoot string) (memory, error) {
	file, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return memory{}, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer file.Close()
	return parseMemory(file)
}

// parseMemory parses the values the host collector uses from /proc/meminfo.
// Values are in kibibytes.
func parseMemory(reader io.Reader) (memory, error) {
	var result memory
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return memory{}, fmt.Errorf("parse /proc/meminfo: %w", err)
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			result.total = value * 1024
		case "MemAvailable":
			result.available = value * 1024
		case "SwapTotal":
			result.swapTotal = value * 1024
		case "SwapFree":
			result.swapFree = value * 1024
		}
	}
	if err := scanner.Err(); err != nil {
		return memory{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	if result.total == 0 {
		return memory{}, fmt.Errorf("read /proc/meminfo: no MemTotal")
	}
	return result, nil
}

// readUptime reads the host's uptime from /proc/uptime.
func readUptime(procRoot string) (time.Duration, error) {
	file, err := os.Open(filepath.Join(procRoot, "uptime"))
	if err != nil {
		return 0, fmt.Errorf("open /proc/uptime: %w", err)
	}
	defer file.Close()
	return parseUptime(file)
}

// parseUptime parses the first value of /proc/uptime, in seconds.
func parseUptime(reader io.Reader) (time.Duration, error) {
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, fmt.Errorf("read /proc/uptime: %w", err)
		}
		return 0, fmt.Errorf("read /proc/uptime: empty file")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) == 0 {
		return 0, fmt.Errorf("read /proc/uptime: empty line")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// diskSample reads the usage of one filesystem.
func diskSample(disk config.Disk, moment time.Time) (store.DiskMinute, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(disk.Path, &stat); err != nil {
		return store.DiskMinute{}, fmt.Errorf("statfs %s: %w", disk.Path, err)
	}
	blockSize := int64(stat.Bsize)
	return store.DiskMinute{
		Disk:       disk.Name,
		TS:         moment,
		SizeBytes:  int64(stat.Blocks) * blockSize,
		UsedBytes:  int64(stat.Blocks-stat.Bfree) * blockSize,
		AvailBytes: int64(stat.Bavail) * blockSize,
		Inodes:     int64(stat.Files),
		InodesUsed: int64(stat.Files - stat.Ffree),
	}, nil
}
