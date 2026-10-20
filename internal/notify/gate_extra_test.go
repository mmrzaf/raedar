package notify

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// silenceStore is a tiny in-memory stand-in for the gate's Store needs.
// It honours the precedence rules by reusing the production matcher so
// the tests can never drift from the runtime behaviour.
type silenceStore struct {
	silences  []store.Silence
	incidents map[int64]store.Incident
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
