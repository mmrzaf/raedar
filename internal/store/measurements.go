package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// RouteMinute is one row of the route_minute table.
type RouteMinute struct {
	// RouteID is the route the minute belongs to.
	RouteID int64
	// TS is the start of the minute, in UTC.
	TS time.Time
	// Requests is the number of requests in the minute.
	Requests int64
	// S1xx to S5xx count the responses by status class.
	S1xx, S2xx, S3xx, S4xx, S5xx int64
	// NoBackend counts requests Traefik answered because it could not reach a
	// backend.
	NoBackend int64
	// Bytes is the total response size.
	Bytes int64
	// Latency is the latency histogram.
	Latency []int32
}

// PathWindow is one row of the path_window table.
type PathWindow struct {
	// RouteID is the route the window belongs to.
	RouteID int64
	// TS is the start of the 10-minute window, in UTC.
	TS time.Time
	// Method is the request method.
	Method string
	// Pattern is the normalized request path.
	Pattern string
	// Requests is the number of requests for the pattern.
	Requests int64
	// S4xx and S5xx count the failing responses.
	S4xx, S5xx int64
	// Latency is the latency histogram.
	Latency []int32
}

// Flush is the content of one aggregator flush: measurements, the source
// position and any dropped-bucket events.
type Flush struct {
	// Minutes are the per-minute route buckets.
	Minutes []RouteMinute
	// Windows are the per-path window buckets.
	Windows []PathWindow
	// Source is the source the position belongs to.
	Source string
	// Position is the source's reader position as JSON; nil writes nothing.
	Position []byte
	// Gaps are events for measurements that were dropped.
	Gaps []Event
	// At is when the flush ran.
	At time.Time
}

// FlushBuckets adds minute and window measurements to their tables, records
// the source position and any gap events, all in one transaction.
func (s *Store) FlushBuckets(ctx context.Context, flush Flush) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin flush: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.logger.Warn("roll back flush", slog.String("error", err.Error()))
		}
	}()
	for _, minute := range flush.Minutes {
		latency := minute.Latency
		if latency == nil {
			latency = make([]int32, Buckets)
		}
		if _, err := tx.Exec(ctx, `
			insert into route_minute (route_id, ts, requests, s1xx, s2xx, s3xx, s4xx, s5xx, no_backend, bytes, latency)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			on conflict (route_id, ts) do update set
				requests = route_minute.requests + excluded.requests,
				s1xx = route_minute.s1xx + excluded.s1xx,
				s2xx = route_minute.s2xx + excluded.s2xx,
				s3xx = route_minute.s3xx + excluded.s3xx,
				s4xx = route_minute.s4xx + excluded.s4xx,
				s5xx = route_minute.s5xx + excluded.s5xx,
				no_backend = route_minute.no_backend + excluded.no_backend,
				bytes = route_minute.bytes + excluded.bytes,
				latency = raedar_hist_add(route_minute.latency, excluded.latency)`,
			minute.RouteID, minute.TS, minute.Requests, minute.S1xx, minute.S2xx, minute.S3xx,
			minute.S4xx, minute.S5xx, minute.NoBackend, minute.Bytes, latency); err != nil {
			return fmt.Errorf("write minute bucket: %w", err)
		}
	}
	for _, window := range flush.Windows {
		latency := window.Latency
		if latency == nil {
			latency = make([]int32, Buckets)
		}
		if _, err := tx.Exec(ctx, `
			insert into path_window (route_id, ts, method, pattern, requests, s4xx, s5xx, latency)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (route_id, ts, method, pattern) do update set
				requests = path_window.requests + excluded.requests,
				s4xx = path_window.s4xx + excluded.s4xx,
				s5xx = path_window.s5xx + excluded.s5xx,
				latency = raedar_hist_add(path_window.latency, excluded.latency)`,
			window.RouteID, window.TS, window.Method, window.Pattern, window.Requests,
			window.S4xx, window.S5xx, latency); err != nil {
			return fmt.Errorf("write window bucket: %w", err)
		}
	}
	if len(flush.Position) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into source_state (source, position, status, updated_at)
			values ($1, $2, 'ok', $3)
			on conflict (source) do update
			set position = excluded.position, updated_at = excluded.updated_at`,
			flush.Source, flush.Position, flush.At); err != nil {
			return fmt.Errorf("write source position: %w", err)
		}
	}
	for _, gap := range flush.Gaps {
		if _, err := tx.Exec(ctx, `
			insert into events (ts, kind, severity, app_id, slot_id, route_id, subject, title, data)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			gap.TS, gap.Kind, gap.Severity, gap.AppID, gap.SlotID, gap.RouteID,
			gap.Subject, gap.Title, gap.Data); err != nil {
			return fmt.Errorf("write gap event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit flush: %w", err)
	}
	return nil
}

// The states a source reports in source_state.
const (
	// StatusOK means the source is working.
	StatusOK = "ok"
	// StatusDegraded means the source is working with errors.
	StatusDegraded = "degraded"
	// StatusUnavailable means the source is not working.
	StatusUnavailable = "unavailable"
)

// SourcePosition returns the reader position recorded for one source, or nil
// when it has none yet.
func (s *Store) SourcePosition(ctx context.Context, source string) ([]byte, error) {
	var position []byte
	err := s.pool.QueryRow(ctx, `select position from source_state where source = $1`, source).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s position: %w", source, err)
	}
	return position, nil
}
