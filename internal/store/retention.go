package store

import (
	"context"
	"fmt"
	"time"
)

// retentionBatch is how many rows are deleted in one transaction.
const retentionBatch = 5000

// staleSubjectAfter is how long a systemd unit or a certificate may go
// without a refresh before it counts as gone and is dropped; a unit that no
// longer exists would otherwise show as failed forever.
const staleSubjectAfter = 7 * 24 * time.Hour

// Retention is how long each part of the history is kept.
type Retention struct {
	// Minute is the retention of the per-minute tables.
	Minute time.Duration
	// Paths is the retention of the per-path windows.
	Paths time.Duration
	// Hour is the retention of the hourly tables and docker_disk.
	Hour time.Duration
	// Events is the retention of events, incidents and deliveries.
	Events time.Duration
}

// Retention deletes the history that is older than the policy, in batches to
// keep the transactions short.
func (s *Store) Retention(ctx context.Context, policy Retention, now time.Time) error {
	minuteTables := []string{"route_minute", "slot_minute", "host_minute", "disk_minute"}
	for _, table := range minuteTables {
		if err := s.deleteBefore(ctx, table, "ts", now.Add(-policy.Minute)); err != nil {
			return err
		}
	}
	if err := s.deleteBefore(ctx, "path_window", "ts", now.Add(-policy.Paths)); err != nil {
		return err
	}
	hourTables := []string{"route_hour", "slot_hour", "host_hour", "disk_hour", "docker_disk"}
	for _, table := range hourTables {
		if err := s.deleteBefore(ctx, table, "ts", now.Add(-policy.Hour)); err != nil {
			return err
		}
	}
	if err := s.deleteBefore(ctx, "deliveries", "created_at", now.Add(-policy.Events)); err != nil {
		return err
	}
	if err := s.deleteBefore(ctx, "incidents", "opened_at", now.Add(-policy.Events)); err != nil {
		return err
	}
	if err := s.deleteBefore(ctx, "events", "ts", now.Add(-policy.Events)); err != nil {
		return err
	}
	if err := s.deleteBefore(ctx, "units", "last_seen", now.Add(-staleSubjectAfter)); err != nil {
		return err
	}
	return s.deleteBefore(ctx, "certs", "checked_at", now.Add(-staleSubjectAfter))
}

// deleteBefore deletes the rows of one table that are older than cutoff, one
// batch at a time.
func (s *Store) deleteBefore(ctx context.Context, table, column string, cutoff time.Time) error {
	for {
		result, err := s.pool.Exec(ctx, fmt.Sprintf(`
			delete from %s where ctid = any(array(
				select ctid from %s where %s < $1 limit $2
			))`, table, table, column), cutoff, retentionBatch)
		if err != nil {
			return fmt.Errorf("delete old %s: %w", table, err)
		}
		if result.RowsAffected() < retentionBatch {
			return nil
		}
	}
}
