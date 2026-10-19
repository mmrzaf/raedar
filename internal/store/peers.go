package store

import (
	"context"
	"fmt"
	"time"
)

// RecordHeartbeat records that a peer reported.
func (s *Store) RecordHeartbeat(ctx context.Context, name string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		insert into peers (name, last_heartbeat) values ($1, $2)
		on conflict (name) do update set last_heartbeat = excluded.last_heartbeat`, name, at)
	if err != nil {
		return fmt.Errorf("record heartbeat of %s: %w", name, err)
	}
	return nil
}
