package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
)

// testClock is a clock the tests move.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig returns the documented defaults with the given database URL.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(func(name string) (string, bool) {
		if name == "RAEDAR_DATABASE_URL" {
			return "postgres://raedar:raedar@postgres:5432/raedar?sslmode=disable", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg
}

// minimalConfig returns the test config with Telegram channels restricted
// to telegram so the pager only fans out one direction, used by
// integration tests that need to count rows precisely.
func minimalConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Notify.WarningChannels = []string{"telegram"}
	cfg.Notify.CriticalChannels = []string{"telegram"}
	cfg.Notify.DigestChannels = []string{"telegram"}
	return cfg
}

// recordedRequest is one request a test server received.
type recordedRequest struct {
	method  string
	path    string
	query   string
	headers http.Header
	body    string
}

// server records the requests it receives and answers with a canned response.
type server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
}

// newServer starts a recording server.
func newServer(t *testing.T, status int, body string) *server {
	t.Helper()
	holder := &server{status: status, body: body}
	holder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		holder.mu.Lock()
		holder.requests = append(holder.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, headers: r.Header.Clone(), body: string(data),
		})
		holder.mu.Unlock()
		w.WriteHeader(holder.status)
		w.Write([]byte(holder.body))
	}))
	t.Cleanup(holder.Close)
	return holder
}

// received returns the recorded requests.
func (s *server) received() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

func TestTelegramChannel(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"ok":true}`)
	channel := &telegramChannel{
		apiURL:  srv.URL,
		token:   "123:secret-token",
		chatIDs: []string{"42", "-1007"},
		client:  srv.Client(),
	}
	if err := channel.Send(context.Background(), Message{Text: "hello"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	requests := srv.received()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want one per chat", len(requests))
	}
	first := requests[0]
	if first.method != http.MethodPost || first.path != "/bot123:secret-token/sendMessage" {
		t.Errorf("request = %s %s, want the sendMessage endpoint", first.method, first.path)
	}
	if contentType := first.headers.Get("Content-Type"); contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form encoding", contentType)
	}
	form, err := url.ParseQuery(first.body)
	if err != nil {
		t.Fatalf("parse form: %v", err)
	}
	if form.Get("chat_id") != "42" || form.Get("text") != "hello" {
		t.Errorf("form = %v, want the chat and the text", form)
	}
	if form.Get("parse_mode") != "HTML" || form.Get("disable_web_page_preview") != "true" {
		t.Errorf("form = %v, want HTML and no link previews", form)
	}
	if second, err := url.ParseQuery(requests[1].body); err != nil || second.Get("chat_id") != "-1007" {
		t.Errorf("second request chat = %v, want -1007", second)
	}
}

func TestHTTPSMSChannelShape(t *testing.T) {
	srv := newServer(t, http.StatusOK, "")
	defer srv.Close()
	channel := &httpSMSChannel{
		url:      srv.URL + "/v1/{{api_key}}/sms/send.json?receptor={{to}}&message={{message}}",
		method:   http.MethodPost,
		encoding: "form",
		from:     "1000",
		body:     "to={{to}}&from={{from}}&text={{message}}",
		to:       []string{"+989120000000", "+989120000001"},
		client:   srv.Client(),
	}
	if err := channel.Send(context.Background(), Message{SMS: "hello"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	requests := srv.received()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want one per recipient (2)", len(requests))
	}
	for i, request := range requests {
		if got := request.path; got != "/v1//sms/send.json" {
			t.Errorf("request %d path = %q, want the send endpoint", i, got)
		}
	}
}

func TestHTTPSMSEncodings(t *testing.T) {
	tests := []struct {
		name        string
		encoding    string
		body        string
		wantBody    string
		wantContent string
	}{
		{
			name:        "json",
			encoding:    "json",
			body:        `{"to": "{{to}}", "text": "{{message}}"}`,
			wantBody:    `{"to": "+989120000000", "text": "line \"one\""}`,
			wantContent: "application/json",
		},
		{
			name:        "form",
			encoding:    "form",
			body:        "to={{to}}&text={{message}}",
			wantBody:    "to=%2B989120000000&text=line+%22one%22",
			wantContent: "application/x-www-form-urlencoded",
		},
		{
			name:     "raw",
			encoding: "raw",
			body:     "{{to}}|{{message}}",
			wantBody: "+989120000000|line \"one\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, http.StatusOK, "ok")
			channel := &httpSMSChannel{
				url: srv.URL + "/send", method: http.MethodPost,
				body: tt.body, encoding: tt.encoding, to: []string{"+989120000000"}, client: srv.Client(),
			}
			if err := channel.Send(context.Background(), Message{SMS: `line "one"`}); err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			request := srv.received()[0]
			if request.body != tt.wantBody {
				t.Errorf("body = %q, want %q", request.body, tt.wantBody)
			}
			if contentType := request.headers.Get("Content-Type"); contentType != tt.wantContent {
				t.Errorf("Content-Type = %q, want %q", contentType, tt.wantContent)
			}
		})
	}
}

func TestHTTPSMSHeaderAndURLPlaceholders(t *testing.T) {
	srv := newServer(t, http.StatusOK, "ok")
	channel := &httpSMSChannel{
		url:      srv.URL + "/send?to={{to}}&text={{message}}",
		method:   http.MethodGet,
		headers:  []config.Header{{Name: "Authorization", Value: "Bearer token"}},
		encoding: "json",
		to:       []string{"a+b@example.com", "second"},
		client:   srv.Client(),
	}
	if err := channel.Send(context.Background(), Message{SMS: "hello world"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	requests := srv.received()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want one per recipient", len(requests))
	}
	if requests[0].method != http.MethodGet || requests[0].path != "/send" {
		t.Errorf("request = %s %s, want a GET to /send", requests[0].method, requests[0].path)
	}
	query, err := url.ParseQuery(requests[0].query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if query.Get("to") != "a+b@example.com" || query.Get("text") != "hello world" {
		t.Errorf("query = %v, want URL-encoded placeholders", query)
	}
	if requests[0].headers.Get("Authorization") != "Bearer token" {
		t.Errorf("Authorization = %q, want the configured header", requests[0].headers.Get("Authorization"))
	}
}

func TestWebhookChannelSignsTheBody(t *testing.T) {
	srv := newServer(t, http.StatusOK, "")
	clock := &testClock{now: time.Unix(1790000000, 0)}
	channel := &webhookChannel{url: srv.URL, secret: "s3cret", client: srv.Client(), clock: clock}
	document := []byte(`{"version":1}`)
	if err := channel.Send(context.Background(), Message{Document: document}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	request := srv.received()[0]
	if request.method != http.MethodPost || request.headers.Get("Content-Type") != "application/json" {
		t.Errorf("request = %s %q, want a JSON POST", request.method, request.headers.Get("Content-Type"))
	}
	if got := request.headers.Get("X-Raedar-Timestamp"); got != "1790000000" {
		t.Errorf("X-Raedar-Timestamp = %q, want the clock", got)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte("1790000000." + string(document)))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := request.headers.Get("X-Raedar-Signature"); got != want {
		t.Errorf("X-Raedar-Signature = %q, want %q", got, want)
	}
}

func TestWebhookChannelWithoutSecret(t *testing.T) {
	srv := newServer(t, http.StatusOK, "")
	channel := &webhookChannel{url: srv.URL, client: srv.Client(), clock: &testClock{now: time.Now()}}
	if err := channel.Send(context.Background(), Message{Document: []byte(`{}`)}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	request := srv.received()[0]
	if request.headers.Get("X-Raedar-Signature") != "" || request.headers.Get("X-Raedar-Timestamp") != "" {
		t.Error("an unsigned webhook carries signature headers")
	}
}

func TestChannelErrorsDoNotLeakSecrets(t *testing.T) {
	// A closed port makes the request fail before any response.
	channel := &telegramChannel{
		apiURL:  "http://127.0.0.1:1",
		token:   "123:very-secret",
		chatIDs: []string{"42"},
		client:  &http.Client{Timeout: time.Second},
	}
	err := channel.Send(context.Background(), Message{Text: "hello"})
	if err == nil {
		t.Fatal("Send() error = nil, want an error")
	}
	if strings.Contains(err.Error(), "very-secret") {
		t.Errorf("error = %q, want the token redacted", err.Error())
	}
}

func TestSMSLength(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC}
	incident := store.Incident{
		ID: 1, Rule: "route.errors", Subject: "shop@docker", Severity: "critical",
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC), Summary: "5xx errors",
		Details: []byte(`{"rate":0.064,"baseline":0.002,"app":"a-very-long-application-name-that-goes-on-and-on-and-on"}`),
	}
	text := f.smsText(group{kind: "opened", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	if len(text) > maxSMS {
		t.Errorf("SMS length = %d, want at most %d", len(text), maxSMS)
	}
	if !strings.HasPrefix(text, "RAEDAR host-1 CRITICAL ") {
		t.Errorf("SMS = %q, want the Raedar prefix", text)
	}
}

func TestSMSFormat(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC}
	down := store.Incident{
		ID: 1, Rule: "route.down", Subject: "shop@docker", Severity: "critical",
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC), Summary: "no backend for requests",
		Details: []byte(`{"requests":100,"no_backend":63,"app":"shop"}`),
	}
	text := f.smsText(group{kind: "opened", incidents: []store.Incident{down}, reopened: map[int64]bool{}})
	want := "RAEDAR host-1 CRITICAL shop: no backend for 63% of requests since 14:32"
	if text != want {
		t.Errorf("SMS = %q, want %q", text, want)
	}
	resolvedAt := down.OpenedAt.Add(18 * time.Minute)
	down.ResolvedAt = &resolvedAt
	text = f.smsText(group{kind: "resolved", incidents: []store.Incident{down}, reopened: map[int64]bool{}})
	want = "RAEDAR host-1 RESOLVED shop: no backend for 63% of requests (18 min)"
	if text != want {
		t.Errorf("resolved SMS = %q, want %q", text, want)
	}
}

func TestTelegramBlockFormat(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC, publicURL: "https://raedar.example.com", now: time.Now()}
	incident := store.Incident{
		ID: 812, Rule: "route.errors", Subject: "shop@docker", Severity: "warning",
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC), Summary: "5xx errors",
		Details: []byte(`{"rate":0.064,"baseline":0.002,"requests":1840,"errors":118,"path":"POST /api/checkout","app":"shop"}`),
	}
	text := f.text(group{kind: "opened", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	for _, want := range []string{
		"🟠 host-1 · shop — 5xx errors",
		"6.4% of requests fail (usual at this hour: 0.2%)",
		"since 14:32",
		"most errors: POST /api/checkout",
		"https://raedar.example.com/incidents/812",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to contain %q", text, want)
		}
	}
	resolvedAt := incident.OpenedAt.Add(18 * time.Minute)
	incident.ResolvedAt = &resolvedAt
	text = f.text(group{kind: "resolved", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	if !strings.HasPrefix(text, "✅ host-1 · shop — 5xx errors resolved after 18 min") {
		t.Errorf("resolved text = %q, want the resolved line", text)
	}
}

func TestWebhookDocument(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC, publicURL: "https://raedar.example.com",
		now: time.Date(2026, 10, 19, 14, 34, 15, 0, time.UTC)}
	incident := store.Incident{
		ID: 812, Rule: "route.errors", Subject: "shop@docker", Severity: "warning",
		OpenedAt: time.Date(2026, 10, 19, 14, 32, 0, 0, time.UTC), Summary: "5xx errors",
		Details: []byte(`{"rate":0.064,"baseline":0.002,"requests":1840,"errors":118,"app":"shop"}`),
	}
	document := f.document(group{kind: "opened", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	var body struct {
		Version   int    `json:"version"`
		Host      string `json:"host"`
		Kind      string `json:"kind"`
		SentAt    string `json:"sent_at"`
		URL       string `json:"url"`
		Incidents []struct {
			ID       int64          `json:"id"`
			Rule     string         `json:"rule"`
			Subject  string         `json:"subject"`
			App      string         `json:"app"`
			Severity string         `json:"severity"`
			OpenedAt string         `json:"opened_at"`
			Reopened bool           `json:"reopened"`
			Title    string         `json:"title"`
			Summary  string         `json:"summary"`
			Context  []string       `json:"context"`
			Details  map[string]any `json:"details"`
		} `json:"incidents"`
	}
	if err := json.Unmarshal(document, &body); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	if body.Version != 1 || body.Host != "host-1" || body.Kind != "incident.opened" {
		t.Errorf("document = %+v, want version 1 from host-1 of kind incident.opened", body)
	}
	if body.SentAt != "2026-10-19T14:34:15Z" {
		t.Errorf("sent_at = %q, want the formatter's time", body.SentAt)
	}
	if body.URL != "https://raedar.example.com/incidents/812" {
		t.Errorf("url = %q, want the incident link", body.URL)
	}
	if len(body.Incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(body.Incidents))
	}
	entry := body.Incidents[0]
	if entry.ID != 812 || entry.Rule != "route.errors" || entry.Subject != "shop@docker" || entry.App != "shop" {
		t.Errorf("incident = %+v, want the documented fields", entry)
	}
	if entry.Title != "5xx errors" || entry.Summary != "6.4% of requests fail (usual at this hour: 0.2%)" {
		t.Errorf("incident = %+v, want the title and summary", entry)
	}
	if entry.Details["requests"] != float64(1840) {
		t.Errorf("details = %v, want the rule's details", entry.Details)
	}
}

// fakeChannel records the messages it is sent.
type fakeChannel struct {
	kind  Kind
	mu    sync.Mutex
	sent  []Message
	fails int
}

func (c *fakeChannel) Kind() Kind { return c.kind }

// ChatIDs returns an empty slice; the test channel does not fan out.
func (c *fakeChannel) ChatIDs() []string { return nil }

func (c *fakeChannel) Send(ctx context.Context, message Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fails > 0 {
		c.fails--
		return errFakeSend
	}
	c.sent = append(c.sent, message)
	return nil
}

// messages returns the sent messages.
func (c *fakeChannel) messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.sent...)
}

// errFakeSend is the failure of the fake channel.
var errFakeSend = &fakeError{}

// fakeError is a delivery failure.
type fakeError struct{}

func (e *fakeError) Error() string { return "fake send failure" }

// fakeStore records deliveries and events.
type fakeStore struct {
	mu         sync.Mutex
	deliveries map[int64]store.Delivery
	nextID     int64
	events     map[store.EventKind][]store.Event
	silences   []store.Silence
	incidents  []store.Incident
}

func (s *fakeStore) AddDelivery(ctx context.Context, delivery store.Delivery) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deliveries == nil {
		s.deliveries = make(map[int64]store.Delivery)
	}
	s.nextID++
	delivery.ID = s.nextID
	s.deliveries[s.nextID] = delivery
	return s.nextID, nil
}

func (s *fakeStore) UpdateDelivery(ctx context.Context, delivery store.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.deliveries[delivery.ID]
	current.SentAt = delivery.SentAt
	current.Attempts = delivery.Attempts
	current.Status = delivery.Status
	current.Error = delivery.Error
	s.deliveries[delivery.ID] = current
	return nil
}

func (s *fakeStore) Deliveries(ctx context.Context, incidentID int64) ([]store.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deliveries []store.Delivery
	for _, delivery := range s.deliveries {
		if delivery.IncidentID != nil && *delivery.IncidentID == incidentID {
			deliveries = append(deliveries, delivery)
		}
	}
	return deliveries, nil
}

func (s *fakeStore) Events(ctx context.Context, kind store.EventKind, from, to time.Time) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[kind], nil
}

func (s *fakeStore) FindActiveSilence(ctx context.Context, rule, subject string, appID *int64, now time.Time) (*store.Silence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return matchSilenceLocal(s.silences, rule, subject, appID), nil
}

func (s *fakeStore) IncidentByID(ctx context.Context, id int64) (*store.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, incident := range s.incidents {
		if incident.ID == id {
			copy := incident
			return &copy, nil
		}
	}
	return nil, nil
}

func (s *fakeStore) LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deliveries {
		if d.Status != "sent" {
			continue
		}
		// We need to know the incident's (rule, subject) for matching.
		// Use a closed form: deliveries don't store rule/subject
		// directly, but the fakeStore has the incidents list.
		var inc *store.Incident
		if d.IncidentID != nil {
			for i, c := range s.incidents {
				if c.ID == *d.IncidentID {
					inc = &s.incidents[i]
					break
				}
			}
		}
		if inc == nil || inc.Rule != rule || inc.Subject != subject {
			continue
		}
		copy := d
		return &copy, nil
	}
	return nil, nil
}

func (s *fakeStore) OpenIncidentCount(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, inc := range s.incidents {
		if inc.ResolvedAt == nil {
			n++
		}
	}
	return n, nil
}

// byChannel returns the deliveries of one channel.
func (s *fakeStore) byChannel(channel Kind) []store.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deliveries []store.Delivery
	for _, delivery := range s.deliveries {
		if delivery.Channel == string(channel) {
			deliveries = append(deliveries, delivery)
		}
	}
	return deliveries
}

// newTestNotifier returns a notifier with fake channels.
func newTestNotifier(t *testing.T, channels ...Channel) (*Notifier, *fakeStore, *testClock) {
	t.Helper()
	cfg := testConfig(t)
	cfg.HostName = "host-1"
	cfg.PublicURL = "https://raedar.example.com"
	clock := &testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}
	st := &fakeStore{events: map[store.EventKind][]store.Event{}}
	notifier := &Notifier{
		cfg:       cfg,
		store:     st,
		logger:    discardLogger(),
		clock:     clock,
		channels:  map[Kind]Channel{},
		limiters:  map[Kind]*limiter{},
		gate:      &Gate{Store: st, Logger: discardLogger()},
		held:      map[Kind]int{},
		formatter: formatter{host: cfg.HostName, publicURL: cfg.PublicURL, location: time.UTC, now: clock.now},
	}
	for _, channel := range channels {
		notifier.channels[channel.Kind()] = channel
		notifier.limiters[channel.Kind()] = &limiter{limit: cfg.Notify.RateLimit, window: time.Hour}
	}
	return notifier, st, clock
}

// incident returns a test incident.
func incident(id int64, severity store.Severity) store.Incident {
	return store.Incident{
		ID: id, Rule: "route.errors", Subject: "shop@docker", Severity: severity,
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC), Summary: "5xx errors",
		Details: []byte(`{"rate":0.064,"app":"shop"}`),
	}
}

func TestNotifierGroupsAndRoutes(t *testing.T) {
	warning := &fakeChannel{kind: KindTelegram}
	critical := &fakeChannel{kind: KindSMS}
	notifier, st, _ := newTestNotifier(t, warning, critical)
	notifier.cfg.Notify.WarningChannels = []string{"telegram"}
	notifier.cfg.Notify.CriticalChannels = []string{"telegram", "sms"}
	notifier.IncidentsChanged(context.Background(), []rules.Change{
		{Incident: incident(1, "warning"), Kind: "opened"},
		{Incident: incident(2, "critical"), Kind: "opened"},
	})
	if got := len(warning.messages()); got != 1 {
		t.Fatalf("telegram messages = %d, want one group", got)
	}
	if got := len(critical.messages()); got != 1 {
		t.Fatalf("SMS messages = %d, want the critical incident", got)
	}
	if !strings.Contains(critical.messages()[0].SMS, "CRITICAL") {
		t.Errorf("SMS = %q, want the critical severity", critical.messages()[0].SMS)
	}
	if got := len(st.byChannel(KindTelegram)); got != 2 {
		t.Errorf("telegram deliveries = %d, want one per incident", got)
	}
}

func TestNotifierDeliversResolutionsToTheOpeningChannels(t *testing.T) {
	telegram := &fakeChannel{kind: KindTelegram}
	sms := &fakeChannel{kind: KindSMS}
	notifier, st, _ := newTestNotifier(t, telegram, sms)
	incidentID := int64(7)
	if _, err := st.AddDelivery(context.Background(), store.Delivery{
		IncidentID: &incidentID, Kind: "opened", Channel: "telegram", Status: "sent",
	}); err != nil {
		t.Fatalf("AddDelivery() error = %v", err)
	}
	resolved := incident(7, "warning")
	now := time.Now()
	resolved.ResolvedAt = &now
	notifier.IncidentsChanged(context.Background(), []rules.Change{{Incident: resolved, Kind: "resolved"}})
	if got := len(telegram.messages()); got != 1 {
		t.Errorf("telegram messages = %d, want the resolution", got)
	}
	if got := len(sms.messages()); got != 0 {
		t.Errorf("SMS messages = %d, want none for a resolution that never used SMS", got)
	}
}

func TestNotifierRateLimitHoldsAndSummarizes(t *testing.T) {
	telegram := &fakeChannel{kind: KindTelegram}
	notifier, _, clock := newTestNotifier(t, telegram)
	notifier.cfg.Notify.WarningChannels = []string{"telegram"}
	notifier.limiters[KindTelegram].limit = 1
	notifier.IncidentsChanged(context.Background(), []rules.Change{{Incident: incident(1, "warning"), Kind: "opened"}})
	notifier.IncidentsChanged(context.Background(), []rules.Change{{Incident: incident(2, "warning"), Kind: "opened"}})
	if got := len(telegram.messages()); got != 1 {
		t.Fatalf("messages = %d, want only the first", got)
	}
	// The held incident is summarized once the window allows it.
	clock.now = clock.now.Add(2 * time.Hour)
	notifier.deliverHeld(context.Background())
	messages := telegram.messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the held summary", len(messages))
	}
	if messages[1].Text != "1 more incidents, see Raedar" {
		t.Errorf("summary = %q, want the held message", messages[1].Text)
	}
}

func TestNotifierRetries(t *testing.T) {
	telegram := &fakeChannel{kind: KindTelegram, fails: 1}
	notifier, st, clock := newTestNotifier(t, telegram)
	notifier.cfg.Notify.WarningChannels = []string{"telegram"}
	notifier.IncidentsChanged(context.Background(), []rules.Change{{Incident: incident(1, "warning"), Kind: "opened"}})
	if got := len(telegram.messages()); got != 0 {
		t.Fatalf("messages = %d, want the first attempt to fail", got)
	}
	deliveries := st.byChannel(KindTelegram)
	if len(deliveries) != 1 || deliveries[0].Attempts != 1 || deliveries[0].Status != "pending" {
		t.Fatalf("delivery = %+v, want one pending attempt", deliveries)
	}
	// The retry happens after ten seconds.
	clock.now = clock.now.Add(5 * time.Second)
	notifier.deliverDue(context.Background())
	if got := len(telegram.messages()); got != 0 {
		t.Fatalf("messages = %d, want no retry before ten seconds", got)
	}
	clock.now = clock.now.Add(5 * time.Second)
	notifier.deliverDue(context.Background())
	if got := len(telegram.messages()); got != 1 {
		t.Fatalf("messages = %d, want the retry", got)
	}
	deliveries = st.byChannel(KindTelegram)
	if deliveries[0].Attempts != 2 || deliveries[0].Status != "sent" || deliveries[0].SentAt == nil {
		t.Errorf("delivery = %+v, want a sent second attempt", deliveries[0])
	}
}

func TestTestSendsToEveryConfiguredChannel(t *testing.T) {
	telegram := newServer(t, http.StatusOK, `{"ok":true}`)
	srv := newServer(t, http.StatusOK, "ok")
	cfg := testConfig(t)
	cfg.HostName = "host-1"
	cfg.Notify.Telegram.Token = "123:token"
	cfg.Notify.Telegram.ChatIDs = []string{"42"}
	cfg.Notify.Telegram.APIURL = telegram.URL
	cfg.Notify.Webhook.URL = srv.URL
	clock := &testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}
	results, err := Test(context.Background(), cfg, discardLogger(), clock, "")
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want telegram and webhook", len(results))
	}
	for _, result := range results {
		if result.Err != nil {
			t.Errorf("channel %s error = %v", result.Channel, result.Err)
		}
	}
	// The webhook test body carries the test kind.
	requests := srv.received()
	if len(requests) != 1 {
		t.Fatalf("webhook requests = %d, want 1", len(requests))
	}
	if !strings.Contains(requests[0].body, `"kind":"test"`) {
		t.Errorf("body = %q, want the test kind", requests[0].body)
	}
	if _, err := Test(context.Background(), cfg, discardLogger(), clock, "sms"); err == nil {
		t.Error("Test(sms) error = nil, want the unconfigured channel error")
	}
}

func TestDatabaseNotificationIsSentFromMemory(t *testing.T) {
	critical := &fakeChannel{kind: KindTelegram}
	notifier, st, _ := newTestNotifier(t, critical)
	notifier.cfg.Notify.WarningChannels = []string{"telegram"}
	notifier.cfg.Notify.CriticalChannels = []string{"telegram"}
	notifier.DatabaseDown(context.Background(), time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC))
	notifier.DatabaseUp(context.Background())
	messages := critical.messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the outage and the recovery", len(messages))
	}
	if !strings.Contains(messages[0].Text, "database writes are failing") {
		t.Errorf("outage message = %q, want the database outage", messages[0].Text)
	}
	if !strings.Contains(messages[1].Text, "resolved") {
		t.Errorf("recovery message = %q, want a resolution", messages[1].Text)
	}
	if deliveries := st.byChannel(KindTelegram); len(deliveries) != 0 {
		t.Errorf("deliveries = %+v, want none: the database cannot store them", deliveries)
	}
}

func TestProxyScopePerChannel(t *testing.T) {
	var (
		mu       sync.Mutex
		proxied  []string
		directed int
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	sms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		directed++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer sms.Close()
	smsURL, err := url.Parse(sms.URL)
	if err != nil {
		t.Fatalf("parse SMS URL: %v", err)
	}
	newConfig := func(scope ...string) *config.Config {
		cfg := testConfig(t)
		cfg.HostName = "host-1"
		cfg.Notify.OutboundProxy = proxy.URL
		cfg.Notify.ProxyFor = scope
		cfg.Notify.Telegram.Token = "123:token"
		cfg.Notify.Telegram.ChatIDs = []string{"42"}
		cfg.Notify.Telegram.APIURL = "http://telegram.invalid"
		cfg.Notify.SMS.HTTP.URL = sms.URL + "/send"
		cfg.Notify.SMS.HTTP.Method = http.MethodPost
		cfg.Notify.SMS.To = []string{"+989120000000"}
		cfg.Notify.SMS.HTTP.Encoding = "json"
		cfg.Notify.SMS.HTTP.Body = `{"to":"{{to}}","text":"{{message}}"}`
		return cfg
	}
	clock := &testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}

	// Telegram through the proxy, SMS direct.
	results, err := Test(context.Background(), newConfig(config.ProxyTargetTelegram), discardLogger(), clock, "")
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	for _, result := range results {
		if result.Err != nil {
			t.Errorf("channel %s error = %v", result.Channel, result.Err)
		}
	}
	mu.Lock()
	if len(proxied) != 1 || proxied[0] != "telegram.invalid" {
		t.Errorf("proxied hosts = %v, want only Telegram", proxied)
	}
	if directed != 1 {
		t.Errorf("direct requests = %d, want the SMS request", directed)
	}
	proxied, directed = nil, 0
	mu.Unlock()

	// The reverse: SMS through the proxy, Telegram direct (and therefore
	// unreachable).
	results, err = Test(context.Background(), newConfig(config.ProxyTargetSMS), discardLogger(), clock, "")
	if err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	mu.Lock()
	if directed != 0 {
		t.Errorf("direct requests = %d, want none", directed)
	}
	if len(proxied) != 1 || proxied[0] != smsURL.Host {
		t.Errorf("proxied hosts = %v, want the SMS endpoint %s", proxied, smsURL.Host)
	}
	mu.Unlock()
	failed := false
	for _, result := range results {
		if result.Channel == KindTelegram && result.Err != nil {
			failed = true
		}
	}
	if !failed {
		t.Error("the direct Telegram request did not fail, want it to bypass the proxy")
	}
}

func TestSendDigestRecordsADeliveryWithoutAnIncident(t *testing.T) {
	telegram := &fakeChannel{kind: KindTelegram}
	notifier, st, _ := newTestNotifier(t, telegram)
	notifier.cfg.Notify.DigestChannels = []string{"telegram"}
	notifier.SendDigest(context.Background(), "all quiet")
	if got := len(telegram.messages()); got != 1 {
		t.Fatalf("telegram messages = %d, want the digest", got)
	}
	deliveries := st.byChannel(KindTelegram)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want one for the digest", len(deliveries))
	}
	if deliveries[0].Kind != store.DeliveryDigest || deliveries[0].IncidentID != nil || deliveries[0].Status != "sent" {
		t.Errorf("delivery = %+v, want a sent digest that belongs to no incident", deliveries[0])
	}
}

func TestEscalationIsRecordedAsItsOwnDeliveryKind(t *testing.T) {
	telegram := &fakeChannel{kind: KindTelegram}
	notifier, st, _ := newTestNotifier(t, telegram)
	notifier.cfg.Notify.WarningChannels = []string{"telegram"}
	notifier.cfg.Notify.CriticalChannels = []string{"telegram"}
	notifier.IncidentsChanged(context.Background(), []rules.Change{
		{Incident: incident(3, store.SeverityCritical), Kind: rules.ChangeEscalated},
	})
	deliveries := st.byChannel(KindTelegram)
	if len(deliveries) != 1 || deliveries[0].Kind != store.DeliveryEscalated {
		t.Errorf("deliveries = %+v, want one escalated delivery", deliveries)
	}
}
