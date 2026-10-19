package store

import (
	"context"
	"fmt"
	"time"
)

// DefaultPoints is the approximate number of points a chart asks for.
const DefaultPoints = 600

// MinuteRange is the longest range read from the minute tables.
const MinuteRange = 14 * 24 * time.Hour

// RangeSource reports which tables a range is read from.
type RangeSource string

const (
	// SourceMinutes reads the per-minute tables.
	SourceMinutes RangeSource = "minutes"
	// SourceHours reads the hourly tables.
	SourceHours RangeSource = "hours"
)

// Range is a chart request: a time range and the approximate number of points.
type Range struct {
	// From is the start of the range.
	From time.Time
	// To is the end of the range.
	To time.Time
	// Points is the approximate number of points; zero means DefaultPoints.
	Points int
}

// Resolution returns the tables a range is read from and the bin size: minute
// tables for ranges of up to 14 days that lie within the minute retention,
// hourly tables otherwise. The bin size is the range divided by the number of
// points, rounded up to a whole minute or hour.
func Resolution(r Range, minuteRetention time.Duration, now time.Time) (RangeSource, time.Duration) {
	points := r.Points
	if points <= 0 {
		points = DefaultPoints
	}
	span := r.To.Sub(r.From)
	if span <= 0 {
		span = time.Minute
	}
	source := SourceHours
	if span <= MinuteRange && !r.From.Before(now.Add(-minuteRetention)) {
		source = SourceMinutes
	}
	unit := time.Hour
	if source == SourceMinutes {
		unit = time.Minute
	}
	bin := span / time.Duration(points)
	if bin < unit {
		bin = unit
	}
	return source, (bin + unit - 1) / unit * unit
}

// RouteBin is one bin of request data.
type RouteBin struct {
	// TS is the start of the bin.
	TS time.Time
	// Requests counts the requests in the bin.
	Requests int64
	// S1xx to S5xx count the responses by status class.
	S1xx, S2xx, S3xx, S4xx, S5xx int64
	// NoBackend counts requests Traefik answered without a backend.
	NoBackend int64
	// Bytes is the total response size.
	Bytes int64
	// Latency is the summed latency histogram.
	Latency Histogram
}

// RouteSeries reads request data for the given routes, grouped into bins
// aligned to the start of the range.
func (s *Store) RouteSeries(ctx context.Context, routeIDs []int64, r Range, source RangeSource, bin time.Duration) ([]RouteBin, error) {
	if len(routeIDs) == 0 {
		return nil, nil
	}
	table := "route_minute"
	if source == SourceHours {
		table = "route_hour"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		select date_bin($4::double precision * interval '1 second', ts, $2::timestamptz) as bin,
		       sum(requests), sum(s1xx), sum(s2xx), sum(s3xx), sum(s4xx), sum(s5xx),
		       sum(no_backend), sum(bytes), raedar_hist_sum(latency)
		from %s
		where route_id = any($1) and ts >= $2 and ts < $3
		group by bin
		order by bin`, table), routeIDs, r.From, r.To, bin.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read route series: %w", err)
	}
	defer rows.Close()
	var bins []RouteBin
	for rows.Next() {
		var (
			entry     RouteBin
			histogram []int32
		)
		if err := rows.Scan(&entry.TS, &entry.Requests, &entry.S1xx, &entry.S2xx, &entry.S3xx,
			&entry.S4xx, &entry.S5xx, &entry.NoBackend, &entry.Bytes, &histogram); err != nil {
			return nil, fmt.Errorf("scan route series: %w", err)
		}
		if entry.Latency, err = LoadHistogram(histogram); err != nil {
			return nil, fmt.Errorf("read route series: %w", err)
		}
		bins = append(bins, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read route series: %w", err)
	}
	return bins, nil
}

// SlotBin is one bin of container data.
type SlotBin struct {
	// TS is the start of the bin.
	TS time.Time
	// CPUAvg and CPUMax are the CPU usage in millicores.
	CPUAvg, CPUMax float64
	// MemAvg and MemMax are the working-set memory in bytes.
	MemAvg, MemMax float64
	// NetRx and NetTx are the network traffic in bytes.
	NetRx, NetTx int64
}

// SlotSeries reads container data for the given slots, grouped into bins.
func (s *Store) SlotSeries(ctx context.Context, slotIDs []int64, r Range, source RangeSource, bin time.Duration) ([]SlotBin, error) {
	if len(slotIDs) == 0 {
		return nil, nil
	}
	var query string
	if source == SourceHours {
		query = `
			select date_bin($4::double precision * interval '1 second', ts, $2::timestamptz) as bin,
			       avg(cpu_avg), max(cpu_max), avg(mem_avg), max(mem_max), sum(net_rx_bytes), sum(net_tx_bytes)
			from slot_hour
			where slot_id = any($1) and ts >= $2 and ts < $3
			group by bin
			order by bin`
	} else {
		query = `
			select date_bin($4::double precision * interval '1 second', ts, $2::timestamptz) as bin,
			       avg(cpu_millicores), max(cpu_millicores), avg(mem_bytes), max(mem_bytes),
			       sum(net_rx_bytes), sum(net_tx_bytes)
			from slot_minute
			where slot_id = any($1) and ts >= $2 and ts < $3
			group by bin
			order by bin`
	}
	rows, err := s.pool.Query(ctx, query, slotIDs, r.From, r.To, bin.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read slot series: %w", err)
	}
	defer rows.Close()
	var bins []SlotBin
	for rows.Next() {
		var entry SlotBin
		if err := rows.Scan(&entry.TS, &entry.CPUAvg, &entry.CPUMax, &entry.MemAvg, &entry.MemMax,
			&entry.NetRx, &entry.NetTx); err != nil {
			return nil, fmt.Errorf("scan slot series: %w", err)
		}
		bins = append(bins, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read slot series: %w", err)
	}
	return bins, nil
}

// HostBin is one bin of host data.
type HostBin struct {
	// TS is the start of the bin.
	TS time.Time
	// CPUAvg and CPUMax are the CPU usage of the host, in percent.
	CPUAvg, CPUMax float64
	// Load1Avg is the average one-minute load.
	Load1Avg float64
	// MemAvailableAvg and MemAvailableMin are the available memory in bytes.
	MemAvailableAvg, MemAvailableMin float64
	// SwapUsedAvg is the swap in use, in bytes.
	SwapUsedAvg float64
}

// HostSeries reads host data, grouped into bins.
func (s *Store) HostSeries(ctx context.Context, r Range, source RangeSource, bin time.Duration) ([]HostBin, error) {
	var query string
	if source == SourceHours {
		query = `
			select date_bin($3::double precision * interval '1 second', ts, $1::timestamptz) as bin,
			       avg(cpu_avg), max(cpu_max), avg(load1_avg), avg(mem_available_avg), min(mem_available_min),
		       avg(swap_used_max)
			from host_hour
			where ts >= $1 and ts < $2
			group by bin
			order by bin`
	} else {
		query = `
			select date_bin($3::double precision * interval '1 second', ts, $1::timestamptz) as bin,
			       avg(cpu_percent), max(cpu_percent), avg(load1), avg(mem_available), min(mem_available),
		       avg(swap_used)
			from host_minute
			where ts >= $1 and ts < $2
			group by bin
			order by bin`
	}
	rows, err := s.pool.Query(ctx, query, r.From, r.To, bin.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read host series: %w", err)
	}
	defer rows.Close()
	var bins []HostBin
	for rows.Next() {
		var entry HostBin
		if err := rows.Scan(&entry.TS, &entry.CPUAvg, &entry.CPUMax, &entry.Load1Avg,
			&entry.MemAvailableAvg, &entry.MemAvailableMin, &entry.SwapUsedAvg); err != nil {
			return nil, fmt.Errorf("scan host series: %w", err)
		}
		bins = append(bins, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read host series: %w", err)
	}
	return bins, nil
}

// DiskBin is one bin of filesystem data.
type DiskBin struct {
	// TS is the start of the bin.
	TS time.Time
	// SizeBytes is the filesystem size.
	SizeBytes int64
	// UsedBytesMax is the highest usage in the bin.
	UsedBytesMax int64
	// AvailBytesMin is the lowest available space in the bin.
	AvailBytesMin int64
	// InodesUsedMax is the highest inode usage in the bin.
	InodesUsedMax int64
}

// DiskSeries reads filesystem data for the given disks, grouped into bins.
func (s *Store) DiskSeries(ctx context.Context, disks []string, r Range, source RangeSource, bin time.Duration) ([]DiskBin, error) {
	if len(disks) == 0 {
		return nil, nil
	}
	var query string
	if source == SourceHours {
		query = `
			select date_bin($4::double precision * interval '1 second', ts, $2::timestamptz) as bin,
			       max(size_bytes), max(used_bytes_max), min(avail_bytes_min), max(inodes_used_max)
			from disk_hour
			where disk = any($1) and ts >= $2 and ts < $3
			group by bin
			order by bin`
	} else {
		query = `
			select date_bin($4::double precision * interval '1 second', ts, $2::timestamptz) as bin,
			       max(size_bytes), max(used_bytes), min(avail_bytes), max(inodes_used)
			from disk_minute
			where disk = any($1) and ts >= $2 and ts < $3
			group by bin
			order by bin`
	}
	rows, err := s.pool.Query(ctx, query, disks, r.From, r.To, bin.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read disk series: %w", err)
	}
	defer rows.Close()
	var bins []DiskBin
	for rows.Next() {
		var entry DiskBin
		if err := rows.Scan(&entry.TS, &entry.SizeBytes, &entry.UsedBytesMax, &entry.AvailBytesMin,
			&entry.InodesUsedMax); err != nil {
			return nil, fmt.Errorf("scan disk series: %w", err)
		}
		bins = append(bins, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read disk series: %w", err)
	}
	return bins, nil
}
