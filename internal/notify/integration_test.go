//go:build integration

package notify

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// TestPagerRowsPerIncidentAndChat verifies that a group delivered to two
// chats records one delivery row per (incident × chat), attaches the
// message_id of the right chat to each row, and lets a reply resolve back
// to a delivery.
func TestPagerRowsPerIncidentAndChat(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()

	incA := insertIncident(t, ctx, st, "route.down", "fallback@file", store.SeverityCritical)
	incB := insertIncident(t, ctx, st, "route.down", "waiotech-api@docker", store.SeverityWarning)

	tg := &scriptedTelegram{
		chatIDs: []string{"101", "202"},
		outcomes: map[string]scriptedOutcome{
			"101": {messageID: 1001},
			"202": {messageID: 1002},
		},
	}
	notifier := integrationNotifier(t, st, []string{"101", "202"}, tg)

	notifier.IncidentsChanged(ctx, []rules.Change{
		{Incident: incA, Kind: rules.ChangeOpened},
		{Incident: incB, Kind: rules.ChangeOpened},
	})

	for _, incident := range []store.Incident{incA, incB} {
		rows, err := st.Deliveries(ctx, incident.ID)
		if err != nil {
			t.Fatalf("Deliveries(%d): %v", incident.ID, err)
		}
		if len(rows) != 2 {
			t.Fatalf("incident %d rows = %d, want 2", incident.ID, len(rows))
		}
		counts := map[int64]int{}
		for _, row := range rows {
			if row.TelegramMessageID == nil {
				t.Fatalf("incident %d row %d has no telegram_message_id", incident.ID, row.ID)
			}
			counts[*row.TelegramMessageID]++
		}
		if counts[1001] != 1 || counts[1002] != 1 {
			t.Fatalf("incident %d message ids = %v, want one of 1001 and 1002", incident.ID, counts)
		}
	}

	// A reply resolves through the chat and message id together; the same
	// message id in another chat is a different message.
	found, err := st.TelegramMessageDeliveries(ctx, 202, 1002)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("deliveries of chat 202 message 1002 = %d, want one per incident", len(found))
	}
	if other, err := st.TelegramMessageDeliveries(ctx, 101, 1002); err != nil || len(other) != 0 {
		t.Fatalf("chat 101 message 1002 = %v, %v; want none", other, err)
	}
}

// TestPagerRetriesOnlyFailedChats is the regression test for the original
// duplicate-page bug: when one chat fails, the retry must not post again
// to the chats that already succeeded.
func TestPagerRetriesOnlyFailedChats(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	incident := insertIncident(t, ctx, st, "route.down", "fallback@file", store.SeverityCritical)

	tg := &scriptedTelegram{
		chatIDs: []string{"personal", "group"},
		outcomes: map[string]scriptedOutcome{
			"personal": {messageID: 2001},
			"group":    {err: errors.New("bot was kicked")},
		},
	}
	notifier := integrationNotifier(t, st, []string{"personal", "group"}, tg)
	notifier.IncidentsChanged(ctx, []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})

	if got := tg.callsFor("personal"); got != 1 {
		t.Fatalf("personal calls after first attempt = %d, want 1", got)
	}
	if got := tg.callsFor("group"); got != 1 {
		t.Fatalf("group calls after first attempt = %d, want 1", got)
	}

	// Run the retry queue once with the failing chat now healthy.
	tg.setOutcome("group", scriptedOutcome{messageID: 2002})
	notifier.mu.Lock()
	queued := len(notifier.retries)
	// Pretend the retry delay has passed.
	for _, pending := range notifier.retries {
		pending.at = time.Now().Add(-time.Second)
	}
	notifier.mu.Unlock()
	if queued == 0 {
		t.Fatal("no retry was queued")
	}
	notifier.deliverDue(ctx)

	if got := tg.callsFor("personal"); got != 1 {
		t.Fatalf("personal calls after retry = %d, want still 1 (must not duplicate)", got)
	}
	if got := tg.callsFor("group"); got != 2 {
		t.Fatalf("group calls after retry = %d, want 2", got)
	}
}

// integrationStore opens the disposable database, migrates it, and returns
// the store. It skips when RAEDAR_TEST_DATABASE_URL is unset.
func integrationStore(t *testing.T) *store.Store {
	t.Helper()
	databaseURL, schema := testdb.NewSchema(t, "pager")
	databaseURL = testdb.WithSearchPath(t, databaseURL, schema)
	ctx := t.Context()
	st, err := store.Open(ctx, databaseURL, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func insertIncident(t *testing.T, ctx context.Context, st *store.Store, rule, subject string, severity store.Severity) store.Incident {
	t.Helper()
	incident := store.Incident{
		Rule: rule, Subject: subject, Severity: severity,
		OpenedAt: time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC),
		Summary:  "test incident", Details: []byte(`{}`),
	}
	id, err := st.InsertIncident(ctx, incident)
	if err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	incident.ID = id
	return incident
}

// minimalConfig returns the test config restricted to the telegram
// channel so a test can count delivery rows precisely.
func minimalConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Notify.WarningChannels = []string{"telegram"}
	cfg.Notify.CriticalChannels = []string{"telegram"}
	cfg.Notify.DigestChannels = []string{"telegram"}
	return cfg
}

func integrationNotifier(t *testing.T, st *store.Store, chatIDs []string, tg Channel) *Notifier {
	t.Helper()
	cfg := minimalConfig(t)
	cfg.Notify.Telegram.Token = "test-token"
	cfg.Notify.Telegram.ChatIDs = chatIDs
	cfg.Notify.WarningChannels = []string{"telegram"}
	cfg.Notify.CriticalChannels = []string{"telegram"}
	cfg.Notify.DigestChannels = []string{"telegram"}
	notifier, err := New(cfg, st, nil, discardLogger(), clock.System())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	notifier.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	notifier.channels[KindTelegram] = tg
	return notifier
}

// scriptedOutcome is the result of posting to one chat.
type scriptedOutcome struct {
	messageID int64
	err       error
}

// scriptedTelegram is a fan-out channel whose per-chat results are
// scripted, so a test can make one chat fail and another succeed.
type scriptedTelegram struct {
	mu       sync.Mutex
	chatIDs  []string
	outcomes map[string]scriptedOutcome
	calls    map[string]int
}

func (s *scriptedTelegram) Kind() Kind        { return KindTelegram }
func (s *scriptedTelegram) ChatIDs() []string { return append([]string(nil), s.chatIDs...) }
func (s *scriptedTelegram) Send(ctx context.Context, message Message) error {
	var errs []error
	for _, result := range s.SendEach(ctx, message, nil) {
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}
	return errors.Join(errs...)
}

func (s *scriptedTelegram) SendEach(ctx context.Context, message Message, chats []string) []chatSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	if len(chats) == 0 {
		chats = s.chatIDs
	}
	results := make([]chatSend, 0, len(chats))
	for _, chatID := range chats {
		s.calls[chatID]++
		outcome := s.outcomes[chatID]
		results = append(results, chatSend{chatID: chatID, messageID: outcome.messageID, err: outcome.err})
	}
	return results
}

func (s *scriptedTelegram) callsFor(chatID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[chatID]
}

func (s *scriptedTelegram) setOutcome(chatID string, outcome scriptedOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[chatID] = outcome
}
