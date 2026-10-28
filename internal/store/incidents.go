package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Incident is one row of the incidents table.
type Incident struct {
	// ID is the incident ID.
	ID int64
	// Episode counts how many times this incident row has been opened. A
	// resolve-then-reopen starts a new episode, clears the ack and gets new
	// notification obligations.
	Episode int64
	// Rule is the rule that fired, for example route.errors.
	Rule string
	// Subject is what the rule fired for.
	Subject string
	// AppID, SlotID and RouteID link the incident to the model; all may be
	// nil.
	AppID   *int64
	SlotID  *int64
	RouteID *int64
	// Severity is warning or critical.
	Severity Severity
	// OpenedAt is when the condition started holding.
	OpenedAt time.Time
	// ResolvedAt is when the condition stopped holding; nil while open.
	ResolvedAt *time.Time
	// OneShot reports an incident that describes something that happened and
	// was resolved at once.
	OneShot bool
	// Summary is the one-line description.
	Summary string
	// Details is the incident's payload as JSON.
	Details []byte
	// AckedAt is when an operator first acked the incident; nil when open
	// and unacked.
	AckedAt *time.Time
	// AckedBy identifies who acked the incident; "" together with a nil
	// AckedAt means no one has acked it.
	AckedBy *string
}

// Acked reports whether the incident has been acked.
func (i Incident) Acked() bool { return i.AckedAt != nil }

// incidentColumns is the SELECT list scanIncident reads.
const incidentColumns = `id, episode, rule, subject, app_id, slot_id, route_id, severity, opened_at,
	resolved_at, one_shot, summary, details, acked_at, acked_by`

// scanIncident reads one row selected with incidentColumns into i, followed by
// any extra columns.
func scanIncident(row pgx.Row, i *Incident, extra ...any) error {
	fields := append([]any{&i.ID, &i.Episode, &i.Rule, &i.Subject, &i.AppID, &i.SlotID,
		&i.RouteID, &i.Severity, &i.OpenedAt, &i.ResolvedAt, &i.OneShot, &i.Summary,
		&i.Details, &i.AckedAt, &i.AckedBy}, extra...)
	return row.Scan(fields...)
}

// OpenIncidents returns the incidents that are not resolved, keyed by rule and
// subject.
func (s *Store) OpenIncidents(ctx context.Context) (map[IncidentKey]Incident, error) {
	rows, err := s.pool.Query(ctx, `select `+incidentColumns+`
		from incidents where resolved_at is null`)
	if err != nil {
		return nil, fmt.Errorf("read open incidents: %w", err)
	}
	defer rows.Close()
	incidents := make(map[IncidentKey]Incident)
	for rows.Next() {
		var incident Incident
		if err := scanIncident(rows, &incident); err != nil {
			return nil, fmt.Errorf("scan open incident: %w", err)
		}
		incidents[IncidentKey{Rule: incident.Rule, Subject: incident.Subject}] = incident
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open incidents: %w", err)
	}
	return incidents, nil
}

// IncidentsForNotify returns the open incidents and the incidents resolved at
// or after since, oldest first. The notification planner uses it to create
// obligations for new incidents and to turn announcements into resolutions.
func (s *Store) IncidentsForNotify(ctx context.Context, since time.Time) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `select `+incidentColumns+`
		from incidents
		where resolved_at is null or resolved_at >= $1
		order by id`, since)
	if err != nil {
		return nil, fmt.Errorf("read incidents for notify: %w", err)
	}
	defer rows.Close()
	var incidents []Incident
	for rows.Next() {
		var incident Incident
		if err := scanIncident(rows, &incident); err != nil {
			return nil, fmt.Errorf("scan incident for notify: %w", err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read incidents for notify: %w", err)
	}
	return incidents, nil
}

// IncidentKey identifies one rule's subject.
type IncidentKey struct {
	// Rule is the rule ID.
	Rule string
	// Subject is what the rule fired for.
	Subject string
}

// InsertIncident records a new incident and returns its ID. A new incident
// always starts at episode 1.
func (s *Store) InsertIncident(ctx context.Context, incident Incident) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into incidents (rule, subject, app_id, slot_id, route_id, severity, opened_at, resolved_at,
		                       one_shot, summary, details)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		returning id`,
		incident.Rule, incident.Subject, incident.AppID, incident.SlotID, incident.RouteID,
		incident.Severity, incident.OpenedAt, incident.ResolvedAt, incident.OneShot,
		incident.Summary, incident.Details).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert incident %s: %w", incident.Rule, err)
	}
	return id, nil
}

// ResolveIncident marks an incident as resolved.
func (s *Store) ResolveIncident(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update incidents set resolved_at = $2 where id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("resolve incident %d: %w", id, err)
	}
	return nil
}

// UpdateIncident replaces the severity, summary and details of an open
// incident.
func (s *Store) UpdateIncident(ctx context.Context, incident Incident) error {
	_, err := s.pool.Exec(ctx, `
		update incidents set severity = $2, summary = $3, details = $4 where id = $1`,
		incident.ID, incident.Severity, incident.Summary, incident.Details)
	if err != nil {
		return fmt.Errorf("update incident %d: %w", incident.ID, err)
	}
	return nil
}

// ReopenIncident starts a new episode of a resolved incident, keeping its ID.
// The ack belongs to the earlier episode, so it is cleared, and pending pages
// of the earlier episode are cancelled: the new episode pages again. It
// returns the new episode number.
func (s *Store) ReopenIncident(ctx context.Context, incident Incident) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reopen incident %d: %w", incident.ID, err)
	}
	defer tx.Rollback(ctx)
	var episode int64
	err = tx.QueryRow(ctx, `
		update incidents
		set resolved_at = null, acked_at = null, acked_by = null, opened_at = $2,
		    severity = $3, summary = $4, details = $5, episode = episode + 1
		where id = $1
		returning episode`,
		incident.ID, incident.OpenedAt, incident.Severity, incident.Summary, incident.Details).Scan(&episode)
	if err != nil {
		return 0, fmt.Errorf("reopen incident %d: %w", incident.ID, err)
	}
	if _, err := tx.Exec(ctx, `
		update pages set status = 'cancelled', reason = 'episode reopened', due_at = now(), updated_at = now()
		where incident_id = $1 and status in ('pending', 'resolving')`, incident.ID); err != nil {
		return 0, fmt.Errorf("cancel pages of incident %d: %w", incident.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reopen incident %d: %w", incident.ID, err)
	}
	return episode, nil
}

// RecentResolvedIncident returns the newest resolved incident of a rule and
// subject resolved at or after since. It returns nil when there is none.
func (s *Store) RecentResolvedIncident(ctx context.Context, rule, subject string, since time.Time) (*Incident, error) {
	var incident Incident
	err := scanIncident(s.pool.QueryRow(ctx, `select `+incidentColumns+`
		from incidents
		where rule = $1 and subject = $2 and resolved_at is not null and resolved_at >= $3
		order by resolved_at desc limit 1`, rule, subject, since), &incident)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recent incident: %w", err)
	}
	return &incident, nil
}

// Incidents returns the incidents that overlap a time range, newest first.
func (s *Store) Incidents(ctx context.Context, from, to time.Time) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `select `+incidentColumns+`
		from incidents
		where opened_at < $2 and (resolved_at is null or resolved_at > $1)
		order by opened_at desc`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read incidents: %w", err)
	}
	defer rows.Close()
	var incidents []Incident
	for rows.Next() {
		var incident Incident
		if err := scanIncident(rows, &incident); err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read incidents: %w", err)
	}
	return incidents, nil
}

// AckIncident records an ack on an open incident and reports whether this
// call set it. The first ack wins: an already acked or resolved incident is
// left alone and reports false, so the duration in the resolved footer is
// anchored to the first ack. Pending pages are cancelled in the same
// transaction, so an ack immediately stops every channel.
func (s *Store) AckIncident(ctx context.Context, id int64, at time.Time, by string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("ack incident %d: %w", id, err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		update incidents set acked_at = $2, acked_by = $3
		where id = $1 and acked_at is null and resolved_at is null`, id, at, by)
	if err != nil {
		return false, fmt.Errorf("ack incident %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		insert into deliveries (incident_id, page_id, kind, channel, recipient, created_at, status, reason)
		select p.incident_id, p.id, p.kind, p.channel, p.recipient, $2, 'acked', 'acked'
		from pages p
		where p.incident_id = $1 and p.status in ('pending', 'resolving')`, id, at); err != nil {
		return false, fmt.Errorf("record acked pages of incident %d: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `
		update pages set status = 'cancelled', reason = 'acked', due_at = now(), updated_at = now()
		where incident_id = $1 and status in ('pending', 'resolving')`, id); err != nil {
		return false, fmt.Errorf("cancel pages of incident %d: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("ack incident %d: %w", id, err)
	}
	return true, nil
}

// IncidentByID returns one incident by id, including its ack metadata.
// Returns (nil, nil) when no row matches.
func (s *Store) IncidentByID(ctx context.Context, id int64) (*Incident, error) {
	var incident Incident
	err := scanIncident(s.pool.QueryRow(ctx, `select `+incidentColumns+`
		from incidents where id = $1`, id), &incident)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read incident %d: %w", id, err)
	}
	return &incident, nil
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
