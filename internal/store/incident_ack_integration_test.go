//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// newMigratedStore returns a store whose schema is fully migrated.
func newMigratedStore(t *testing.T) *Store {
	t.Helper()
	st := newStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func newTestIncident(t *testing.T, st *Store, rule string) Incident {
	t.Helper()
	incident := Incident{
		Rule: rule, Subject: "fallback@file", Severity: SeverityWarning,
		OpenedAt: time.Now().Add(-time.Hour), Summary: "down", Details: []byte(`{}`),
	}
	id, err := st.InsertIncident(context.Background(), incident)
	if err != nil {
		t.Fatalf("InsertIncident: %v", err)
	}
	incident.ID = id
	return incident
}

// TestAckIncidentFirstAckWins verifies that only the first ack of an open
// incident takes effect and that a resolved incident cannot be acked.
func TestAckIncidentFirstAckWins(t *testing.T) {
	st := newMigratedStore(t)
	ctx := context.Background()
	incident := newTestIncident(t, st, "route.down")

	first := time.Now().Truncate(time.Microsecond)
	if acked, err := st.AckIncident(ctx, incident.ID, first, "telegram:1"); err != nil || !acked {
		t.Fatalf("first ack = %v, %v; want true", acked, err)
	}
	if acked, err := st.AckIncident(ctx, incident.ID, first.Add(time.Minute), "telegram:2"); err != nil || acked {
		t.Fatalf("second ack = %v, %v; want false", acked, err)
	}
	got, err := st.IncidentByID(ctx, incident.ID)
	if err != nil || got == nil || got.AckedBy == nil || *got.AckedBy != "telegram:1" || !got.AckedAt.Equal(first) {
		t.Fatalf("incident after acks = %+v, %v; want the first ack kept", got, err)
	}

	resolved := newTestIncident(t, st, "host.disk")
	if err := st.ResolveIncident(ctx, resolved.ID, time.Now()); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if acked, err := st.AckIncident(ctx, resolved.ID, time.Now(), "telegram:1"); err != nil || acked {
		t.Fatalf("ack of a resolved incident = %v, %v; want false", acked, err)
	}
}

// TestReopenIncidentClearsAck verifies that a new episode pages again: the ack
// belongs to the episode it was given for.
func TestReopenIncidentClearsAck(t *testing.T) {
	st := newMigratedStore(t)
	ctx := context.Background()
	incident := newTestIncident(t, st, "route.down")
	if _, err := st.AckIncident(ctx, incident.ID, time.Now(), "telegram:1"); err != nil {
		t.Fatalf("AckIncident: %v", err)
	}
	if err := st.ResolveIncident(ctx, incident.ID, time.Now()); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	incident.OpenedAt = time.Now()
	if err := st.ReopenIncident(ctx, incident); err != nil {
		t.Fatalf("ReopenIncident: %v", err)
	}
	got, err := st.IncidentByID(ctx, incident.ID)
	if err != nil || got == nil {
		t.Fatalf("IncidentByID = %v, %v", got, err)
	}
	if got.Acked() || got.AckedBy != nil {
		t.Fatalf("reopened incident is still acked: %+v", got)
	}
}

// TestTelegramMessageDeliveriesKeyedByChat verifies that message ids are
// resolved per chat, because Telegram only makes them unique within a chat.
func TestTelegramMessageDeliveriesKeyedByChat(t *testing.T) {
	st := newMigratedStore(t)
	ctx := context.Background()
	incident := newTestIncident(t, st, "route.down")
	chatA, chatB, message := int64(10), int64(20), int64(500)
	for _, chat := range []int64{chatA, chatB} {
		if _, err := st.AddDelivery(ctx, Delivery{
			IncidentID: &incident.ID, Kind: DeliveryOpened, Channel: "telegram", CreatedAt: time.Now(),
			Status: "sent", TelegramChatID: &chat, TelegramMessageID: &message,
		}); err != nil {
			t.Fatalf("AddDelivery: %v", err)
		}
	}
	got, err := st.TelegramMessageDeliveries(ctx, chatB, message)
	if err != nil || len(got) != 1 || *got[0].TelegramChatID != chatB {
		t.Fatalf("deliveries of chat B = %+v, %v; want the one in chat B", got, err)
	}
	if got, err := st.TelegramMessageDeliveries(ctx, 30, message); err != nil || len(got) != 0 {
		t.Fatalf("deliveries of an unknown chat = %+v, %v; want none", got, err)
	}
}

// TestLastSentCountsOnlyPages verifies that resolutions and tests do not start
// the pager cooldown.
func TestLastSentCountsOnlyPages(t *testing.T) {
	st := newMigratedStore(t)
	ctx := context.Background()
	incident := newTestIncident(t, st, "route.down")
	sent := time.Now()
	for _, kind := range []DeliveryKind{DeliveryResolved, DeliveryTest} {
		if _, err := st.AddDelivery(ctx, Delivery{
			IncidentID: &incident.ID, Kind: kind, Channel: "telegram", CreatedAt: sent, SentAt: &sent, Status: "sent",
		}); err != nil {
			t.Fatalf("AddDelivery: %v", err)
		}
	}
	if got, err := st.LastSent(ctx, incident.Rule, incident.Subject); err != nil || got != nil {
		t.Fatalf("LastSent = %+v, %v; want none for a resolution", got, err)
	}
	if _, err := st.AddDelivery(ctx, Delivery{
		IncidentID: &incident.ID, Kind: DeliveryEscalated, Channel: "telegram", CreatedAt: sent, SentAt: &sent, Status: "sent",
	}); err != nil {
		t.Fatalf("AddDelivery: %v", err)
	}
	if got, err := st.LastSent(ctx, incident.Rule, incident.Subject); err != nil || got == nil {
		t.Fatalf("LastSent = %+v, %v; want the escalation", got, err)
	}
}
