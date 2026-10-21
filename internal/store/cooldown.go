package store

import (
	"context"
	"fmt"
)

// LastSent returns the most recent sent delivery for a (rule, subject)
// pair, or nil when nothing has been delivered for that pair yet.
// Used by the gate to implement pager cooldown.
func (s *Store) LastSent(ctx context.Context, rule, subject string) (*Delivery, error) {
	var d Delivery
	err := s.pool.QueryRow(ctx, `
		select d.id, d.incident_id, d.kind, d.channel, d.created_at, d.sent_at, d.attempts, d.status, d.error, d.telegram_message_id
		from deliveries d
		join incidents i on i.id = d.incident_id
		where i.rule = $1 and i.subject = $2
		  and d.status = 'sent'
		order by d.sent_at desc nulls last, d.id desc
		limit 1`, rule, subject).
		Scan(&d.ID, &d.IncidentID, &d.Kind, &d.Channel, &d.CreatedAt, &d.SentAt, &d.Attempts, &d.Status, &d.Error, &d.TelegramMessageID)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read last sent: %w", err)
	}
	return &d, nil
}

// OpenIncidentCount returns the number of currently open incidents.
func (s *Store) OpenIncidentCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from incidents where resolved_at is null`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count open incidents: %w", err)
	}
	return n, nil
}

// isNoRows matches pgx.ErrNoRows without importing it twice across
// packages; callers use the result to convert empty results into nil.
func isNoRows(err error) bool {
	return err != nil && (err.Error() == "no rows in result set" ||
		err.Error() == "ErrNoRows")
}
