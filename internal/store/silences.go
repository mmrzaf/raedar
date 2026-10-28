package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxSilenceDuration bounds a silence created by an operator so a typo cannot
// mute a subject for a year.
const MaxSilenceDuration = 30 * 24 * time.Hour

// SilenceKind is the kind of a silence row.
type SilenceKind string

// The silence kinds, matching the silences.kind check constraint.
const (
	SilenceMaintenanceWindow SilenceKind = "maintenance_window"
	SilenceOneShotMute       SilenceKind = "one_shot_mute"
	SilenceAutoDeploy        SilenceKind = "auto_deploy"
)

// Silence is one row of the silences table.
type Silence struct {
	ID        int64
	Kind      SilenceKind
	Rule      *string
	Subject   *string
	AppID     *int64
	Reason    string
	StartsAt  time.Time
	EndsAt    time.Time
	CreatedAt time.Time
	CreatedBy *string
	DedupKey  *string
	Comment   *string
}

// CreateSilence inserts one silence. A non-null DedupKey makes the insert
// idempotent per (kind, dedup_key): a duplicate returns the existing row's
// id without error.
func (s *Store) CreateSilence(ctx context.Context, silence Silence) (int64, error) {
	if silence.DedupKey == nil {
		var id int64
		err := s.pool.QueryRow(ctx, `
			insert into silences (kind, rule, subject, app_id, reason, starts_at, ends_at, created_by, dedup_key, comment)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			returning id`,
			string(silence.Kind), silence.Rule, silence.Subject, silence.AppID, silence.Reason,
			silence.StartsAt, silence.EndsAt, silence.CreatedBy, silence.DedupKey, silence.Comment,
		).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("insert silence: %w", err)
		}
		return id, nil
	}
	// PG requires an explicit inference predicate matching the partial unique
	// index's WHERE clause for ON CONFLICT (cols) to find it.
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into silences (kind, rule, subject, app_id, reason, starts_at, ends_at, created_by, dedup_key, comment)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		on conflict (kind, dedup_key) where dedup_key is not null do update set
			rule = excluded.rule, subject = excluded.subject, app_id = excluded.app_id,
			reason = excluded.reason, starts_at = excluded.starts_at, ends_at = excluded.ends_at,
			created_by = excluded.created_by, comment = excluded.comment
		returning id`,
		string(silence.Kind), silence.Rule, silence.Subject, silence.AppID, silence.Reason,
		silence.StartsAt, silence.EndsAt, silence.CreatedBy, silence.DedupKey, silence.Comment,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert silence: %w", err)
	}
	return id, nil
}

// DeleteSilence removes one silence by id and returns whether anything was
// removed. Pages waiting on that silence are woken in the same transaction,
// so removing a mute releases the incident on the next dispatcher tick.
func (s *Store) DeleteSilence(ctx context.Context, id int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("delete silence %d: %w", id, err)
	}
	defer tx.Rollback(ctx)
	// Wake before deleting: the foreign key would otherwise null the
	// silence_id first and the update would match nothing.
	if _, err := tx.Exec(ctx, `
		update pages set due_at = now(), reason = 'silence removed', updated_at = now()
		where silence_id = $1 and status in ('pending', 'resolving')`, id); err != nil {
		return false, fmt.Errorf("wake pages of silence %d: %w", id, err)
	}
	tag, err := tx.Exec(ctx, `delete from silences where id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete silence %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("delete silence %d: %w", id, err)
	}
	return true, nil
}

// ActiveSilences returns every silence in force at now, oldest end first
// so the caller can rely on a stable order for equally specific matches.
func (s *Store) ActiveSilences(ctx context.Context, now time.Time) ([]Silence, error) {
	rows, err := s.pool.Query(ctx, silenceSelect+`
		where starts_at <= $1 and ends_at > $1
		order by ends_at asc`, now)
	if err != nil {
		return nil, fmt.Errorf("read silences: %w", err)
	}
	return scanSilences(rows)
}

// MatchSilence finds the most specific silence for rule+subject, where an
// app-scoped silence matches when appID matches. A silence with rule=nil
// matches any rule; subject=nil matches any subject; app=nil matches any app.
// Precedence:
//
//	(rule, subject) > (rule) > (subject) > (app) > (wildcard)
//
// Ties on rank prefer the silence ending later.
func MatchSilence(active []Silence, rule, subject string, appID *int64) *Silence {
	var best *Silence
	bestRank := -1
	for i := range active {
		candidate := &active[i]
		var rank int
		switch {
		case candidate.Rule != nil && *candidate.Rule == rule && candidate.Subject != nil && *candidate.Subject == subject:
			rank = 4
		case candidate.Rule != nil && *candidate.Rule == rule && candidate.Subject == nil && candidate.AppID == nil:
			rank = 3
		case candidate.Rule == nil && candidate.Subject != nil && *candidate.Subject == subject && candidate.AppID == nil:
			rank = 2
		case candidate.Rule == nil && candidate.Subject == nil && candidate.AppID != nil && appID != nil && *candidate.AppID == *appID:
			rank = 1
		case candidate.Rule == nil && candidate.Subject == nil && candidate.AppID == nil:
			rank = 0
		default:
			continue
		}
		if rank > bestRank {
			bestRank = rank
			best = candidate
			continue
		}
		if rank == bestRank && best != nil && candidate.EndsAt.After(best.EndsAt) {
			best = candidate
		}
	}
	return best
}

// PruneSilences deletes the generated silence rows that no longer have any
// effect: auto-deploy rows after a day and one-shot mutes after the operator
// cap. Maintenance windows are kept as operator history.
func (s *Store) PruneSilences(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		delete from silences
		where (kind = 'auto_deploy' and ends_at < $1)
		   or (kind = 'one_shot_mute' and ends_at < $2)`,
		now.Add(-24*time.Hour), now.Add(-MaxSilenceDuration))
	if err != nil {
		return 0, fmt.Errorf("prune silences: %w", err)
	}
	return tag.RowsAffected(), nil
}

// silenceSelect is the SELECT fragment shared by silence queries.
const silenceSelect = `
select id, kind, rule, subject, app_id, reason, starts_at, ends_at,
       created_at, created_by, dedup_key, comment
from silences`

// scanSilences reads many silences from rows.
func scanSilences(rows pgx.Rows) ([]Silence, error) {
	defer rows.Close()
	var silences []Silence
	for rows.Next() {
		var silence Silence
		if err := rows.Scan(&silence.ID, &silence.Kind, &silence.Rule, &silence.Subject,
			&silence.AppID, &silence.Reason, &silence.StartsAt, &silence.EndsAt,
			&silence.CreatedAt, &silence.CreatedBy, &silence.DedupKey, &silence.Comment); err != nil {
			return nil, fmt.Errorf("scan silence: %w", err)
		}
		silences = append(silences, silence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read silences: %w", err)
	}
	return silences, nil
}

// ListSilences returns silences ordered by their start, newest first.
// Silences in force at now come first so the operator sees what is active.
func (s *Store) ListSilences(ctx context.Context, now time.Time, limit int) ([]Silence, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, silenceSelect+`
		order by (ends_at > $1) desc, starts_at desc
		limit $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("read silences: %w", err)
	}
	return scanSilences(rows)
}
