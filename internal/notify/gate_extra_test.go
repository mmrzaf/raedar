package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// fakeGateStore is an in-memory stand-in for the gate's store needs.
type fakeGateStore struct {
	mu            sync.Mutex
	silences      []store.Silence
	announcements map[string]announcement
	err           error
}

type announcement struct {
	count int
	last  time.Time
}

func newGateStore() *fakeGateStore {
	return &fakeGateStore{announcements: map[string]announcement{}}
}

func (s *fakeGateStore) ActiveSilences(context.Context, time.Time) ([]store.Silence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return append([]store.Silence(nil), s.silences...), nil
}

func (s *fakeGateStore) Announcements(_ context.Context, rule, subject, channel string, since time.Time) (int, *time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, nil, s.err
	}
	entry, ok := s.announcements[rule+"\x00"+subject+"\x00"+channel]
	if !ok || entry.last.IsZero() || entry.last.Before(since) {
		return 0, nil, nil
	}
	last := entry.last
	return entry.count, &last, nil
}

func (s *fakeGateStore) setAnnouncements(rule, subject, channel string, count int, last time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.announcements[rule+"\x00"+subject+"\x00"+channel] = announcement{count: count, last: last}
}

func gateStr(s string) *string { return &s }

func gateIncident(severity store.Severity) store.Incident {
	return store.Incident{
		ID: 1, Episode: 1, Rule: "route.down", Subject: "fallback@file",
		Severity: severity, OpenedAt: time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC),
	}
}

func TestGateAcked(t *testing.T) {
	now := time.Now()
	incident := gateIncident(store.SeverityCritical)
	incident.AckedAt = &now
	gate := &Gate{Store: newGateStore(), Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: incident, Kind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionDrop || got.Reason != "acked" {
		t.Fatalf("decision = %+v, want drop acked", got)
	}
}

func TestGateManualSilenceWaits(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ends := now.Add(time.Hour)
	db := newGateStore()
	db.silences = append(db.silences, store.Silence{
		ID: 16, Kind: store.SilenceOneShotMute, Rule: gateStr("route.down"), Subject: gateStr("fallback@file"),
		Reason: "scanner noise", StartsAt: now.Add(-time.Minute), EndsAt: ends,
	})
	gate := &Gate{Store: db, Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionWait || !got.ReadyAt.Equal(ends) || got.SilenceID == nil || *got.SilenceID != 16 {
		t.Fatalf("decision = %+v, want wait until %v on silence 16", got, ends)
	}
}

func TestGateAutoDeploySilenceSparesCritical(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	appID := int64(7)
	db := newGateStore()
	db.silences = append(db.silences, store.Silence{
		Kind: store.SilenceAutoDeploy, AppID: &appID, Reason: "deploy",
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	gate := &Gate{Store: db, Logger: discardLogger()}
	incident := gateIncident(store.SeverityWarning)
	incident.AppID = &appID
	incident.Rule = "container.crashed"
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: incident, Kind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionWait {
		t.Fatalf("warning decision = %+v, want wait", got)
	}
	incident.Severity = store.SeverityCritical
	got = gate.Evaluate(context.Background(), DecisionInputs{
		Incident: incident, Kind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("critical decision = %+v, want send (auto-deploy must not hide a critical)", got)
	}
}

func TestGateAutoDeployDoesNotSuppressResolution(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	appID := int64(7)
	db := newGateStore()
	db.silences = append(db.silences, store.Silence{
		Kind: store.SilenceAutoDeploy, AppID: &appID, Reason: "deploy",
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	gate := &Gate{Store: db, Logger: discardLogger()}
	incident := gateIncident(store.SeverityWarning)
	incident.AppID = &appID
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: incident, Kind: store.DeliveryResolved, Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("decision = %+v, want send", got)
	}
}

func TestGateSilenceSpecificBeatsGlobal(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := newGateStore()
	db.silences = append(db.silences,
		store.Silence{Kind: store.SilenceMaintenanceWindow, Reason: "global",
			StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)},
		store.Silence{Kind: store.SilenceOneShotMute, Rule: gateStr("route.down"), Subject: gateStr("fallback@file"),
			Reason: "exact", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(2 * time.Hour)},
	)
	gate := &Gate{Store: db, Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionWait || !strings.Contains(got.Reason, "exact") {
		t.Fatalf("decision = %+v, want the exact silence", got)
	}
}

func TestGateCriticalSilenceFallsThroughToBroaderManual(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	appID := int64(7)
	db := newGateStore()
	db.silences = append(db.silences,
		store.Silence{Kind: store.SilenceAutoDeploy, AppID: &appID, Reason: "deploy",
			StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)},
		store.Silence{Kind: store.SilenceMaintenanceWindow, Rule: gateStr("route.down"), Reason: "maintenance",
			StartsAt: now.Add(-time.Minute), EndsAt: now.Add(2 * time.Hour)},
	)
	gate := &Gate{Store: db, Logger: discardLogger()}
	incident := gateIncident(store.SeverityCritical)
	incident.AppID = &appID
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: incident, Kind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionWait || !strings.Contains(got.Reason, "maintenance") {
		t.Fatalf("decision = %+v, want the manual silence, not the skipped auto-deploy one", got)
	}
}

func TestGateDefaultSends(t *testing.T) {
	now := time.Now()
	gate := &Gate{Store: newGateStore(), Logger: discardLogger()}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("decision = %+v, want send", got)
	}
}

func TestGateQuietHours(t *testing.T) {
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	gate := &Gate{
		Store: newGateStore(), Logger: discardLogger(),
		QuietFrom: &from, QuietTo: &to, QuietZone: time.UTC, PageCritical: true,
	}
	warning := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if warning.Action != ActionWait {
		t.Fatalf("warning decision = %+v, want wait", warning)
	}
	wantReady := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	if !warning.ReadyAt.Equal(wantReady) {
		t.Fatalf("readyAt = %v, want %v", warning.ReadyAt, wantReady)
	}
	critical := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if critical.Action != ActionSend {
		t.Fatalf("critical decision = %+v, want send", critical)
	}
}

func TestGateQuietHoursHoldsCriticalWhenDisabled(t *testing.T) {
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	gate := &Gate{
		Store: newGateStore(), Logger: discardLogger(),
		QuietFrom: &from, QuietTo: &to, QuietZone: time.UTC, PageCritical: false,
	}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionWait {
		t.Fatalf("decision = %+v, want wait", got)
	}
}

func TestGateResolutionsAreBestEffort(t *testing.T) {
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	gate := &Gate{
		Store: newGateStore(), Logger: discardLogger(),
		QuietFrom: &from, QuietTo: &to, QuietZone: time.UTC, PageCritical: true,
	}
	warning := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryResolved,
		Channel: KindTelegram, Now: now,
	})
	if warning.Action != ActionDrop {
		t.Fatalf("warning resolution = %+v, want drop", warning)
	}
	critical := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryResolved,
		Channel: KindTelegram, Now: now,
	})
	if critical.Action != ActionSend {
		t.Fatalf("critical resolution = %+v, want send", critical)
	}
}

func TestGateCooldown(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := newGateStore()
	db.setAnnouncements("route.down", "fallback@file", "telegram", 1, now.Add(-5*time.Minute))
	gate := &Gate{Store: db, Logger: discardLogger(), Cooldown: 15 * time.Minute}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionWait || !got.ReadyAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("decision = %+v, want wait until the cooldown ends", got)
	}
	// Past the cooldown the page sends.
	later := now.Add(11 * time.Minute)
	got = gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: later,
	})
	if got.Action != ActionSend {
		t.Fatalf("after cooldown: decision = %+v, want send", got)
	}
}

func TestGateCooldownByRule(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := newGateStore()
	db.setAnnouncements("route.down", "fallback@file", "telegram", 1, now.Add(-20*time.Minute))
	gate := &Gate{
		Store: db, Logger: discardLogger(),
		Cooldown:       30 * time.Minute,
		CooldownByRule: map[string]time.Duration{"route.down": 15 * time.Minute},
	}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("decision = %+v, want send (15m override expired)", got)
	}
}

func TestGateCooldownSparesEscalationsAndResolutions(t *testing.T) {
	now := time.Now()
	db := newGateStore()
	db.setAnnouncements("route.down", "fallback@file", "telegram", 1, now.Add(-time.Minute))
	gate := &Gate{Store: db, Logger: discardLogger(), Cooldown: 30 * time.Minute}
	for kind, want := range map[store.DeliveryKind]Action{
		store.DeliveryOpened:    ActionWait,
		store.DeliveryEscalated: ActionSend,
		store.DeliveryResolved:  ActionSend,
	} {
		got := gate.Evaluate(context.Background(), DecisionInputs{
			Incident: gateIncident(store.SeverityWarning), Kind: kind,
			Channel: KindTelegram, Now: now,
		})
		if got.Action != want {
			t.Errorf("%s: action = %q, want %q", kind, got.Action, want)
		}
	}
}

func TestGateFlapCountsAnnouncedPagesOnly(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := newGateStore()
	gate := &Gate{
		Store: db, Logger: discardLogger(),
		FlapWindow: time.Hour, FlapMaxPages: 1,
	}
	// No announced page yet: the first page is allowed.
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("first open = %+v, want send", got)
	}
	// One announced episode uses the budget.
	db.setAnnouncements("route.down", "fallback@file", "telegram", 1, now.Add(-time.Minute))
	got = gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionDrop || got.Reason != "flapping" {
		t.Fatalf("second open = %+v, want drop flapping", got)
	}
	// A critical episode bypasses the budget.
	got = gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityCritical), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("critical open = %+v, want send", got)
	}
	// Resolutions are not flap pages.
	got = gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryResolved,
		Channel: KindTelegram, Now: now,
	})
	if got.Action != ActionSend {
		t.Fatalf("resolution = %+v, want send", got)
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
	now := time.Date(2026, 3, 28, 23, 30, 0, 0, berlin)
	got, inside := gate.quietEndsAt(now)
	want := time.Date(2026, 3, 29, 7, 0, 0, 0, berlin)
	if !inside || !got.Equal(want) {
		t.Fatalf("quietEndsAt = %v, %v; want %v", got, inside, want)
	}
}

// failingGateStore fails every lookup.
type failingGateStore struct{}

func (failingGateStore) ActiveSilences(context.Context, time.Time) ([]store.Silence, error) {
	return nil, errGateStore
}
func (failingGateStore) Announcements(context.Context, string, string, string, time.Time) (int, *time.Time, error) {
	return 0, nil, errGateStore
}

var errGateStore = errors.New("database unavailable")

// TestGateFailsOpenAndLogs verifies that a failing store never blocks a page
// and that each failed lookup is logged.
func TestGateFailsOpenAndLogs(t *testing.T) {
	var logs strings.Builder
	gate := &Gate{
		Store:    failingGateStore{},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		Cooldown: time.Hour,
	}
	got := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: gateIncident(store.SeverityWarning), Kind: store.DeliveryOpened,
		Channel: KindTelegram, Now: time.Now(),
	})
	if got.Action != ActionSend {
		t.Fatalf("decision = %+v, want send", got)
	}
	for _, want := range []string{"read silences", "read last announcement"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not mention %q:\n%s", want, logs.String())
		}
	}
}
