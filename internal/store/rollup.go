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

// Rollup recomputes the hourly tables for the last three complete hours from
// the minute tables, replacing the rows of those hours. Recomputing more than
// one hour absorbs late access log lines and restarts.
func (s *Store) Rollup(ctx context.Context, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rollup: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.logger.Warn("roll back rollup", slog.String("error", err.Error()))
		}
	}()
	hour := now.UTC().Truncate(time.Hour)
	for i := 1; i <= RollupHours; i++ {
		start := hour.Add(-time.Duration(i) * time.Hour)
		if err := rollupHour(ctx, tx, start); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollup: %w", err)
	}
	return nil
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
