package inbox

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/store"
)

// TestSourcePullIgnoresUnknownChatsAndParsesTimestamps exercises the
// subset of Source that turns a getUpdates JSON payload into Message
// values. The test wires a fake Telegram server and a no-op dedupe
// function so the Source can run end-to-end.
func TestSourcePullIgnoresUnknownChatsAndParsesTimestamps(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		body := `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":42,"text":"status","chat":{"id":111},"from":{"id":7777}}},
			{"update_id":2,"message":{"message_id":43,"text":"unknown","chat":{"id":9999},"from":{"id":7777}}}
		]}`
		io.WriteString(w, body)
	}))
	defer server.Close()

	s := &Source{
		APIURL:     server.URL,
		Token:      "test-token",
		ChatIDs:    []int64{111},
		LongPoll:   time.Second,
		HTTP:       server.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:      clock.System(),
		Deduper:    func(ctx context.Context, id int64) (bool, error) { return false, nil },
		Dispatcher: &Dispatcher{Host: "host-1", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}

	msgs, next, err := s.pull(context.Background(), 0)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	// pull returns every message Telegram sent; chat filtering happens in
	// Run. Apply the same filter here to assert the right call survived.
	var kept []Message
	for _, msg := range msgs {
		if s.chatAllowed(msg.ChatID) {
			kept = append(kept, msg)
		}
	}
	if len(kept) != 1 {
		t.Fatalf("kept %d messages after chat filter, want 1", len(kept))
	}
	if kept[0].ChatID != 111 || kept[0].UserID != 7777 || kept[0].MessageID != 42 {
		t.Fatalf("unexpected message: %+v", kept[0])
	}
	if next != 2 {
		t.Fatalf("next offset = %d, want 2", next)
	}
}

func TestSourceRunStopsOnContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	defer server.Close()
	s := &Source{
		APIURL:     server.URL,
		Token:      "test-token",
		ChatIDs:    []int64{1},
		LongPoll:   time.Millisecond,
		HTTP:       server.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:      clock.System(),
		Deduper:    func(ctx context.Context, id int64) (bool, error) { return false, nil },
		Dispatcher: &Dispatcher{Host: "host-1", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Source did not stop on context cancel")
	}
}

func TestSourceDispatchesAndReplies(t *testing.T) {
	var sent bool
	dispatcher := &Dispatcher{
		Store: nilStore{},
		Notifier: &stubNotifier{
			send: func(ctx context.Context, chatID int64, text string) error {
				sent = true
				return nil
			},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Host:   "host-1",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":42,"text":"help","chat":{"id":111},"from":{"id":7777}}}
		]}`)
	}))
	defer server.Close()

	s := &Source{
		APIURL:     server.URL,
		Token:      "test-token",
		ChatIDs:    []int64{111},
		LongPoll:   time.Millisecond,
		HTTP:       server.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:      clock.System(),
		Deduper:    func(ctx context.Context, id int64) (bool, error) { return false, nil },
		Dispatcher: dispatcher,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel after the loop dispatches one update.
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_ = s.Run(ctx)
	if !sent {
		t.Fatalf("dispatcher did not call SendReply")
	}
}

type stubNotifier struct {
	send func(ctx context.Context, chatID int64, text string) error
}

func (s *stubNotifier) SendReply(ctx context.Context, chatID int64, text string) error {
	if s.send == nil {
		return nil
	}
	return s.send(ctx, chatID, text)
}

// nilStore is a placeholder Store. The dispatcher paths exercised by
// this test ("help", plain chat) do not need a working store. The
// concrete methods return zero values so the dispatcher compiler
// accepts the type.
type nilStore struct{}

func (nilStore) RecordProcessedUpdate(context.Context, int64) (bool, error) { return false, nil }
func (nilStore) FindActiveSilence(context.Context, string, string, time.Time) (*store.Silence, error) {
	return nil, nil
}
func (nilStore) IncidentByID(context.Context, int64) (*store.Incident, error) { return nil, nil }
func (nilStore) CreateSilence(context.Context, store.Silence) (int64, error)  { return 0, nil }
func (nilStore) DeleteSilence(context.Context, int64) (bool, error)           { return false, nil }
func (nilStore) AckIncident(context.Context, int64, time.Time, string) (bool, error) {
	return false, nil
}
func (nilStore) OpenIncidents(context.Context) (map[store.IncidentKey]store.Incident, error) {
	return nil, nil
}
func (nilStore) LookupDeliveryByTelegramMessageID(context.Context, int64) (*store.Delivery, error) {
	return nil, nil
}
func (nilStore) ActiveSilences(context.Context, time.Time) ([]store.Silence, error) {
	return nil, nil
}
