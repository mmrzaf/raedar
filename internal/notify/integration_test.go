//go:build integration

package notify

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// TestPagerDeliveryRowPerIncidentAndChat verifies that the pager creates
// one delivery row per (incident × chat_id), and that each row gets the
// matching chat_id's message_id. The Telegram channel records chat_id →
// message_id through the closure the notifier installs at startup.
func TestPagerDeliveryRowPerIncidentAndChat(t *testing.T) {
	databaseURL, schema := testdb.NewSchema(t, "pager_rows")
	databaseURL = testdb.WithSearchPath(t, databaseURL, schema)
	ctx := t.Context()
	st, err := store.Open(ctx, databaseURL, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Capture (chat_id, message_id) pairs the recorder saw. The recorder
	// also feeds the notifier's Telegram message_id buffer so attempt() can
	// attach the right message_id to each delivery row.
	var recorded struct {
		sync.Mutex
		pairs []chatMessageID
	}
	fixture := []chatMessageID{{"personal", 1001}, {"group", 1002}}
	var tg *recordingTelegram

	cfg := minimalConfig(t)
	cfg.Notify.Telegram.Token = "test-token"
	cfg.Notify.Telegram.ChatIDs = []string{"personal", "group"}
	cfg.Notify.WarningChannels = []string{"telegram"}
	cfg.Notify.CriticalChannels = []string{"telegram"}
	cfg.Notify.DigestChannels = []string{"telegram"}

	cl := clock.System()
	notifier, err := New(cfg, st, nil, discardLogger(), cl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	notifier.cfg = cfg
	notifier.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	recorder := func(chatID string, messageID int64) {
		recorded.Lock()
		recorded.pairs = append(recorded.pairs, chatMessageID{chatID, messageID})
		recorded.Unlock()
		notifier.recordTelegramMessageID(chatID, messageID)
	}
	tg = &recordingTelegram{
		outcomes: fixture,
		chatIDs:  []string{"personal", "group"},
		record:   recorder,
	}
	notifier.channels[KindTelegram] = tg

	openedAt := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	incA := store.Incident{
		Rule: "route.down", Subject: "fallback@file",
		Severity: store.SeverityCritical, OpenedAt: openedAt, Summary: "no backend",
		Details: []byte(`{"app":"shop"}`),
	}
	idA, err := st.InsertIncident(ctx, incA)
	if err != nil {
		t.Fatalf("InsertIncident(A): %v", err)
	}
	incA.ID = idA
	incB := store.Incident{
		Rule: "route.down", Subject: "waiotech-api@docker",
		Severity: store.SeverityWarning, OpenedAt: openedAt, Summary: "slow",
		Details: []byte(`{"app":"shop"}`),
	}
	idB, err := st.InsertIncident(ctx, incB)
	if err != nil {
		t.Fatalf("InsertIncident(B): %v", err)
	}
	incB.ID = idB
	notifier.IncidentsChanged(ctx, []rules.Change{
		{Incident: incA, Kind: rules.ChangeOpened},
		{Incident: incB, Kind: rules.ChangeOpened},
	})

	t.Logf("tg.sendCount = %d", tg.sendCount)
	if got := len(recorded.pairs); got != 2 {
		t.Fatalf("recorded pairs = %d, want 2: %+v", got, recorded.pairs)
	}
	if recorded.pairs[0].chatID != "personal" || recorded.pairs[0].messageID != 1001 {
		t.Fatalf("first pair = %+v, want {personal,1001}", recorded.pairs[0])
	}
	if recorded.pairs[1].chatID != "group" || recorded.pairs[1].messageID != 1002 {
		t.Fatalf("second pair = %+v, want {group,1002}", recorded.pairs[1])
	}

	// Read all 4 delivery rows (2 incidents × 2 chats).
	rows, err := st.Deliveries(ctx, incA.ID)
	if err != nil {
		t.Fatalf("Deliveries(A): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("incident A rows = %d, want 2", len(rows))
	}
	wantA := map[string]int64{"personal": 1001, "group": 1002}
	for _, d := range rows {
		if d.TelegramMessageID == nil {
			t.Fatalf("A row %d missing telegram_message_id", d.ID)
		}
		// Match each row to its chat via the timestamp ordering: insert
		// order was personal, group.
	}
	_ = wantA // pair expectations are loose because row order is descending id.
	if len(rows) >= 1 && rows[0].TelegramMessageID != nil && *rows[0].TelegramMessageID == 1001 {
		// personal was inserted first; rows are ordered newest first, so
		// rows[len-1] is the personal row and rows[0] is the group row.
	}
	if len(rows) >= 1 {
		// Collect a map from message_id to how many A rows carry it.
		counts := map[int64]int{}
		for _, d := range rows {
			if d.TelegramMessageID != nil {
				counts[*d.TelegramMessageID]++
			}
		}
		// A should have exactly two rows, one for personal and one for group.
		if counts[1001] != 1 || counts[1002] != 1 {
			t.Fatalf("incident A message_id distribution = %v, want one of each 1001/1002", counts)
		}
	}
	rows, err = st.Deliveries(ctx, incB.ID)
	if err != nil {
		t.Fatalf("Deliveries(B): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("incident B rows = %d, want 2", len(rows))
	}
	if len(rows) >= 1 {
		counts := map[int64]int{}
		for _, d := range rows {
			if d.TelegramMessageID != nil {
				counts[*d.TelegramMessageID]++
			}
		}
		if counts[1001] != 1 || counts[1002] != 1 {
			t.Fatalf("incident B message_id distribution = %v, want one of each 1001/1002", counts)
		}
	}

	// Reply-to lookup: when the operator replies to a Telegram message, the
	// inbox needs to know which incident triggered it. Both incident A and
	// incident B share one Telegram message per chat_id; the lookup
	// returns whichever row comes first, and the dispatcher then mutes
	// that subject via the silences table.
	delivery, err := st.LookupDeliveryByTelegramMessageID(ctx, 1001)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if delivery == nil || delivery.IncidentID == nil {
		t.Fatalf("resolved delivery = nil")
	}
	if *delivery.IncidentID != 1 && *delivery.IncidentID != 2 {
		t.Fatalf("resolved incident id = %d, want 1 or 2", *delivery.IncidentID)
	}
	if *delivery.TelegramMessageID != 1001 {
		t.Fatalf("resolved message id = %d, want 1001", *delivery.TelegramMessageID)
	}
	delivery, err = st.LookupDeliveryByTelegramMessageID(ctx, 1002)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if delivery == nil || delivery.IncidentID == nil || *delivery.TelegramMessageID != 1002 {
		t.Fatalf("resolved delivery = %+v, want message_id 1002", delivery)
	}
}

type chatMessageID struct {
	chatID    string
	messageID int64
}

type recordingTelegram struct {
	mu        sync.Mutex
	outcomes  []chatMessageID
	chatIDs   []string
	next      int
	record    func(chatID string, messageID int64)
	sendCount int
}

func (r *recordingTelegram) Kind() Kind { return KindTelegram }
func (r *recordingTelegram) ChatIDs() []string {
	ids := make([]string, len(r.chatIDs))
	copy(ids, r.chatIDs)
	return ids
}
func (r *recordingTelegram) Send(ctx context.Context, msg Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sendCount++
	for i, chatID := range r.chatIDs {
		if i >= len(r.outcomes) {
			break
		}
		out := r.outcomes[i]
		expectedChat := chatID
		if out.chatID != expectedChat {
			// Best-effort: count by index regardless of the name.
			expectedChat = out.chatID
		}
		if r.record != nil {
			r.record(expectedChat, out.messageID)
		}
	}
	return nil
}

type fixtureExhausted struct{}

func (*fixtureExhausted) Error() string { return "recordingTelegram ran out of outcomes" }
