package probe

import (
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

const (
	// failAfter is how many consecutive failed checks turn a healthy route
	// unhealthy or unreachable; one lost request is noise.
	failAfter = 3
	// recoverAfter is how many consecutive good checks make a failing route
	// healthy again.
	recoverAfter = 2
)

// Advance applies one check outcome to the previous verdict of a route.
//
// The verdict changes only after failAfter failures in a row or recoverAfter
// successes in a row, so incidents neither flap nor open on a single timeout.
// A route with too little evidence is unknown, never healthy by default.
func Advance(previous *store.ProbeState, routeID int64, target string, outcome Outcome, now time.Time) store.ProbeState {
	state := store.ProbeState{
		RouteID: routeID, State: store.ProbeUnknown, Since: now, LastResult: outcome.Result,
		LastChecked: now, Error: outcome.Error, Target: target,
	}
	if outcome.Status != 0 {
		status := outcome.Status
		latency := int(outcome.Latency / time.Millisecond)
		state.StatusCode, state.LatencyMS = &status, &latency
	}
	if previous != nil {
		state.State, state.Since = previous.State, previous.Since
		state.ConsecutiveFailures, state.ConsecutiveSuccesses = previous.ConsecutiveFailures, previous.ConsecutiveSuccesses
	}
	if outcome.Result == store.ProbeHealthy {
		state.ConsecutiveSuccesses++
		state.ConsecutiveFailures = 0
		// The very first healthy check is enough to leave unknown.
		if state.State == store.ProbeUnknown || state.ConsecutiveSuccesses >= recoverAfter {
			if state.State != store.ProbeHealthy {
				state.State, state.Since = store.ProbeHealthy, now
			}
		}
		return state
	}
	state.ConsecutiveFailures++
	state.ConsecutiveSuccesses = 0
	if state.ConsecutiveFailures >= failAfter {
		if state.State == store.ProbeHealthy || state.State == store.ProbeUnknown {
			state.Since = now
		}
		state.State = outcome.Result
	}
	return state
}

// Failing reports whether a verdict is a failure incidents should act on.
func Failing(state string) bool {
	return state == store.ProbeUnhealthy || state == store.ProbeUnreachable
}
