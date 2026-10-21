package store

import (
	"context"
	"fmt"
	"time"
)

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return nil
}

// AllEvents returns every event in a time range, newest first.
func (s *Store) AllEvents(ctx context.Context, from, to time.Time) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		select id, ts, kind, severity, app_id, slot_id, route_id, subject, title, data
		from events where ts >= $1 and ts < $2 order by ts desc, id desc`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.TS, &event.Kind, &event.Severity, &event.AppID, &event.SlotID,
			&event.RouteID, &event.Subject, &event.Title, &event.Data); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return events, nil
}

// LatestEvents returns the newest events, newest first.
func (s *Store) LatestEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		select id, ts, kind, severity, app_id, slot_id, route_id, subject, title, data
		from events order by ts desc, id desc limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read latest events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.TS, &event.Kind, &event.Severity, &event.AppID, &event.SlotID,
			&event.RouteID, &event.Subject, &event.Title, &event.Data); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read latest events: %w", err)
	}
	return events, nil
}

// TableSize is the disk usage of one table.
type TableSize struct {
	// Name is the table name.
	Name string
	// Bytes is the table size including its indexes.
	Bytes int64
}

// TableSizes returns the size of every table in the current schema.
func (s *Store) TableSizes(ctx context.Context) ([]TableSize, error) {
	rows, err := s.pool.Query(ctx, `
		select relname, pg_total_relation_size(c.oid)
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = current_schema() and c.relkind = 'r'
		order by relname`)
	if err != nil {
		return nil, fmt.Errorf("read table sizes: %w", err)
	}
	defer rows.Close()
	var sizes []TableSize
	for rows.Next() {
		var size TableSize
		if err := rows.Scan(&size.Name, &size.Bytes); err != nil {
			return nil, fmt.Errorf("scan table size: %w", err)
		}
		sizes = append(sizes, size)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read table sizes: %w", err)
	}
	return sizes, nil
}

// AppMinuteStats sums the minute data of every app that had requests in a
// range, by app ID.
func (s *Store) AppMinuteStats(ctx context.Context, from, to time.Time) (map[int64]AppDayStats, error) {
	rows, err := s.pool.Query(ctx, `
		select r.app_id, sum(rm.requests), sum(rm.s5xx), raedar_hist_sum(rm.latency)
		from route_minute rm
		join routes r on r.id = rm.route_id
		where r.app_id is not null and rm.ts >= $1 and rm.ts < $2
		group by r.app_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read app minute stats: %w", err)
	}
	defer rows.Close()
	stats := make(map[int64]AppDayStats)
	for rows.Next() {
		var (
			entry     AppDayStats
			histogram []int32
		)
		if err := rows.Scan(&entry.AppID, &entry.Requests, &entry.S5xx, &histogram); err != nil {
			return nil, fmt.Errorf("scan app minute stats: %w", err)
		}
		if entry.Latency, err = LoadHistogram(histogram); err != nil {
			return nil, fmt.Errorf("read app minute stats: %w", err)
		}
		stats[entry.AppID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read app minute stats: %w", err)
	}
	return stats, nil
}

// AppHourlyRequests returns the requests per hour since from for every app,
// by app ID, from the hourly tables.
func (s *Store) AppHourlyRequests(ctx context.Context, from time.Time) (map[int64]map[time.Time]int64, error) {
	rows, err := s.pool.Query(ctx, `
		select r.app_id, rh.ts, sum(rh.requests)
		from route_hour rh
		join routes r on r.id = rh.route_id
		where r.app_id is not null and rh.ts >= $1
		group by r.app_id, rh.ts`, from)
	if err != nil {
		return nil, fmt.Errorf("read hourly requests: %w", err)
	}
	defer rows.Close()
	byApp := make(map[int64]map[time.Time]int64)
	for rows.Next() {
		var (
			appID int64
			ts    time.Time
			count int64
		)
		if err := rows.Scan(&appID, &ts, &count); err != nil {
			return nil, fmt.Errorf("scan hourly requests: %w", err)
		}
		if byApp[appID] == nil {
			byApp[appID] = make(map[time.Time]int64)
		}
		byApp[appID][ts.UTC()] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read hourly requests: %w", err)
	}
	return byApp, nil
}

// RoutePathStat is the summed path data of one route and pattern.
type RoutePathStat struct {
	RouteID  int64
	Method   string
	Pattern  string
	Requests int64
	S4xx     int64
	S5xx     int64
	Latency  Histogram
}

// RoutePathStats sums the path windows of the given routes over a range.
func (s *Store) RoutePathStats(ctx context.Context, routeIDs []int64, from, to time.Time) ([]RoutePathStat, error) {
	if len(routeIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		select route_id, method, pattern, sum(requests), sum(s4xx), sum(s5xx), raedar_hist_sum(latency)
		from path_window
		where route_id = any($1) and ts >= $2 and ts < $3
		group by route_id, method, pattern`, routeIDs, from, to)
	if err != nil {
		return nil, fmt.Errorf("read path stats: %w", err)
	}
	defer rows.Close()
	var stats []RoutePathStat
	for rows.Next() {
		var (
			stat      RoutePathStat
			histogram []int32
		)
		if err := rows.Scan(&stat.RouteID, &stat.Method, &stat.Pattern, &stat.Requests, &stat.S4xx,
			&stat.S5xx, &histogram); err != nil {
			return nil, fmt.Errorf("scan path stat: %w", err)
		}
		if stat.Latency, err = LoadHistogram(histogram); err != nil {
			return nil, fmt.Errorf("read path stats: %w", err)
		}
		stats = append(stats, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read path stats: %w", err)
	}
	return stats, nil
}
