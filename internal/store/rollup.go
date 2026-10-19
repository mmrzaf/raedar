package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RollupHours is how many complete hours are recomputed on every rollup.
const RollupHours = 3

// TruncateLocalHour returns the start of the hour that contains t in a
// location. Hourly rows start on local hour boundaries, so hour-of-day
// statistics are exact in zones whose offset is not a whole number of hours,
// such as Asia/Tehran (UTC+03:30).
func TruncateLocalHour(t time.Time, location *time.Location) time.Time {
	local := t.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location).UTC()
}

// Rollup recomputes the hourly tables for the last three complete local hours
// from the minute tables, replacing the rows of those hours. Recomputing more
// than one hour absorbs late access log lines and restarts.
func (s *Store) Rollup(ctx context.Context, now time.Time, location *time.Location) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rollup: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.logger.Warn("roll back rollup", slog.String("error", err.Error()))
		}
	}()
	hour := TruncateLocalHour(now, location)
	for i := 1; i <= RollupHours; i++ {
		if err := rollupHour(ctx, tx, hour.Add(-time.Duration(i)*time.Hour)); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollup: %w", err)
	}
	return nil
}

// rollupTables are the tables that hold one row per local hour.
var rollupTables = []string{"route_hour", "slot_hour", "host_hour", "disk_hour"}

// RealignRollups rebuilds the hourly rows that do not start on a local hour
// boundary, which happens when the time zone changes or when rows were written
// before hours followed the local zone. Only hours that the minute tables still
// cover can be rebuilt; older rows stay as they are. It returns how many hours
// it recomputed.
func (s *Store) RealignRollups(ctx context.Context, location *time.Location, minuteRetention time.Duration, now time.Time) (int, error) {
	zone := location.String()
	// The first complete local hour that the minute tables fully cover.
	start := TruncateLocalHour(now.Add(-minuteRetention), location).Add(time.Hour)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin realign: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.logger.Warn("roll back realign", slog.String("error", err.Error()))
		}
	}()
	removed := int64(0)
	for _, table := range rollupTables {
		tag, err := tx.Exec(ctx, "delete from "+table+" where ts >= $1 and ts <> date_trunc('hour', ts, $2)", start, zone)
		if err != nil {
			return 0, fmt.Errorf("clear misaligned %s: %w", table, err)
		}
		removed += tag.RowsAffected()
	}
	if removed == 0 {
		return 0, nil
	}
	hours := 0
	end := TruncateLocalHour(now, location)
	for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
		if err := rollupHour(ctx, tx, hour); err != nil {
			return 0, err
		}
		hours++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit realign: %w", err)
	}
	return hours, nil
}

// rollupHour recomputes the hourly rows of one hour.
func rollupHour(ctx context.Context, tx pgx.Tx, hour time.Time) error {
	statements := []struct {
		name string
		sql  string
	}{
		{
			name: "route_hour",
			sql: `
				insert into route_hour (route_id, ts, requests, s1xx, s2xx, s3xx, s4xx, s5xx, no_backend, bytes, latency)
				select route_id, $1, sum(requests), sum(s1xx), sum(s2xx), sum(s3xx), sum(s4xx), sum(s5xx),
				       sum(no_backend), sum(bytes), raedar_hist_sum(latency)
				from route_minute
				where ts >= $1 and ts < $1 + interval '1 hour'
				group by route_id`,
		},
		{
			name: "slot_hour",
			sql: `
				insert into slot_hour (slot_id, ts, cpu_avg, cpu_max, mem_avg, mem_max, mem_limit_bytes, net_rx_bytes, net_tx_bytes)
				select slot_id, $1, avg(cpu_millicores), max(cpu_millicores), avg(mem_bytes), max(mem_bytes),
				       max(mem_limit_bytes), sum(net_rx_bytes), sum(net_tx_bytes)
				from slot_minute
				where ts >= $1 and ts < $1 + interval '1 hour'
				group by slot_id`,
		},
		{
			name: "host_hour",
			sql: `
				insert into host_hour (ts, cpu_avg, cpu_max, load1_avg, load1_max, mem_available_min, mem_available_avg, swap_used_max)
				select $1, avg(cpu_percent), max(cpu_percent), avg(load1), max(load1),
				       min(mem_available), avg(mem_available), max(swap_used)
				from host_minute
				where ts >= $1 and ts < $1 + interval '1 hour'
				having count(*) > 0`,
		},
		{
			name: "disk_hour",
			sql: `
				insert into disk_hour (disk, ts, size_bytes, used_bytes_max, avail_bytes_min, inodes, inodes_used_max)
				select disk, $1, max(size_bytes), max(used_bytes), min(avail_bytes), max(inodes), max(inodes_used)
				from disk_minute
				where ts >= $1 and ts < $1 + interval '1 hour'
				group by disk`,
		},
	}
	for _, statement := range statements {
		table := strings.TrimSuffix(statement.name, "_hour")
		if _, err := tx.Exec(ctx, "delete from "+statement.name+" where ts = $1", hour); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
		if _, err := tx.Exec(ctx, statement.sql, hour); err != nil {
			return fmt.Errorf("roll up %s: %w", table, err)
		}
	}
	return nil
}
