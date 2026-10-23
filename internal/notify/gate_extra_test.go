package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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
		store.Silence{Kind: store.SilenceMaintenanceWindow, Reason: "global",
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

func TestGateFlap(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	incident := store.Incident{
		ID: 1, Rule: "host.unit_failed", Subject: "storage-budget-check.service",
		Severity: store.SeverityWarning, OpenedAt: now.Add(-10 * time.Minute),
	}
	count := 0
	gate := &Gate{
		Store: newGateStore(), Logger: discardLogger(),
		FlapWindow: time.Hour, FlapMaxPages: 1,
		RecentOpenCount: func(context.Context, string, string, string, time.Time) (int, error) {
			return count, nil
		},
	}
	tests := []struct {
		name   string
		kind   store.DeliveryKind
		count  int
		status GateStatus
		drop   bool
	}{
		{name: "first open records", kind: store.DeliveryOpened, count: 0, status: GateRecorded},
		{name: "second open drops", kind: store.DeliveryOpened, count: 1, status: GateCooldown, drop: true},
		{name: "first resolution records", kind: store.DeliveryResolved, count: 1, status: GateRecorded},
		{name: "resolution after flapping drops", kind: store.DeliveryResolved, count: 2, status: GateCooldown, drop: true},
		{name: "escalation always records", kind: store.DeliveryEscalated, count: 5, status: GateRecorded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count = tt.count
			got := gate.Evaluate(context.Background(), DecisionInputs{
				Incident: incident, ChangeKind: tt.kind, Channel: KindTelegram, Now: now,
			})
			if got.Status != tt.status || got.Drop != tt.drop {
				t.Fatalf("decision = %+v, want status %q drop %v", got, tt.status, tt.drop)
			}
		})
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

// TestGateCooldownSparesEscalationsAndResolutions verifies that the cooldown
// only delays opening pages: an escalation or a resolution right after the
// opening page must go out.
func TestGateCooldownSparesEscalationsAndResolutions(t *testing.T) {
	now := time.Now()
	db := newGateStore()
	sentAt := now.Add(-time.Minute)
	db.incidents[1] = store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning, OpenedAt: sentAt}
	id := int64(1)
	db.deliveries = append(db.deliveries, store.Delivery{IncidentID: &id, Kind: store.DeliveryOpened, Status: "sent", SentAt: &sentAt})
	gate := &Gate{Store: db, Logger: discardLogger(), Cooldown: 30 * time.Minute}
	for kind, want := range map[store.DeliveryKind]GateStatus{
		store.DeliveryOpened:    GateCooldown,
		store.DeliveryEscalated: GateRecorded,
		store.DeliveryResolved:  GateRecorded,
	} {
		got := gate.Evaluate(context.Background(), DecisionInputs{
			Incident: db.incidents[1], ChangeKind: kind, Channel: KindTelegram, Now: now,
		})
		if got.Status != want {
			t.Errorf("%s: status = %q, want %q", kind, got.Status, want)
		}
	}
}

// TestQuietEndsAtAcrossDaylightSaving verifies the window closes at the To
// wall-clock time on the night the clocks change.
func TestQuietEndsAtAcrossDaylightSaving(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no time zone database: %v", err)
	}
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	gate := &Gate{QuietFrom: &from, QuietTo: &to, QuietZone: berlin}
	// The clocks go forward at 02:00 on 2026-03-29.
	now := time.Date(2026, 3, 28, 23, 30, 0, 0, berlin)
	got, inside := gate.quietEndsAt(now)
	want := time.Date(2026, 3, 29, 7, 0, 0, 0, berlin)
	if !inside || !got.Equal(want) {
		t.Fatalf("quietEndsAt = %v, %v; want %v", got, inside, want)
	}
}

// failingGateStore fails every lookup.
type failingGateStore struct{}

func (failingGateStore) FindActiveSilence(context.Context, string, string, *int64, time.Time) (*store.Silence, error) {
	return nil, errGateStore
}
func (failingGateStore) IncidentByID(context.Context, int64) (*store.Incident, error) {
	return nil, errGateStore
}
func (failingGateStore) LastSent(context.Context, string, string) (*store.Delivery, error) {
	return nil, errGateStore
}

var errGateStore = errors.New("database unavailable")

// TestGateFailsOpenAndLogs verifies that a failing store never blocks a page
// and that each failed lookup is logged.
func TestGateFailsOpenAndLogs(t *testing.T) {
	var logs bytes.Buffer
	gate := &Gate{
		Store:    &failingGateStore{},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		Cooldown: time.Hour,
	}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: time.Now(),
	})
	if got.Status != GateRecorded {
		t.Fatalf("status = %q, want recorded", got.Status)
	}
	for _, want := range []string{"read incident", "read silences", "read last page"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not mention %q:\n%s", want, logs.String())
		}
	}
}
