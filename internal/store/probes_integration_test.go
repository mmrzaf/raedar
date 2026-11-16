//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestProbeStateResultsAndRollup(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	routeID := insertRoute(t, st, "shop@docker")
	hour := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	latency := 40
	for i, result := range []string{ProbeHealthy, ProbeHealthy, ProbeUnreachable, ProbeUnhealthy} {
		r := ProbeMinute{RouteID: routeID, TS: hour.Add(time.Duration(i*20) * time.Second), Result: result}
		if result != ProbeUnreachable {
			r.LatencyMS = &latency
		}
		if err := st.AddProbeResult(ctx, r); err != nil {
			t.Fatalf("AddProbeResult() error = %v", err)
		}
	}
	if err := st.AddProbeResult(ctx, ProbeMinute{RouteID: routeID, TS: hour, Result: "bogus"}); err == nil {
		t.Error("AddProbeResult accepted an unknown result")
	}
	rng := Range{From: hour, To: hour.Add(time.Hour)}
	got, err := st.ProbeAvailability(ctx, []int64{routeID}, rng, SourceMinutes)
	if err != nil {
		t.Fatalf("ProbeAvailability() error = %v", err)
	}
	if got.Checks != 4 || got.Healthy != 2 || got.Unhealthy != 1 || got.Unreachable != 1 {
		t.Errorf("availability = %+v, want 4 checks: 2 healthy, 1 unhealthy, 1 unreachable", got)
	}
	if percent, known := got.Percent(); !known || percent != 50 {
		t.Errorf("percent = %v, %v; want 50", percent, known)
	}
	// Rolling up the hour gives the same totals from the hourly table.
	if err := st.Rollup(ctx, hour.Add(time.Hour+10*time.Minute), time.UTC); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	hourly, err := st.ProbeAvailability(ctx, []int64{routeID}, rng, SourceHours)
	if err != nil || hourly != got {
		t.Errorf("hourly availability = %+v, %v; want %+v", hourly, err, got)
	}
	if empty, _ := (ProbeAvailability{}).Percent(); empty != 0 {
		t.Error("no checks must not report availability")
	}

	state := ProbeState{RouteID: routeID, State: ProbeUnreachable, Since: hour, LastResult: ProbeUnreachable,
		LastChecked: hour, Error: "timeout", Target: "shop.example.com/", ConsecutiveFailures: 3}
	if err := st.SaveProbeState(ctx, state); err != nil {
		t.Fatalf("SaveProbeState() error = %v", err)
	}
	states, err := st.ProbeStates(ctx)
	if err != nil || states[routeID].State != ProbeUnreachable || states[routeID].ConsecutiveFailures != 3 {
		t.Errorf("ProbeStates() = %+v, %v", states, err)
	}
	if err := st.DeleteProbeStatesExcept(ctx, []int64{}); err != nil {
		t.Fatalf("DeleteProbeStatesExcept() error = %v", err)
	}
	if states, _ := st.ProbeStates(ctx); len(states) != 0 {
		t.Errorf("states after pruning = %v, want none", states)
	}
}
