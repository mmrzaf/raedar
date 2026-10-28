//go:build integration

package store

import (
	"testing"
	"time"
)

// TestPagesLifecycle covers the durable notification state: creation, the due
// queue, announcement counts, ack cancellation and reopen.
func TestPagesLifecycle(t *testing.T) {
	st := newMigratedStore(t)
	ctx := t.Context()
	incident := newTestIncident(t, st, "container.restarting")
	current, err := st.IncidentByID(ctx, incident.ID)
	if err != nil || current == nil {
		t.Fatalf("IncidentByID = %v, %v", current, err)
	}
	now := time.Now().Truncate(time.Microsecond)
	page, err := st.CreatePage(ctx, Page{
		IncidentID: incident.ID, Episode: current.Episode, Channel: "telegram", Recipient: "101",
		Status: PagePending, Kind: DeliveryOpened, Severity: SeverityCritical, DueAt: now,
	})
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	if page.ID == 0 {
		t.Fatal("CreatePage returned no ID")
	}
	due, err := st.DuePages(ctx, now.Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0].ID != page.ID {
		t.Fatalf("DuePages = %+v, %v; want the new page", due, err)
	}
	announced := now.Add(time.Minute)
	page.Status = PageAnnounced
	page.AnnouncedAt = &announced
	if err := st.UpdatePage(ctx, page); err != nil {
		t.Fatalf("UpdatePage: %v", err)
	}
	count, last, err := st.Announcements(ctx, incident.Rule, incident.Subject, "telegram", now)
	if err != nil || count != 1 || last == nil || !last.Equal(announced) {
		t.Fatalf("Announcements = %d, %v, %v; want one at %v", count, last, err, announced)
	}

	sentAt := now.Add(30 * time.Second)
	incidentID := incident.ID
	if _, err := st.AddDelivery(ctx, Delivery{
		IncidentID: &incidentID, Kind: DeliveryOpened, Channel: "telegram", Recipient: "101",
		CreatedAt: sentAt, SentAt: &sentAt, Status: "sent",
	}); err != nil {
		t.Fatalf("AddDelivery: %v", err)
	}
	seed, err := st.SentOpeningAt(ctx, incident.ID, current.OpenedAt, "telegram", "101")
	if err != nil || seed == nil || !seed.Equal(sentAt) {
		t.Fatalf("SentOpeningAt = %v, %v; want %v", seed, err, sentAt)
	}

	pending, err := st.CreatePage(ctx, Page{
		IncidentID: incident.ID, Episode: current.Episode, Channel: "sms", Recipient: "+98912",
		Status: PagePending, Kind: DeliveryOpened, Severity: SeverityCritical, DueAt: now,
	})
	if err != nil {
		t.Fatalf("CreatePage sms: %v", err)
	}
	if acked, err := st.AckIncident(ctx, incident.ID, now, "telegram:1"); err != nil || !acked {
		t.Fatalf("AckIncident = %v, %v", acked, err)
	}
	pages, err := st.PagesForEpisode(ctx, incident.ID, current.Episode)
	if err != nil {
		t.Fatalf("PagesForEpisode: %v", err)
	}
	for _, saved := range pages {
		if saved.ID == pending.ID && saved.Status != PageCancelled {
			t.Fatalf("pending page after ack = %+v, want cancelled", saved)
		}
		if saved.ID == page.ID && saved.Status != PageAnnounced {
			t.Fatalf("announced page after ack = %+v, want it kept", saved)
		}
	}
	rows, err := st.Deliveries(ctx, incident.ID)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	var acked bool
	for _, row := range rows {
		acked = acked || row.Status == "acked"
	}
	if !acked {
		t.Fatal("no acked delivery row recorded")
	}
}

// TestReopenStartsANewEpisodeAndCancelsOldPages verifies the episode counter
// and that ack and pending pages do not leak into the new episode.
func TestReopenStartsANewEpisodeAndCancelsOldPages(t *testing.T) {
	st := newMigratedStore(t)
	ctx := t.Context()
	incident := newTestIncident(t, st, "container.restarting")
	current, err := st.IncidentByID(ctx, incident.ID)
	if err != nil || current == nil {
		t.Fatalf("IncidentByID = %v, %v", current, err)
	}
	_, err = st.CreatePage(ctx, Page{
		IncidentID: incident.ID, Episode: current.Episode, Channel: "telegram", Recipient: "",
		Status: PagePending, Kind: DeliveryOpened, Severity: SeverityCritical, DueAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	if _, err := st.AckIncident(ctx, incident.ID, time.Now(), "telegram:1"); err != nil {
		t.Fatalf("AckIncident: %v", err)
	}
	if err := st.ResolveIncident(ctx, incident.ID, time.Now()); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	reopened := *current
	reopened.OpenedAt = time.Now()
	episode, err := st.ReopenIncident(ctx, reopened)
	if err != nil {
		t.Fatalf("ReopenIncident: %v", err)
	}
	if episode != current.Episode+1 {
		t.Fatalf("episode = %d, want %d", episode, current.Episode+1)
	}
	got, err := st.IncidentByID(ctx, incident.ID)
	if err != nil || got == nil {
		t.Fatalf("IncidentByID after reopen = %v, %v", got, err)
	}
	if got.Acked() || got.ResolvedAt != nil || got.Episode != episode {
		t.Fatalf("reopened incident = %+v, want open, unacked, episode %d", got, episode)
	}
	saved, err := st.Page(ctx, incident.ID, current.Episode, "telegram", "")
	if err != nil || saved == nil || saved.Status != PageCancelled {
		t.Fatalf("old page = %+v, %v; want cancelled", saved, err)
	}
}

// TestDeleteSilenceWakesPages verifies that removing a silence brings its
// pending pages forward immediately.
func TestDeleteSilenceWakesPages(t *testing.T) {
	st := newMigratedStore(t)
	ctx := t.Context()
	incident := newTestIncident(t, st, "route.down")
	current, err := st.IncidentByID(ctx, incident.ID)
	if err != nil || current == nil {
		t.Fatalf("IncidentByID = %v, %v", current, err)
	}
	now := time.Now()
	silenceID, err := st.CreateSilence(ctx, Silence{
		Kind: SilenceOneShotMute, Rule: &incident.Rule, Subject: &incident.Subject,
		Reason: "noise", StartsAt: now, EndsAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSilence: %v", err)
	}
	page, err := st.CreatePage(ctx, Page{
		IncidentID: incident.ID, Episode: current.Episode, Channel: "telegram", Recipient: "",
		Status: PagePending, Kind: DeliveryOpened, Severity: SeverityCritical,
		DueAt: now.Add(time.Hour), Reason: "silence #1", SilenceID: &silenceID,
	})
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	if deleted, err := st.DeleteSilence(ctx, silenceID); err != nil || !deleted {
		t.Fatalf("DeleteSilence = %v, %v", deleted, err)
	}
	saved, err := st.Page(ctx, incident.ID, current.Episode, "telegram", "")
	if err != nil || saved == nil {
		t.Fatalf("Page = %v, %v", saved, err)
	}
	if saved.DueAt.After(time.Now()) || saved.Reason != "silence removed" {
		t.Fatalf("page after delete = %+v, want woken", saved)
	}
	if saved.ID != page.ID {
		t.Fatalf("page id = %d, want %d", saved.ID, page.ID)
	}
}

// TestPruneSilences verifies the generated silence rows are removed without
// touching an operator's maintenance window.
func TestPruneSilences(t *testing.T) {
	st := newMigratedStore(t)
	ctx := t.Context()
	now := time.Now()
	autoID, err := st.CreateSilence(ctx, Silence{
		Kind: SilenceAutoDeploy, Reason: "old deploy",
		StartsAt: now.Add(-48 * time.Hour), EndsAt: now.Add(-47 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSilence auto: %v", err)
	}
	muteID, err := st.CreateSilence(ctx, Silence{
		Kind: SilenceOneShotMute, Reason: "old mute",
		StartsAt: now.Add(-40 * 24 * time.Hour), EndsAt: now.Add(-31 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSilence mute: %v", err)
	}
	maintenanceID, err := st.CreateSilence(ctx, Silence{
		Kind: SilenceMaintenanceWindow, Reason: "history",
		StartsAt: now.Add(-48 * time.Hour), EndsAt: now.Add(-47 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSilence maintenance: %v", err)
	}
	if pruned, err := st.PruneSilences(ctx, now); err != nil || pruned != 2 {
		t.Fatalf("PruneSilences = %d, %v; want 2", pruned, err)
	}
	remaining, err := st.ListSilences(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != maintenanceID {
		t.Fatalf("remaining silences = %+v, want only maintenance %d", remaining, maintenanceID)
	}
	_ = autoID
	_ = muteID
}
