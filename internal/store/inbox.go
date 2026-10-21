package store

import (
	"context"
	"fmt"
)

// RecordProcessedUpdate records an update_id we have just handled. Rows
// older than the inbox dedup window are pruned by the runner; for the
// tests we only need the insert path to behave correctly.
//
// The ON CONFLICT clause keeps the call idempotent across retries: a row
// for the same update_id is updated in place instead of failing on the
// primary-key collision.
func (s *Store) RecordProcessedUpdate(ctx context.Context, updateID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		insert into telegram_inbox (update_id, processed_at)
		values ($1, now())
		on conflict (update_id) do update set processed_at = excluded.processed_at`,
		updateID)
	if err != nil {
		return false, fmt.Errorf("record update %d: %w", updateID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// HasProcessedUpdate reports whether the update_id was already handled.
// Returns true when the row exists, false when the caller should process
// the update.
func (s *Store) HasProcessedUpdate(ctx context.Context, updateID int64) (bool, error) {
	var seen int64
	err := s.pool.QueryRow(ctx, `select update_id from telegram_inbox where update_id = $1`, updateID).Scan(&seen)
	if err != nil {
		return false, nil
	}
	return seen == updateID, nil
}

// PruneProcessedUpdates drops inbox rows older than maxAge. The goroutine
// runs this once per hour so the table never grows unbounded.
func (s *Store) PruneProcessedUpdates(ctx context.Context, maxAge int64) (int64, error) {
	tag, err := s.pool.Exec(ctx, `delete from telegram_inbox where processed_at < now() - make_interval(secs => $1)`, maxAge)
	if err != nil {
		return 0, fmt.Errorf("prune telegram_inbox: %w", err)
	}
	return tag.RowsAffected(), nil
}
