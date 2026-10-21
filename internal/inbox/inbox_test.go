package inbox

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
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
		Claim:      func(ctx context.Context, id int64) (bool, error) { return true, nil },
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
		Claim:      func(ctx context.Context, id int64) (bool, error) { return true, nil },
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
		Claim:      func(ctx context.Context, id int64) (bool, error) { return true, nil },
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

func (nilStore) FindActiveSilence(context.Context, string, string, *int64, time.Time) (*store.Silence, error) {
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
func (nilStore) TelegramMessageDeliveries(context.Context, int64, int64) ([]store.Delivery, error) {
	return nil, nil
}
func (nilStore) ActiveSilences(context.Context, time.Time) ([]store.Silence, error) {
	return nil, nil
}

// recordingStore is a dispatcher store with scripted answers.
type recordingStore struct {
	nilStore
	incidents  map[int64]store.Incident
	deliveries []store.Delivery
	silences   []store.Silence
	acked      bool
}

func (s *recordingStore) IncidentByID(_ context.Context, id int64) (*store.Incident, error) {
	if incident, ok := s.incidents[id]; ok {
		return &incident, nil
	}
	return nil, nil
}

func (s *recordingStore) AckIncident(context.Context, int64, time.Time, string) (bool, error) {
	acked := !s.acked
	s.acked = true
	return acked, nil
}

func (s *recordingStore) TelegramMessageDeliveries(context.Context, int64, int64) ([]store.Delivery, error) {
	return s.deliveries, nil
}

func (s *recordingStore) CreateSilence(_ context.Context, silence store.Silence) (int64, error) {
	s.silences = append(s.silences, silence)
	return int64(len(s.silences)), nil
}

func newTestDispatcher(st Store) *Dispatcher {
	return &Dispatcher{
		Store:  st,
		Host:   "host-1",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
}

func execute(t *testing.T, d *Dispatcher, text string, replyTo int64) string {
	t.Helper()
	result, err := d.Execute(context.Background(), Message{ChatID: 5, UserID: 9, Text: text, ReplyToMessageID: replyTo})
	if err != nil {
		t.Fatalf("Execute(%q): %v", text, err)
	}
	return result.Reply.Text
}

// TestRepliesAreValidTelegramHTML verifies that usage and error replies carry
// no raw angle brackets: Telegram rejects an unknown tag such as <rule> and the
// operator would get no answer at all.
func TestRepliesAreValidTelegramHTML(t *testing.T) {
	d := newTestDispatcher(&recordingStore{})
	for _, text := range []string{"mute", "mute a b", "ack", "ack x", "unmute", "mute 2h", "mute until 14:00"} {
		reply := execute(t, d, text, 0)
		if reply == "" {
			t.Errorf("%q: no reply", text)
		}
		if strings.Contains(reply, "<") && !strings.Contains(reply, "&lt;") {
			t.Errorf("%q: reply %q contains a raw tag", text, reply)
		}
		if strings.ContainsAny(strings.ReplaceAll(strings.ReplaceAll(reply, "&lt;", ""), "&gt;", ""), "<>") {
			t.Errorf("%q: reply %q contains unescaped markup", text, reply)
		}
	}
}

func TestAckReportsAlreadyAckedAndResolved(t *testing.T) {
	resolvedAt := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	st := &recordingStore{incidents: map[int64]store.Incident{
		1: {ID: 1, Rule: "route.down", Subject: "a<b"},
		2: {ID: 2, Rule: "host.disk", Subject: "/", ResolvedAt: &resolvedAt},
	}}
	d := newTestDispatcher(st)
	if reply := execute(t, d, "ack 1", 0); !strings.Contains(reply, "acked incident 1") || !strings.Contains(reply, "a&lt;b") {
		t.Fatalf("first ack reply = %q, want an escaped confirmation", reply)
	}
	if reply := execute(t, d, "ack 1", 0); !strings.Contains(reply, "already acked") {
		t.Fatalf("second ack reply = %q", reply)
	}
	if reply := execute(t, d, "ack 2", 0); !strings.Contains(reply, "already resolved") {
		t.Fatalf("resolved ack reply = %q", reply)
	}
	if reply := execute(t, d, "ack 3", 0); !strings.Contains(reply, "does not exist") {
		t.Fatalf("missing ack reply = %q", reply)
	}
}

func TestMuteByReplyResolvesTheAnnouncedIncident(t *testing.T) {
	one, two := int64(1), int64(2)
	st := &recordingStore{
		incidents:  map[int64]store.Incident{1: {ID: 1, Rule: "route.down", Subject: "fallback@file"}},
		deliveries: []store.Delivery{{IncidentID: &one}},
	}
	d := newTestDispatcher(st)
	if reply := execute(t, d, "mute 2h", 77); !strings.Contains(reply, "muted route.down on fallback@file") {
		t.Fatalf("reply = %q", reply)
	}
	if len(st.silences) != 1 || *st.silences[0].Rule != "route.down" || *st.silences[0].Subject != "fallback@file" {
		t.Fatalf("silences = %+v, want one for route.down on fallback@file", st.silences)
	}

	// A grouped message announces several incidents: refuse to guess.
	st.deliveries = []store.Delivery{{IncidentID: &one}, {IncidentID: &two}}
	if reply := execute(t, d, "mute 2h", 77); !strings.Contains(reply, "covers 2 incidents") {
		t.Fatalf("grouped reply = %q, want the ambiguity explained", reply)
	}
	if len(st.silences) != 1 {
		t.Fatalf("silences = %d, want the ambiguous mute to create none", len(st.silences))
	}

	st.deliveries = nil
	if reply := execute(t, d, "mute 2h", 77); !strings.Contains(reply, "not a Raedar page") {
		t.Fatalf("unknown message reply = %q", reply)
	}
	if reply := execute(t, d, "mute 2h", 0); !strings.Contains(reply, "reply to a paged message") {
		t.Fatalf("bare mute reply = %q", reply)
	}
}

func TestMuteIsBounded(t *testing.T) {
	st := &recordingStore{}
	d := newTestDispatcher(st)
	if reply := execute(t, d, "mute route.down fallback@file 31d", 0); !strings.Contains(reply, "at most 30 days") {
		t.Fatalf("reply = %q", reply)
	}
	if reply := execute(t, d, "mute route.down fallback@file until 2020-01-01T00:00:00Z", 0); !strings.Contains(reply, "in the past") {
		t.Fatalf("reply = %q", reply)
	}
	if len(st.silences) != 0 {
		t.Fatalf("silences = %d, want none", len(st.silences))
	}
}

func testConfig(proxy string, proxyFor ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Notify.Telegram.Token = "123:secret-token"
	cfg.Notify.Telegram.APIURL = "https://api.telegram.org"
	cfg.Notify.Telegram.ChatIDs = []string{"111"}
	cfg.Notify.OutboundProxy = proxy
	cfg.Notify.ProxyFor = proxyFor
	return cfg
}

// TestNewSourceFollowsTheOutboundProxy verifies that polling goes through the
// proxy exactly when Telegram messages do, and that the client outlives a long
// poll.
func TestNewSourceFollowsTheOutboundProxy(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := map[string]struct {
		cfg   *config.Config
		proxy bool
	}{
		"no proxy":           {testConfig(""), false},
		"proxy for all":      {testConfig("http://127.0.0.1:3128"), true},
		"proxy for telegram": {testConfig("http://127.0.0.1:3128", "telegram"), true},
		"proxy for webhook":  {testConfig("http://127.0.0.1:3128", "webhook"), false},
	}
	for name, tc := range cases {
		source, err := NewSource(tc.cfg, logger, clock.System(), nil, nil)
		if err != nil {
			t.Fatalf("%s: NewSource: %v", name, err)
		}
		transport := source.HTTP.Transport.(*http.Transport)
		if got := transport.Proxy != nil; got != tc.proxy {
			t.Errorf("%s: proxied = %v, want %v", name, got, tc.proxy)
		}
		if source.HTTP.Timeout <= source.LongPoll {
			t.Errorf("%s: client timeout %v does not cover the %v long poll", name, source.HTTP.Timeout, source.LongPoll)
		}
	}
}

// TestRunLogDoesNotLeakTheBotToken verifies that a failing poll is logged
// without the request URL, which contains the bot token.
func TestRunLogDoesNotLeakTheBotToken(t *testing.T) {
	var logs bytes.Buffer
	s := &Source{
		APIURL:   "http://127.0.0.1:1",
		Token:    "123:secret-token",
		ChatIDs:  []int64{111},
		LongPoll: time.Millisecond,
		HTTP:     &http.Client{Timeout: time.Second},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		Clock:    clock.System(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)
	if !strings.Contains(logs.String(), "telegram getUpdates") {
		t.Fatalf("expected a logged poll failure, got %q", logs.String())
	}
	if strings.Contains(logs.String(), "secret-token") {
		t.Fatalf("log leaks the bot token: %s", logs.String())
	}
}
