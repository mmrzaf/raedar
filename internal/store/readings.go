package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RouteWindow is the summed request data of one route over a window.
type RouteWindow struct {
	// RouteID is the route.
	RouteID int64
	// Requests counts all requests.
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

// RouteWindows sums the minute data of every route over a window.
func (s *Store) RouteWindows(ctx context.Context, from, to time.Time) ([]RouteWindow, error) {
	rows, err := s.pool.Query(ctx, `
		select route_id, sum(requests), sum(s1xx), sum(s2xx), sum(s3xx), sum(s4xx), sum(s5xx),
		       sum(no_backend), sum(bytes), raedar_hist_sum(latency)
		from route_minute
		where ts >= $1 and ts < $2
		group by route_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read route windows: %w", err)
	}
	defer rows.Close()
	var windows []RouteWindow
	for rows.Next() {
		var (
			window    RouteWindow
			histogram []int32
		)
		if err := rows.Scan(&window.RouteID, &window.Requests, &window.S1xx, &window.S2xx, &window.S3xx,
			&window.S4xx, &window.S5xx, &window.NoBackend, &window.Bytes, &histogram); err != nil {
			return nil, fmt.Errorf("scan route window: %w", err)
		}
		if window.Latency, err = LoadHistogram(histogram); err != nil {
			return nil, fmt.Errorf("read route windows: %w", err)
		}
		windows = append(windows, window)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read route windows: %w", err)
	}
	return windows, nil
}

// PathWindowSummary is the summed path data of one route and pattern.
type PathWindowSummary struct {
	// RouteID is the route.
	RouteID int64
	// Pattern is the normalized request path.
	Pattern string
	// Requests counts the requests.
	Requests int64
	// S4xx and S5xx count the failing responses.
	S4xx, S5xx int64
	// Latency is the summed latency histogram.
	Latency Histogram
}

// PathWindows sums the path windows of every route and pattern over a window.
func (s *Store) PathWindows(ctx context.Context, from, to time.Time) ([]PathWindowSummary, error) {
	rows, err := s.pool.Query(ctx, `
		select route_id, pattern, sum(requests), sum(s4xx), sum(s5xx), raedar_hist_sum(latency)
		from path_window
		where ts >= $1 and ts < $2
		group by route_id, pattern`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read path windows: %w", err)
	}
	defer rows.Close()
	var windows []PathWindowSummary
	for rows.Next() {
		var (
			window    PathWindowSummary
			histogram []int32
		)
		if err := rows.Scan(&window.RouteID, &window.Pattern, &window.Requests, &window.S4xx, &window.S5xx, &histogram); err != nil {
			return nil, fmt.Errorf("scan path window: %w", err)
		}
		if window.Latency, err = LoadHistogram(histogram); err != nil {
			return nil, fmt.Errorf("read path windows: %w", err)
		}
		windows = append(windows, window)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read path windows: %w", err)
	}
	return windows, nil
}

// SlotSample is the newest sample of one slot.
type SlotSample struct {
	// SlotID is the slot.
	SlotID int64
	// TS is when the sample was taken.
	TS time.Time
	// CPUMillicores is the CPU usage in millicores.
	CPUMillicores int
	// MemBytes is the working-set memory.
	MemBytes int64
	// MemLimitBytes is the memory limit; nil when there is none.
	MemLimitBytes *int64
	// Running reports whether the container was running.
	Running bool
}

// SlotSamples returns the newest sample of every slot at or after from.
func (s *Store) SlotSamples(ctx context.Context, from time.Time) (map[int64]SlotSample, error) {
	rows, err := s.pool.Query(ctx, `
		select distinct on (slot_id) slot_id, ts, cpu_millicores, mem_bytes, mem_limit_bytes, running
		from slot_minute
		where ts >= $1
		order by slot_id, ts desc`, from)
	if err != nil {
		return nil, fmt.Errorf("read slot samples: %w", err)
	}
	defer rows.Close()
	samples := make(map[int64]SlotSample)
	for rows.Next() {
		var sample SlotSample
		if err := rows.Scan(&sample.SlotID, &sample.TS, &sample.CPUMillicores, &sample.MemBytes,
			&sample.MemLimitBytes, &sample.Running); err != nil {
			return nil, fmt.Errorf("scan slot sample: %w", err)
		}
		samples[sample.SlotID] = sample
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read slot samples: %w", err)
	}
	return samples, nil
}

// Events returns the events of one kind in a time range, oldest first.
func (s *Store) Events(ctx context.Context, kind EventKind, from, to time.Time) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		select id, ts, kind, severity, app_id, slot_id, route_id, subject, title, data
		from events
		where kind = $1 and ts >= $2 and ts < $3
		order by ts`, kind, from, to)
	if err != nil {
		return nil, fmt.Errorf("read %s events: %w", kind, err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.TS, &event.Kind, &event.Severity, &event.AppID, &event.SlotID,
			&event.RouteID, &event.Subject, &event.Title, &event.Data); err != nil {
			return nil, fmt.Errorf("scan %s event: %w", kind, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s events: %w", kind, err)
	}
	return events, nil
}

// LatestEventByApp returns, for each app, the time of its newest event of any
// of the given kinds.
func (s *Store) LatestEventByApp(ctx context.Context, kinds ...EventKind) (map[int64]time.Time, error) {
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	rows, err := s.pool.Query(ctx, `
		select app_id, max(ts) from events
		where kind = any($1) and app_id is not null
		group by app_id`, names)
	if err != nil {
		return nil, fmt.Errorf("read latest events per app: %w", err)
	}
	defer rows.Close()
	latest := make(map[int64]time.Time)
	for rows.Next() {
		var (
			appID int64
			at    time.Time
		)
		if err := rows.Scan(&appID, &at); err != nil {
			return nil, fmt.Errorf("scan latest event: %w", err)
		}
		latest[appID] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read latest events per app: %w", err)
	}
	return latest, nil
}

// LatestEventTime returns the time of the newest event of one kind, or the
// zero time when there is none.
func (s *Store) LatestEventTime(ctx context.Context, kind EventKind) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `select max(ts) from events where kind = $1`, kind).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("read latest %s event: %w", kind, err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return at.UTC(), nil
}

// Units returns every recorded unit.
func (s *Store) Units(ctx context.Context) ([]Unit, error) {
	rows, err := s.pool.Query(ctx, `
		select name, active_state, sub_state, result, exit_status, since, last_seen from units order by name`)
	if err != nil {
		return nil, fmt.Errorf("read units: %w", err)
	}
	defer rows.Close()
	var units []Unit
	for rows.Next() {
		var unit Unit
		if err := rows.Scan(&unit.Name, &unit.ActiveState, &unit.SubState, &unit.Result, &unit.ExitStatus,
			&unit.Since, &unit.LastSeen); err != nil {
			return nil, fmt.Errorf("scan unit: %w", err)
		}
		units = append(units, unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read units: %w", err)
	}
	return units, nil
}

// Certs returns every recorded certificate check.
func (s *Store) Certs(ctx context.Context) ([]Cert, error) {
	rows, err := s.pool.Query(ctx, `
		select domain, route_id, checked_at, status, not_after, issuer, subject, sans, coalesce(error, '') from certs order by domain`)
	if err != nil {
		return nil, fmt.Errorf("read certificates: %w", err)
	}
	defer rows.Close()
	var certs []Cert
	for rows.Next() {
		var cert Cert
		if err := rows.Scan(&cert.Domain, &cert.RouteID, &cert.CheckedAt, &cert.Status, &cert.NotAfter,
			&cert.Issuer, &cert.Subject, &cert.SANs, &cert.Error); err != nil {
			return nil, fmt.Errorf("scan certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read certificates: %w", err)
	}
	return certs, nil
}

// Peer is one row of the peers table.
type Peer struct {
	// Name is the peer name.
	Name string
	// LastHeartbeat is when the peer last reported.
	LastHeartbeat time.Time
}

// Peers returns every recorded peer.
func (s *Store) Peers(ctx context.Context) ([]Peer, error) {
	rows, err := s.pool.Query(ctx, `select name, last_heartbeat from peers order by name`)
	if err != nil {
		return nil, fmt.Errorf("read peers: %w", err)
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var peer Peer
		if err := rows.Scan(&peer.Name, &peer.LastHeartbeat); err != nil {
			return nil, fmt.Errorf("scan peer: %w", err)
		}
		peers = append(peers, peer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read peers: %w", err)
	}
	return peers, nil
}

// SourceState is one row of the source_state table.
type SourceState struct {
	// Source is the source name.
	Source string
	// Position is the reader position as JSON; nil when there is none.
	Position []byte
	// Status is ok, degraded or unavailable.
	Status string
	// LastError is the last error message; nil when there is none.
	LastError *string
	// UpdatedAt is when the state was written.
	UpdatedAt time.Time
}

// SourceStates returns the recorded state of every source.
func (s *Store) SourceStates(ctx context.Context) ([]SourceState, error) {
	rows, err := s.pool.Query(ctx, `select source, position, status, last_error, updated_at from source_state order by source`)
	if err != nil {
		return nil, fmt.Errorf("read source states: %w", err)
	}
	defer rows.Close()
	var states []SourceState
	for rows.Next() {
		var state SourceState
		if err := rows.Scan(&state.Source, &state.Position, &state.Status, &state.LastError, &state.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan source state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read source states: %w", err)
	}
	return states, nil
}

// Baseline is one row of the baselines table.
type Baseline struct {
	// RouteID is the route.
	RouteID int64
	// HourOfDay is the local hour, 0 to 23.
	HourOfDay int
	// ErrorRate is the median error rate; nil when it is missing.
	ErrorRate *float64
	// P95MS is the median p95 latency in milliseconds; nil when it is
	// missing.
	P95MS *float64
	// DaysUsed is the number of qualifying days.
	DaysUsed int
	// ComputedAt is when the baseline was computed.
	ComputedAt time.Time
}

// Baselines returns every computed baseline.
func (s *Store) Baselines(ctx context.Context) ([]Baseline, error) {
	rows, err := s.pool.Query(ctx, `
		select route_id, hour_of_day, error_rate, p95_ms, days_used, computed_at from baselines`)
	if err != nil {
		return nil, fmt.Errorf("read baselines: %w", err)
	}
	defer rows.Close()
	var baselines []Baseline
	for rows.Next() {
		var baseline Baseline
		if err := rows.Scan(&baseline.RouteID, &baseline.HourOfDay, &baseline.ErrorRate, &baseline.P95MS,
			&baseline.DaysUsed, &baseline.ComputedAt); err != nil {
			return nil, fmt.Errorf("scan baseline: %w", err)
		}
		baselines = append(baselines, baseline)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read baselines: %w", err)
	}
	return baselines, nil
}

// DiskSamples returns the disk samples of every disk at or after from, oldest
// first.
func (s *Store) DiskSamples(ctx context.Context, from time.Time) (map[string][]DiskMinute, error) {
	rows, err := s.pool.Query(ctx, `
		select disk, ts, size_bytes, used_bytes, avail_bytes, inodes, inodes_used
		from disk_minute where ts >= $1 order by disk, ts`, from)
	if err != nil {
		return nil, fmt.Errorf("read disk samples: %w", err)
	}
	defer rows.Close()
	samples := make(map[string][]DiskMinute)
	for rows.Next() {
		var sample DiskMinute
		if err := rows.Scan(&sample.Disk, &sample.TS, &sample.SizeBytes, &sample.UsedBytes,
			&sample.AvailBytes, &sample.Inodes, &sample.InodesUsed); err != nil {
			return nil, fmt.Errorf("scan disk sample: %w", err)
		}
		samples[sample.Disk] = append(samples[sample.Disk], sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read disk samples: %w", err)
	}
	return samples, nil
}

// LatestHostSample returns the newest host sample, or nil when there is none.
func (s *Store) LatestHostSample(ctx context.Context) (*HostMinute, error) {
	var sample HostMinute
	err := s.pool.QueryRow(ctx, `
		select ts, cpu_percent, load1, load5, load15, mem_total, mem_available, swap_total, swap_used
		from host_minute order by ts desc limit 1`).Scan(&sample.TS, &sample.CPUPercent, &sample.Load1,
		&sample.Load5, &sample.Load15, &sample.MemTotal, &sample.MemAvailable, &sample.SwapTotal, &sample.SwapUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest host sample: %w", err)
	}
	return &sample, nil
}
