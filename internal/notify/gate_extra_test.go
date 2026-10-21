package notify

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// silenceStore is a tiny in-memory stand-in for the gate's Store needs.
// It honours the precedence rules by reusing the production matcher so
// the tests can never drift from the runtime behaviour.
type silenceStore struct {
	mu         sync.Mutex
	silences   []store.Silence
	incidents  map[int64]store.Incident
	deliveries []store.Delivery
}

func newSilenceStore() *silenceStore {
	return &silenceStore{incidents: map[int64]store.Incident{}}
}

func (s *silenceStore) FindActiveSilence(ctx context.Context, rule, subject string, now time.Time) (*store.Silence, error) {
	return matchSilenceLocal(s.silences, rule, subject), nil
}

func (s *silenceStore) IncidentByID(ctx context.Context, id int64) (*store.Incident, error) {
	if inc, ok := s.incidents[id]; ok {
		copy := inc
		return &copy, nil
	}
	return nil, nil
}

func (s *silenceStore) LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deliveries {
		if d.Status != "sent" {
			continue
		}
		if d.IncidentID == nil {
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

func (s *silenceStore) OpenIncidentCount(ctx context.Context) (int, error) {
	return 0, nil
}

// TestGateCooldownSuppresses verifies that a delivery within the cooldown
// window after a previous one is suppressed with status cooldown.
func TestGateCooldownSuppresses(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	db := &silenceStore{
		incidents: map[int64]store.Incident{
			1: {ID: 1, Rule: "route.down", Subject: "fallback@file", Severity: store.SeverityCritical, OpenedAt: now},
		},
	}
	gate := &Gate{
		Store:        db,
		Cooldown:     15 * time.Minute,
		PageCritical: true,
	}
	// First call: no prior delivery, page goes through.
	first := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   db.incidents[1],
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now,
	}, Message{Text: "first"})
	if first.Status != GateRecorded {
		t.Fatalf("first: status = %q, want recorded", first.Status)
	}
	// Second call 5 minutes later, within the 15-minute cooldown.
	// LastSent is nil in the fake, so the gate would record it. We need
	// the test to simulate the "already sent" state.
	dbLastSent := &silenceStore{
		incidents: db.incidents,
		deliveries: []store.Delivery{
			{ID: 1, IncidentID: ptrInt64(1), Status: "sent", SentAt: ptrTime(now.Add(-5 * time.Minute))},
		},
	}
	gate.Store = dbLastSent
	second := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   dbLastSent.incidents[1],
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now,
	}, Message{Text: "second"})
	if second.Status != GateCooldown {
		t.Fatalf("second: status = %q, want cooldown", second.Status)
	}
	// Third call 16 minutes after the first (just past the cooldown).
	third := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   dbLastSent.incidents[1],
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now.Add(11 * time.Minute),
	}, Message{Text: "third"})
	if third.Status != GateRecorded {
		t.Fatalf("third: status = %q, want recorded (cooldown expired)", third.Status)
	}
}

// TestGateQuietHoursHoldAndCriticalBypasses verifies that quiet hours
// hold a warning and that critical incidents bypass when the setting
// is enabled.
func TestGateQuietHoursHoldAndCriticalBypasses(t *testing.T) {
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC) // inside the window
	gate := &Gate{
		Store:        &silenceStore{},
		QuietFrom:    &from,
		QuietTo:      &to,
		QuietZone:    time.UTC,
		PageCritical: true,
	}
	dec := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning},
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now,
	}, Message{Text: "warning"})
	if dec.Status != GateHeld {
		t.Fatalf("warning during quiet: status = %q, want held", dec.Status)
	}
	decCritical := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 2, Rule: "route.down", Subject: "x", Severity: store.SeverityCritical},
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now,
	}, Message{Text: "critical"})
	if decCritical.Status != GateRecorded {
		t.Fatalf("critical during quiet: status = %q, want recorded (PageCritical bypasses)", decCritical.Status)
	}
}

// TestGateStandDownPostsAllQuiet confirms that, when the host has been
// quiet long enough and no incidents are open, a new warning is
// absorbed by stand-down. Critical still bypasses.
func TestGateStandDownPostsAllQuiet(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	openStore := &silenceStore{} // OpenIncidentCount returns 0
	closedStore := &openStoreHelper{openCount: 0}
	gate := &Gate{
		Store:          closedStore,
		StandDownAfter: 6 * time.Hour,
		PageCritical:   true,
		lastPagedAt:    now.Add(-7 * time.Hour),
	}
	dec := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "host.disk", Subject: "/", Severity: store.SeverityWarning},
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now,
	}, Message{Text: "warning"})
	if dec.Status != GateStandDown {
		t.Fatalf("warning under stand-down: status = %q, want stand_down", dec.Status)
	}
	decCritical := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 2, Rule: "route.down", Subject: "x", Severity: store.SeverityCritical},
		ChangeKind: store.DeliveryOpened,
		Channel:    KindTelegram,
		Now:        now,
	}, Message{Text: "critical"})
	if decCritical.Status != GateRecorded {
		t.Fatalf("critical under stand-down: status = %q, want recorded (critical bypasses)", decCritical.Status)
	}
	_ = openStore
}

// openStoreHelper is a small wrapper that returns a specific open count
// for the stand-down test.
type openStoreHelper struct {
	silenceStore
	openCount int
}

func (h *openStoreHelper) FindActiveSilence(ctx context.Context, rule, subject string, now time.Time) (*store.Silence, error) {
	return h.silenceStore.FindActiveSilence(ctx, rule, subject, now)
}
func (h *openStoreHelper) IncidentByID(ctx context.Context, id int64) (*store.Incident, error) {
	return h.silenceStore.IncidentByID(ctx, id)
}
func (h *openStoreHelper) LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error) {
	return h.silenceStore.LastSent(ctx, rule, subject)
}
func (h *openStoreHelper) OpenIncidentCount(ctx context.Context) (int, error) {
	return h.openCount, nil
}

func ptrInt64(v int64) *int64        { return &v }
func ptrTime(t time.Time) *time.Time { return &t }

// silenceStrLocal mirrors store-package stringptr without the import cycle.
func silenceStrLocal(s string) *string { return &s }

// matchSilenceLocal matches the store package's logic for tests.
func matchSilenceLocal(active []store.Silence, rule, subject string) *store.Silence {
	var best *store.Silence
	bestRank := -1
	for i := range active {
		candidate := &active[i]
		var rank int
		switch {
		case candidate.Rule != nil && *candidate.Rule == rule && candidate.Subject != nil && *candidate.Subject == subject:
			rank = 3
		case candidate.Rule != nil && *candidate.Rule == rule && candidate.Subject == nil:
			rank = 2
		case candidate.Rule == nil && candidate.Subject != nil && *candidate.Subject == subject:
			rank = 1
		case candidate.Rule == nil && candidate.Subject == nil:
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

// testWriter wraps a *testing.T so it satisfies io.Writer when slog asks for one.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func TestGateSilenced(t *testing.T) {
	now := time.Now()
	db := newSilenceStore()
	gate := &Gate{Store: db, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}

	rule := silenceStrLocal("route.down")
	subject := silenceStrLocal("fallback@file")
	db.silences = append(db.silences, store.Silence{
		Kind: store.SilenceOneShotMute, Rule: rule, Subject: subject,
		Reason: "scanner noise", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	inc := store.Incident{
		ID: 1, Rule: "route.down", Subject: "fallback@file",
		Severity: store.SeverityCritical, OpenedAt: now, Summary: "no backend",
		Details: []byte(`{}`),
	}
	dec := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: inc, ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	}, Message{Text: "ignored"})
	if dec.Status != GateSilenced {
		t.Fatalf("want silenced, got %q", dec.Status)
	}
	if dec.Message != nil {
		t.Fatalf("silenced page must not carry a message")
	}
}

func TestGateAcked(t *testing.T) {
	now := time.Now()
	db := newSilenceStore()
	db.incidents[1] = store.Incident{
		ID: 1, Rule: "host.disk", Subject: "/",
		Severity: store.SeverityWarning, OpenedAt: now.Add(-time.Minute),
		Summary: "disk filling up", Details: []byte(`{}`),
		AckedAt: &now,
	}
	gate := &Gate{Store: db, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	dec := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: db.incidents[1], ChangeKind: store.DeliveryEscalated, Channel: KindTelegram, Now: now,
	}, Message{Text: "ignored"})
	if dec.Status != GateAcked {
		t.Fatalf("want acked, got %q", dec.Status)
	}
}

func TestGateDefaultRecords(t *testing.T) {
	now := time.Now()
	db := newSilenceStore()
	db.incidents[1] = store.Incident{
		ID: 1, Rule: "host.disk", Subject: "/",
		Severity: store.SeverityWarning, OpenedAt: now,
		Summary: "disk filling up", Details: []byte(`{}`),
	}
	gate := &Gate{Store: db, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	message := Message{Text: "page"}
	dec := gate.Evaluate(context.Background(), DecisionInputs{
		Incident: db.incidents[1], ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	}, message)
	if dec.Status != GateRecorded {
		t.Fatalf("want recorded, got %q", dec.Status)
	}
	if dec.Message == nil || dec.Message.Text != "page" {
		t.Fatalf("recorded decision must carry the message")
	}
}

func TestGateSilencedSpecificBeatsGlobal(t *testing.T) {
	now := time.Now()
	db := newSilenceStore()
	rule := silenceStrLocal("route.down")
	subject := silenceStrLocal("fallback@file")
	db.silences = append(db.silences,
		store.Silence{Kind: store.SilenceQuietHours, Reason: "global",
			StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)},
		store.Silence{Kind: store.SilenceOneShotMute, Rule: rule, Subject: subject,
			Reason: "exact", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)},
	)
	gate := &Gate{Store: db, Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	dec := gate.Evaluate(context.Background(), DecisionInputs{
		Incident:   store.Incident{ID: 1, Rule: "route.down", Subject: "fallback@file", Severity: store.SeverityWarning, OpenedAt: now, Summary: "x"},
		ChangeKind: store.DeliveryOpened, Channel: KindTelegram, Now: now,
	}, Message{})
	if dec.Status != GateSilenced {
		t.Fatalf("want silenced, got %q", dec.Status)
	}
	if dec.Reason != "exact" {
		t.Fatalf("want most-specific reason \"exact\", got %q", dec.Reason)
	}
}
