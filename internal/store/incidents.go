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
	Severity string
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
}

// OpenIncidents returns the incidents that are not resolved, keyed by rule and
// subject.
func (s *Store) OpenIncidents(ctx context.Context) (map[IncidentKey]Incident, error) {
	rows, err := s.pool.Query(ctx, `
		select id, rule, subject, app_id, slot_id, route_id, severity, opened_at, resolved_at, one_shot, summary, details
		from incidents where resolved_at is null`)
	if err != nil {
		return nil, fmt.Errorf("read open incidents: %w", err)
	}
	defer rows.Close()
	incidents := make(map[IncidentKey]Incident)
	for rows.Next() {
		var incident Incident
		if err := rows.Scan(&incident.ID, &incident.Rule, &incident.Subject, &incident.AppID, &incident.SlotID,
			&incident.RouteID, &incident.Severity, &incident.OpenedAt, &incident.ResolvedAt, &incident.OneShot,
			&incident.Summary, &incident.Details); err != nil {
			return nil, fmt.Errorf("scan open incident: %w", err)
		}
		incidents[IncidentKey{Rule: incident.Rule, Subject: incident.Subject}] = incident
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open incidents: %w", err)
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

// InsertIncident records a new incident and returns its ID.
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
func (s *Store) ReopenIncident(ctx context.Context, incident Incident) error {
	_, err := s.pool.Exec(ctx, `
		update incidents
		set resolved_at = null, opened_at = $2, severity = $3, summary = $4, details = $5
		where id = $1`, incident.ID, incident.OpenedAt, incident.Severity, incident.Summary, incident.Details)
	if err != nil {
		return fmt.Errorf("reopen incident %d: %w", incident.ID, err)
	}
	return nil
}

// RecentResolvedIncident returns the newest resolved incident of a rule and
// subject resolved at or after since. It returns nil when there is none.
func (s *Store) RecentResolvedIncident(ctx context.Context, rule, subject string, since time.Time) (*Incident, error) {
	var incident Incident
	err := s.pool.QueryRow(ctx, `
		select id, rule, subject, app_id, slot_id, route_id, severity, opened_at, resolved_at, one_shot, summary, details
		from incidents
		where rule = $1 and subject = $2 and resolved_at is not null and resolved_at >= $3
		order by resolved_at desc limit 1`, rule, subject, since).Scan(&incident.ID, &incident.Rule,
		&incident.Subject, &incident.AppID, &incident.SlotID, &incident.RouteID, &incident.Severity,
		&incident.OpenedAt, &incident.ResolvedAt, &incident.OneShot, &incident.Summary, &incident.Details)
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
	rows, err := s.pool.Query(ctx, `
		select id, rule, subject, app_id, slot_id, route_id, severity, opened_at, resolved_at, one_shot, summary, details
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
		if err := rows.Scan(&incident.ID, &incident.Rule, &incident.Subject, &incident.AppID, &incident.SlotID,
			&incident.RouteID, &incident.Severity, &incident.OpenedAt, &incident.ResolvedAt, &incident.OneShot,
			&incident.Summary, &incident.Details); err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read incidents: %w", err)
	}
	return incidents, nil
}
