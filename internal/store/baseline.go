package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// BaselineDays is how many previous days a baseline looks at.
const BaselineDays = 7

// BaselineMinDays is the number of qualifying days a baseline needs; with
// fewer, its values are stored as missing.
const BaselineMinDays = 3

// ComputeBaselines computes the error rate and p95 latency baselines for
// every route and every hour of the day from the previous seven days, and
// replaces the baselines table. Hours with fewer than minRequests requests
// and hours that overlap an incident on the route are skipped.
func (s *Store) ComputeBaselines(ctx context.Context, location *time.Location, minRequests int, now time.Time) error {
	if location == nil {
		location = time.UTC
	}
	local := now.In(location)
	to := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	from := to.AddDate(0, 0, -BaselineDays)
	rows, err := s.baselineHours(ctx, from.UTC(), to.UTC(), minRequests)
	if err != nil {
		return err
	}
	samples := make(map[baselineKey][]baselineSample)
	for _, row := range rows {
		hour := row.ts.In(location).Hour()
		histogram, err := LoadHistogram(row.latency)
		if err != nil {
			return fmt.Errorf("baseline for route %d: %w", row.routeID, err)
		}
		p95, _ := histogram.Percentile(95)
		key := baselineKey{routeID: row.routeID, hour: hour}
		samples[key] = append(samples[key], baselineSample{
			errorRate: float64(row.s5xx) / float64(row.requests),
			p95:       p95,
		})
	}
	routeIDs, err := s.routeIDs(ctx)
	if err != nil {
		return err
	}
	return s.replaceBaselines(ctx, routeIDs, samples, now)
}

// baselineHour is one hourly row used for a baseline.
type baselineHour struct {
	routeID        int64
	ts             time.Time
	requests, s5xx int64
	latency        []int32
}

// baselineSample is one qualifying hour's values.
type baselineSample struct {
	errorRate float64
	p95       float64
}

// baselineKey identifies one route's hour of the day.
type baselineKey struct {
	routeID int64
	hour    int
}

// baselineHours reads the qualifying hourly rows in a time range.
func (s *Store) baselineHours(ctx context.Context, from, to time.Time, minRequests int) ([]baselineHour, error) {
	rows, err := s.pool.Query(ctx, `
		select rh.route_id, rh.ts, rh.requests, rh.s5xx, rh.latency
		from route_hour rh
		where rh.ts >= $1 and rh.ts < $2 and rh.requests >= $3
		  and not exists (
		    select 1 from incidents i
		    where i.route_id = rh.route_id
		      and i.opened_at < rh.ts + interval '1 hour'
		      and (i.resolved_at is null or i.resolved_at > rh.ts)
		  )
		order by rh.route_id, rh.ts`, from, to, minRequests)
	if err != nil {
		return nil, fmt.Errorf("read baseline hours: %w", err)
	}
	defer rows.Close()
	var hours []baselineHour
	for rows.Next() {
		var hour baselineHour
		if err := rows.Scan(&hour.routeID, &hour.ts, &hour.requests, &hour.s5xx, &hour.latency); err != nil {
			return nil, fmt.Errorf("scan baseline hour: %w", err)
		}
		hours = append(hours, hour)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read baseline hours: %w", err)
	}
	return hours, nil
}

// routeIDs returns the IDs of every route.
func (s *Store) routeIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `select id from routes order by id`)
	if err != nil {
		return nil, fmt.Errorf("read route IDs: %w", err)
	}
	defer rows.Close()
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read route IDs: %w", err)
	}
	return ids, nil
}

// replaceBaselines writes one row per route and hour of the day.
func (s *Store) replaceBaselines(ctx context.Context, routeIDs []int64, samples map[baselineKey][]baselineSample, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin baselines: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.logger.Warn("roll back baselines", slog.String("error", err.Error()))
		}
	}()
	if _, err := tx.Exec(ctx, `delete from baselines`); err != nil {
		return fmt.Errorf("clear baselines: %w", err)
	}
	for _, routeID := range routeIDs {
		for hour := 0; hour < 24; hour++ {
			values := samples[baselineKey{routeID: routeID, hour: hour}]
			var (
				errorRate *float64
				p95       *float64
			)
			if len(values) >= BaselineMinDays {
				medianError := median(values, func(sample baselineSample) float64 { return sample.errorRate })
				medianP95 := median(values, func(sample baselineSample) float64 { return sample.p95 })
				errorRate, p95 = &medianError, &medianP95
			}
			if _, err := tx.Exec(ctx, `
				insert into baselines (route_id, hour_of_day, error_rate, p95_ms, days_used, computed_at)
				values ($1, $2, $3, $4, $5, $6)`,
				routeID, hour, errorRate, p95, len(values), now); err != nil {
				return fmt.Errorf("write baseline for route %d hour %d: %w", routeID, hour, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit baselines: %w", err)
	}
	return nil
}

// median returns the median of the values selected from the samples.
func median(samples []baselineSample, value func(baselineSample) float64) float64 {
	values := make([]float64, len(samples))
	for i, sample := range samples {
		values[i] = value(sample)
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}
