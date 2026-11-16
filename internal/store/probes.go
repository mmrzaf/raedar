package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The verdicts of an active check.
const (
	// ProbeHealthy means the route answered as expected.
	ProbeHealthy = "healthy"
	// ProbeUnhealthy means the route answered, but not as expected.
	ProbeUnhealthy = "unhealthy"
	// ProbeUnreachable means no answer came: connection, TLS or timeout error.
	ProbeUnreachable = "unreachable"
	// ProbeUnknown means there is no usable verdict yet.
	ProbeUnknown = "unknown"
)

// ProbeState is the current verdict of the active checks of one route.
type ProbeState struct {
	RouteID int64
	// State is the verdict incidents act on: it changes only after several
	// consecutive checks agree.
	State string
	// Since is when State last changed.
	Since time.Time
	// LastResult is the outcome of the newest single check.
	LastResult  string
	LastChecked time.Time
	// StatusCode is the HTTP status of the newest check; nil without a response.
	StatusCode *int
	// LatencyMS is the duration of the newest check; nil when it failed to connect.
	LatencyMS *int
	// Error describes the newest failure; empty for a healthy check.
	Error string
	// Target is what was requested, for display: host and path.
	Target string
	// ConsecutiveFailures and ConsecutiveSuccesses count the newest run of
	// checks with the same side of the verdict.
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
}

// SaveProbeState stores the verdict of a route.
func (s *Store) SaveProbeState(ctx context.Context, state ProbeState) error {
	_, err := s.pool.Exec(ctx, `
		insert into probe_state (route_id, state, since, last_result, last_checked, status_code, latency_ms, error, target,
		                         consecutive_failures, consecutive_successes)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		on conflict (route_id) do update set
			state = excluded.state, since = excluded.since, last_result = excluded.last_result,
			last_checked = excluded.last_checked, status_code = excluded.status_code,
			latency_ms = excluded.latency_ms, error = excluded.error, target = excluded.target,
			consecutive_failures = excluded.consecutive_failures,
			consecutive_successes = excluded.consecutive_successes`,
		state.RouteID, state.State, state.Since, state.LastResult, state.LastChecked, state.StatusCode,
		state.LatencyMS, state.Error, state.Target, state.ConsecutiveFailures, state.ConsecutiveSuccesses)
	if err != nil {
		return fmt.Errorf("save probe state: %w", err)
	}
	return nil
}

// ProbeStates returns the verdicts of all routes by route ID.
func (s *Store) ProbeStates(ctx context.Context) (map[int64]ProbeState, error) {
	rows, err := s.pool.Query(ctx, `
		select route_id, state, since, last_result, last_checked, status_code, latency_ms, error, target,
		       consecutive_failures, consecutive_successes
		from probe_state`)
	if err != nil {
		return nil, fmt.Errorf("read probe states: %w", err)
	}
	defer rows.Close()
	states := make(map[int64]ProbeState)
	for rows.Next() {
		var state ProbeState
		if err := rows.Scan(&state.RouteID, &state.State, &state.Since, &state.LastResult, &state.LastChecked,
			&state.StatusCode, &state.LatencyMS, &state.Error, &state.Target,
			&state.ConsecutiveFailures, &state.ConsecutiveSuccesses); err != nil {
			return nil, fmt.Errorf("scan probe state: %w", err)
		}
		states[state.RouteID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read probe states: %w", err)
	}
	return states, nil
}

// DeleteProbeStatesExcept removes the verdicts of routes that are no longer
// checked, so a route that stopped being eligible does not keep a stale one.
func (s *Store) DeleteProbeStatesExcept(ctx context.Context, keep []int64) error {
	if _, err := s.pool.Exec(ctx, `delete from probe_state where not (route_id = any($1))`, keep); err != nil {
		return fmt.Errorf("delete probe states: %w", err)
	}
	return nil
}

// ProbeMinute is the outcome of one check, counted into its minute.
type ProbeMinute struct {
	RouteID int64
	// TS is the time of the check; it is truncated to the minute.
	TS time.Time
	// Result is healthy, unhealthy or unreachable.
	Result string
	// LatencyMS is the duration of the check; nil when none was measured.
	LatencyMS *int
}

// AddProbeResult counts one check into its minute row.
func (s *Store) AddProbeResult(ctx context.Context, result ProbeMinute) error {
	var healthy, unhealthy, unreachable int
	switch result.Result {
	case ProbeHealthy:
		healthy = 1
	case ProbeUnhealthy:
		unhealthy = 1
	case ProbeUnreachable:
		unreachable = 1
	default:
		return fmt.Errorf("add probe result: unknown result %q", result.Result)
	}
	_, err := s.pool.Exec(ctx, `
		insert into probe_minute (route_id, ts, checks, healthy, unhealthy, unreachable, latency_ms_max)
		values ($1, $2, 1, $3, $4, $5, $6)
		on conflict (route_id, ts) do update set
			checks = probe_minute.checks + 1,
			healthy = probe_minute.healthy + excluded.healthy,
			unhealthy = probe_minute.unhealthy + excluded.unhealthy,
			unreachable = probe_minute.unreachable + excluded.unreachable,
			latency_ms_max = greatest(probe_minute.latency_ms_max, excluded.latency_ms_max)`,
		result.RouteID, result.TS.Truncate(time.Minute), healthy, unhealthy, unreachable, result.LatencyMS)
	if err != nil {
		return fmt.Errorf("add probe result: %w", err)
	}
	return nil
}

// ProbeAvailability is the outcome of the checks of some routes over a range.
type ProbeAvailability struct {
	Checks      int64
	Healthy     int64
	Unhealthy   int64
	Unreachable int64
}

// Percent returns the share of healthy checks; known is false without checks.
func (a ProbeAvailability) Percent() (percent float64, known bool) {
	if a.Checks == 0 {
		return 0, false
	}
	return 100 * float64(a.Healthy) / float64(a.Checks), true
}

// ProbeAvailability sums the checks of routes in a range from the minute or
// hourly table, depending on the source the range resolves to.
func (s *Store) ProbeAvailability(ctx context.Context, routeIDs []int64, r Range, source RangeSource) (ProbeAvailability, error) {
	var total ProbeAvailability
	if len(routeIDs) == 0 {
		return total, nil
	}
	table := "probe_minute"
	if source == SourceHours {
		table = "probe_hour"
	}
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
		select coalesce(sum(checks), 0), coalesce(sum(healthy), 0), coalesce(sum(unhealthy), 0), coalesce(sum(unreachable), 0)
		from %s where route_id = any($1) and ts >= $2 and ts < $3`, table), routeIDs, r.From, r.To).
		Scan(&total.Checks, &total.Healthy, &total.Unhealthy, &total.Unreachable)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return total, fmt.Errorf("read probe availability: %w", err)
	}
	return total, nil
}
