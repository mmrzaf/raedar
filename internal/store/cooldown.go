package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// LastSent returns the most recent sent opening or escalation page for a
// (rule, subject) pair, or nil when none was sent yet. The gate uses it to
// implement the pager cooldown; resolutions and tests do not count.
func (s *Store) LastSent(ctx context.Context, rule, subject string) (*Delivery, error) {
	var d Delivery
	row := s.pool.QueryRow(ctx, `
		select `+deliveryColumns+`
		from deliveries d
		join incidents i on i.id = d.incident_id
		where i.rule = $1 and i.subject = $2
		  and d.status = 'sent' and d.kind in ('opened', 'escalated')
		order by d.sent_at desc nulls last, d.id desc
		limit 1`, rule, subject)
	err := scanDelivery(row, &d)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last sent: %w", err)
	}
	return &d, nil
}

// RecentOpenCount returns how many opening pages were recorded for a (rule,
// subject, channel) since a time. Reopened incidents reuse their incident row,
// so the delivery rows are the only history of earlier episodes. The count
// includes pages the gate suppressed, which is what makes flap detection see
// every episode rather than only the ones that were sent.
func (s *Store) RecentOpenCount(ctx context.Context, rule, subject, channel string, since time.Time) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		select count(distinct d.created_at)
		from deliveries d
		join incidents i on i.id = d.incident_id
		where i.rule = $1 and i.subject = $2 and d.channel = $3
		  and d.kind in ('opened', 'escalated') and d.created_at >= $4`,
		rule, subject, channel, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count recent pages: %w", err)
	}
	return count, nil
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
