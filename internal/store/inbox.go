package store

import (
	"context"
	"fmt"
	"time"
)

// ClaimUpdate records an update_id and reports whether this process owns
// it. The insert is atomic, so a redelivered update is claimed once even
// when two polls race.
func (s *Store) ClaimUpdate(ctx context.Context, updateID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		insert into telegram_inbox (update_id, processed_at)
		values ($1, now())
		on conflict (update_id) do nothing`,
		updateID)
	if err != nil {
		return false, fmt.Errorf("claim update %d: %w", updateID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// PruneProcessedUpdates drops inbox rows older than olderThan. The
// goroutine runs this once an hour so the table never grows unbounded.
func (s *Store) PruneProcessedUpdates(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`delete from telegram_inbox where processed_at < now() - make_interval(secs => $1)`,
		int64(olderThan.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("prune telegram_inbox: %w", err)
	}
	return tag.RowsAffected(), nil
}
