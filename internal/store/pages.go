package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PageStatus is the state of one durable notification obligation.
type PageStatus string

// The page states.
const (
	// PagePending needs an announcement (or an escalation).
	PagePending PageStatus = "pending"
	// PageAnnounced means the announcement reached the recipient; the
	// incident is still open.
	PageAnnounced PageStatus = "announced"
	// PageResolving means the incident resolved and the recipient was
	// announced, so a resolution is owed.
	PageResolving PageStatus = "resolving"
	// PageResolved means the resolution was sent.
	PageResolved PageStatus = "resolved"
	// PageCancelled is terminal: acked, app alerts off, resolved before it
	// was announced, or deliberately dropped by policy.
	PageCancelled PageStatus = "cancelled"
)

// Page is one row of the pages table: the durable state of one notification
// obligation for an incident episode, channel and recipient.
type Page struct {
	ID          int64
	IncidentID  int64
	Episode     int64
	Channel     string
	Recipient   string
	Status      PageStatus
	Kind        DeliveryKind
	Severity    Severity
	DueAt       time.Time
	Reason      string
	SilenceID   *int64
	Attempts    int
	LastError   *string
	AnnouncedAt *time.Time
	ResolvedAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// pageColumns is the SELECT list scanPage reads.
const pageColumns = `id, incident_id, episode, channel, recipient, status, kind, severity, due_at,
	reason, silence_id, attempts, last_error, announced_at, resolved_at, created_at, updated_at`

// scanPage reads one row selected with pageColumns into p, followed by any
// extra columns.
func scanPage(row pgx.Row, p *Page, extra ...any) error {
	fields := append([]any{&p.ID, &p.IncidentID, &p.Episode, &p.Channel, &p.Recipient, &p.Status,
		&p.Kind, &p.Severity, &p.DueAt, &p.Reason, &p.SilenceID, &p.Attempts, &p.LastError,
		&p.AnnouncedAt, &p.ResolvedAt, &p.CreatedAt, &p.UpdatedAt}, extra...)
	return row.Scan(fields...)
}

// CreatePage inserts one page. An existing (incident, episode, channel,
// recipient) row is left untouched and returned instead.
func (s *Store) CreatePage(ctx context.Context, page Page) (Page, error) {
	var created Page
	err := scanPage(s.pool.QueryRow(ctx, `
		insert into pages (incident_id, episode, channel, recipient, status, kind, severity, due_at,
		                   reason, silence_id, attempts, last_error, announced_at, resolved_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		on conflict (incident_id, episode, channel, recipient) do nothing
		returning `+pageColumns,
		page.IncidentID, page.Episode, page.Channel, page.Recipient, page.Status, page.Kind,
		page.Severity, page.DueAt, page.Reason, page.SilenceID, page.Attempts, page.LastError,
		page.AnnouncedAt, page.ResolvedAt), &created)
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Page{}, fmt.Errorf("create page: %w", err)
	}
	existing, err := s.Page(ctx, page.IncidentID, page.Episode, page.Channel, page.Recipient)
	if err != nil {
		return Page{}, err
	}
	if existing == nil {
		return Page{}, fmt.Errorf("create page: conflict but no row")
	}
	return *existing, nil
}

// Page returns the page of one incident episode, channel and recipient, or
// nil when there is none.
func (s *Store) Page(ctx context.Context, incidentID, episode int64, channel, recipient string) (*Page, error) {
	var page Page
	err := scanPage(s.pool.QueryRow(ctx, `select `+pageColumns+`
		from pages
		where incident_id = $1 and episode = $2 and channel = $3 and recipient = $4`,
		incidentID, episode, channel, recipient), &page)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}
	return &page, nil
}

// PagesForEpisode returns every page of one incident episode, ordered by
// channel and recipient so the planner has a stable order.
func (s *Store) PagesForEpisode(ctx context.Context, incidentID, episode int64) ([]Page, error) {
	rows, err := s.pool.Query(ctx, `select `+pageColumns+`
		from pages
		where incident_id = $1 and episode = $2
		order by channel, recipient`, incidentID, episode)
	if err != nil {
		return nil, fmt.Errorf("read pages: %w", err)
	}
	defer rows.Close()
	var pages []Page
	for rows.Next() {
		var page Page
		if err := scanPage(rows, &page); err != nil {
			return nil, fmt.Errorf("scan page: %w", err)
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pages: %w", err)
	}
	return pages, nil
}

// DuePages returns the pages that may run at now, oldest due first.
func (s *Store) DuePages(ctx context.Context, now time.Time, limit int) ([]Page, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `select `+pageColumns+`
		from pages
		where status in ('pending', 'resolving') and due_at <= $1
		order by due_at, id
		limit $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("read due pages: %w", err)
	}
	defer rows.Close()
	var pages []Page
	for rows.Next() {
		var page Page
		if err := scanPage(rows, &page); err != nil {
			return nil, fmt.Errorf("scan due page: %w", err)
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due pages: %w", err)
	}
	return pages, nil
}

// UpdatePage writes the mutable state of one page.
func (s *Store) UpdatePage(ctx context.Context, page Page) error {
	_, err := s.pool.Exec(ctx, `
		update pages
		set status = $2, kind = $3, severity = $4, due_at = $5, reason = $6, silence_id = $7,
		    attempts = $8, last_error = $9, announced_at = $10, resolved_at = $11, updated_at = now()
		where id = $1`,
		page.ID, page.Status, page.Kind, page.Severity, page.DueAt, page.Reason, page.SilenceID,
		page.Attempts, page.LastError, page.AnnouncedAt, page.ResolvedAt)
	if err != nil {
		return fmt.Errorf("update page %d: %w", page.ID, err)
	}
	return nil
}

// CancelPage marks one page cancelled with a reason.
func (s *Store) CancelPage(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx, `
		update pages set status = 'cancelled', reason = $2, due_at = now(), updated_at = now()
		where id = $1 and status in ('pending', 'resolving')`, id, reason)
	if err != nil {
		return fmt.Errorf("cancel page %d: %w", id, err)
	}
	return nil
}

// CancelPendingPages cancels every pending page of an incident. An ack uses
// its own transaction; this covers the planner's app-alerts=false and
// vanished-incident paths.
func (s *Store) CancelPendingPages(ctx context.Context, incidentID int64, reason string) error {
	_, err := s.pool.Exec(ctx, `
		update pages set status = 'cancelled', reason = $2, due_at = now(), updated_at = now()
		where incident_id = $1 and status in ('pending', 'resolving')`, incidentID, reason)
	if err != nil {
		return fmt.Errorf("cancel pages of incident %d: %w", incidentID, err)
	}
	return nil
}

// WakePagesForSilence brings forward the pages waiting on one silence, so
// removing a silence takes effect on the next dispatcher tick instead of when
// the original end time arrives.
func (s *Store) WakePagesForSilence(ctx context.Context, silenceID int64) error {
	_, err := s.pool.Exec(ctx, `
		update pages set due_at = now(), reason = 'silence removed', updated_at = now()
		where silence_id = $1 and status in ('pending', 'resolving')`, silenceID)
	if err != nil {
		return fmt.Errorf("wake pages of silence %d: %w", silenceID, err)
	}
	return nil
}

// Announcements returns how many incident episodes of a (rule, subject,
// channel) were announced since a time, and when the most recent one was
// announced. Suppressed attempts do not count: the flap budget is about
// pages the operator actually saw.
func (s *Store) Announcements(ctx context.Context, rule, subject, channel string, since time.Time) (int, *time.Time, error) {
	var count int
	var last *time.Time
	err := s.pool.QueryRow(ctx, `
		select count(distinct (p.incident_id, p.episode)), max(p.announced_at)
		from pages p
		join incidents i on i.id = p.incident_id
		where i.rule = $1 and i.subject = $2 and p.channel = $3
		  and p.announced_at is not null and p.announced_at >= $4`, rule, subject, channel, since).
		Scan(&count, &last)
	if err != nil {
		return 0, nil, fmt.Errorf("read announcements: %w", err)
	}
	return count, last, nil
}

// SentOpeningAt returns when an opening or escalation page was last sent to
// one recipient for an incident episode, or nil when none was sent. It seeds
// the durable page state from deliveries written by an earlier Raedar
// version or before a crash.
func (s *Store) SentOpeningAt(ctx context.Context, incidentID int64, openedAt time.Time, channel, recipient string) (*time.Time, error) {
	var sent *time.Time
	err := s.pool.QueryRow(ctx, `
		select max(d.sent_at)
		from deliveries d
		where d.incident_id = $1
		  and d.kind in ('opened', 'escalated')
		  and d.status = 'sent'
		  and d.created_at >= $2
		  and d.channel = $3
		  and (
		      d.recipient = $4
		      or (d.recipient = '' and $3 <> 'telegram')
		      or ($3 = 'telegram' and d.telegram_chat_id::text = $4)
		  )`, incidentID, openedAt, channel, recipient).Scan(&sent)
	if err != nil {
		return nil, fmt.Errorf("read sent opening: %w", err)
	}
	return sent, nil
}
