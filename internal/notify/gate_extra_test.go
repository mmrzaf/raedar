package notify

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// fakeGateStore is an in-memory stand-in for the gate's store needs. It
// reuses the production silence matcher so the precedence tests can never
// drift from the runtime behaviour.
type fakeGateStore struct {
	mu         sync.Mutex
	silences   []store.Silence
	incidents  map[int64]store.Incident
	deliveries []store.Delivery
}

func newGateStore() *fakeGateStore {
	return &fakeGateStore{incidents: map[int64]store.Incident{}}
}

func (s *fakeGateStore) FindActiveSilence(ctx context.Context, rule, subject string, appID *int64, now time.Time) (*store.Silence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return matchSilenceLocal(s.silences, rule, subject, appID), nil
}

func (s *fakeGateStore) IncidentByID(ctx context.Context, id int64) (*store.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc, ok := s.incidents[id]; ok {
		copy := inc
		return &copy, nil
	}
	return nil, nil
}

func (s *fakeGateStore) LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deliveries {
		if d.Status != "sent" || d.IncidentID == nil {
			continue
		}
		inc, ok := s.incidents[*d.IncidentID]
		if !ok || inc.Rule != rule || inc.Subject != subject {
			continue
		}
		copy := d
		return &copy, nil
	}
	return nil, nil
}

func gateStr(s string) *string { return &s }

func TestGateAcked(t *testing.T) {
	now := time.Now()
	db := newGateStore()
	db.incidents[1] = store.Incident{
		ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning,
		OpenedAt: now.Add(-time.Minute), Summary: "disk", AckedAt: &now,
	}
	gate := &Gate{Store: db, Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: db.incidents[1], ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateAcked {
		t.Fatalf("status = %q, want acked", got.Status)
	}
}

func TestGateSilenced(t *testing.T) {
	now := time.Now()
	db := newGateStore()
	db.silences = append(db.silences, store.Silence{
		Kind: store.SilenceOneShotMute, Rule: gateStr("route.down"), Subject: gateStr("fallback@file"),
		Reason: "scanner noise", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	gate := &Gate{Store: db, Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "route.down", Subject: "fallback@file", Severity: store.SeverityCritical, OpenedAt: now},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateSilenced {
		t.Fatalf("status = %q, want silenced", got.Status)
	}
}

func TestGateSilenceSpecificBeatsGlobal(t *testing.T) {
	now := time.Now()
	db := newGateStore()
	db.silences = append(db.silences,
		store.Silence{Kind: store.SilenceQuietHours, Reason: "global",
			StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)},
		store.Silence{Kind: store.SilenceOneShotMute, Rule: gateStr("route.down"), Subject: gateStr("fallback@file"),
			Reason: "exact", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)},
	)
	gate := &Gate{Store: db, Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "route.down", Subject: "fallback@file", Severity: store.SeverityWarning, OpenedAt: now},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateSilenced || got.Reason != "exact" {
		t.Fatalf("got %+v, want silenced with reason exact", got)
	}
}

func TestGateAppScopedSilence(t *testing.T) {
	now := time.Now()
	db := newGateStore()
	appID := int64(7)
	db.silences = append(db.silences, store.Silence{
		Kind: store.SilenceAutoDeploy, AppID: &appID, Reason: "deploy",
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	gate := &Gate{Store: db, Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "container.crashed", Subject: "shop / shop #1", AppID: &appID, Severity: store.SeverityWarning, OpenedAt: now},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateSilenced {
		t.Fatalf("status = %q, want silenced by app-scoped silence", got.Status)
	}
}

func TestGateDefaultRecords(t *testing.T) {
	now := time.Now()
	gate := &Gate{Store: newGateStore(), Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning, OpenedAt: now},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateRecorded {
		t.Fatalf("status = %q, want recorded", got.Status)
	}
}

func TestGateQuietHoursHoldsButCriticalBypasses(t *testing.T) {
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	gate := &Gate{
		Store: newGateStore(), Logger: discardLogger(),
		QuietFrom: &from, QuietTo: &to, QuietZone: time.UTC, PageCritical: true,
	}
	warning := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if warning.Status != GateHeld {
		t.Fatalf("warning status = %q, want held", warning.Status)
	}
	// The window closes at 07:00 the next morning.
	wantReady := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	if !warning.ReadyAt.Equal(wantReady) {
		t.Fatalf("readyAt = %v, want %v", warning.ReadyAt, wantReady)
	}
	critical := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 2, Rule: "route.down", Subject: "x", Severity: store.SeverityCritical},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if critical.Status != GateRecorded {
		t.Fatalf("critical status = %q, want recorded", critical.Status)
	}
}

func TestGateQuietHoursOutsideWindowRecords(t *testing.T) {
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	gate := &Gate{
		Store: newGateStore(), Logger: discardLogger(),
		QuietFrom: &from, QuietTo: &to, QuietZone: time.UTC,
	}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateRecorded {
		t.Fatalf("status = %q, want recorded", got.Status)
	}
}

func TestGateCooldown(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := newGateStore()
	db.incidents[1] = store.Incident{
		ID: 1, Rule: "route.down", Subject: "fallback@file",
		Severity: store.SeverityCritical, OpenedAt: now.Add(-time.Hour),
	}
	sentAt := now.Add(-5 * time.Minute)
	db.deliveries = []store.Delivery{
		{ID: 1, IncidentID: ptrInt64(1), Status: "sent", SentAt: &sentAt},
	}
	gate := &Gate{Store: db, Logger: discardLogger(), Cooldown: 15 * time.Minute}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: db.incidents[1], ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateCooldown {
		t.Fatalf("status = %q, want cooldown", got.Status)
	}
	if want := sentAt.Add(15 * time.Minute); !got.ReadyAt.Equal(want) {
		t.Fatalf("readyAt = %v, want %v", got.ReadyAt, want)
	}

	// Past the cooldown the page records.
	later := now.Add(11 * time.Minute)
	got = gate.Evaluate(context.Background(), DecisionInputs{
		Incident: db.incidents[1], ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: later,
	})
	if got.Status != GateRecorded {
		t.Fatalf("after cooldown: status = %q, want recorded", got.Status)
	}
}

func TestGateCooldownByRule(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := newGateStore()
	db.incidents[1] = store.Incident{
		ID: 1, Rule: "route.down", Subject: "fallback@file",
		Severity: store.SeverityCritical, OpenedAt: now.Add(-time.Hour),
	}
	sentAt := now.Add(-20 * time.Minute)
	db.deliveries = []store.Delivery{
		{ID: 1, IncidentID: ptrInt64(1), Status: "sent", SentAt: &sentAt},
	}
	gate := &Gate{
		Store: db, Logger: discardLogger(),
		Cooldown:       30 * time.Minute,
		CooldownByRule: map[string]time.Duration{"route.down": 15 * time.Minute},
	}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: db.incidents[1], ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Status != GateRecorded {
		t.Fatalf("status = %q, want recorded (15m override expired)", got.Status)
	}
}

func matchSilenceLocal(active []store.Silence, rule, subject string, appID *int64) *store.Silence {
	var best *store.Silence
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

func ptrInt64(v int64) *int64 { return &v }

var _ = slog.LevelInfo
