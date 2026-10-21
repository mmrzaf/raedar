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

func (s *fakeStore) FailPendingDeliveries(ctx context.Context, reason string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for id, d := range s.deliveries {
		if d.Status == "pending" {
			d.Status, d.Error = "failed", &reason
			s.deliveries[id] = d
			count++
		}
	}
	return count, nil
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
		cfg:         cfg,
		store:       st,
		logger:      discardLogger(),
		clock:       clock,
		channels:    map[Kind]Channel{},
		limiters:    map[Kind]*limiter{},
		gate:        &Gate{Store: st, Logger: discardLogger()},
		held:        map[Kind]int{},
		heldRetryAt: map[Kind]time.Time{},
		deferred:    map[string]*deferredPage{},
		formatter:   formatter{host: cfg.HostName, publicURL: cfg.PublicURL, location: time.UTC, now: clock.now},
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

// TestDeliverDeferredSendsCatchUp verifies that pages held by the gate are
// delivered as one catch-up message once their ready time passes.
func TestDeliverDeferredSendsCatchUp(t *testing.T) {
	notifier, st, cl := newTestNotifier(t, &fakeChannel{kind: KindTelegram})
	notifier.cfg.Notify.Silent.StandDownAfter = 0
	incident := incident(1, store.SeverityWarning)
	incident.Rule = "host.disk"
	incident.Subject = "/"
	st.incidents = append(st.incidents, incident)
	channel := notifier.channels[KindTelegram].(*fakeChannel)

	notifier.deferIncident(channel, store.DeliveryOpened, incident, Decision{
		Status: GateHeld, ReadyAt: cl.Now().Add(-time.Second),
	})
	notifier.deliverDeferred(context.Background())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want one catch-up message", got)
	}
	if text := channel.messages()[0].Text; !strings.Contains(text, "held while quiet") {
		t.Fatalf("message = %q, want the quiet-hours header", text)
	}
	// A second call has nothing left to send.
	notifier.deliverDeferred(context.Background())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after second call = %d, want still 1", got)
	}
}

// TestMaybeStandDownSendsOnce verifies the all-quiet notice is sent once
// per quiet period and never while an incident is open.
func TestMaybeStandDownSendsOnce(t *testing.T) {
	notifier, _, cl := newTestNotifier(t, &fakeChannel{kind: KindTelegram})
	notifier.cfg.Notify.Silent.StandDownAfter = time.Hour
	notifier.cfg.Notify.DigestChannels = []string{"telegram"}
	channel := notifier.channels[KindTelegram].(*fakeChannel)

	notifier.mu.Lock()
	notifier.lastPageAt = cl.Now().Add(-2 * time.Hour)
	notifier.mu.Unlock()

	notifier.maybeStandDown(context.Background())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want one all-quiet notice", got)
	}
	if text := channel.messages()[0].Text; !strings.Contains(text, "all quiet") {
		t.Fatalf("message = %q, want the all-quiet text", text)
	}
	notifier.maybeStandDown(context.Background())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after second call = %d, want still 1", got)
	}
}

// TestHTTPSMSAPIKeyPlaceholder verifies {{api_key}} is substituted in the
// URL and the body and is never left in the request.
func TestHTTPSMSAPIKeyPlaceholder(t *testing.T) {
	srv := newServer(t, http.StatusOK, "ok")
	channel := &httpSMSChannel{
		url:      srv.URL + "/v1/{{api_key}}/sms?to={{to}}",
		method:   http.MethodPost,
		body:     `{"key":"{{api_key}}","text":"{{message}}"}`,
		encoding: "json",
		apiKey:   "secret-key",
		to:       []string{"+989120000000"},
		client:   srv.Client(),
	}
	if err := channel.Send(context.Background(), Message{SMS: "hello"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	request := srv.received()[0]
	if request.path != "/v1/secret-key/sms" {
		t.Fatalf("path = %q, want the key substituted", request.path)
	}
	if !strings.Contains(request.body, `"key":"secret-key"`) {
		t.Fatalf("body = %q, want the key substituted", request.body)
	}
}

// TestSubjectNamesInMessages verifies the configured display name replaces
// the raw subject in Telegram text.
func TestSubjectNamesInMessages(t *testing.T) {
	notifier, _, _ := newTestNotifier(t, &fakeChannel{kind: KindTelegram})
	notifier.formatter.names = func(subject string) string {
		if subject == "fallback@file" {
			return "Unrouted traffic"
		}
		return subject
	}
	incident := incident(1, store.SeverityWarning)
	incident.Rule = "route.down"
	incident.Subject = "fallback@file"
	incident.Details = []byte(`{}`)
	g := group{kind: store.DeliveryOpened, incidents: []store.Incident{incident}, reopened: map[int64]bool{}}
	if text := notifier.formatter.text(g); !strings.Contains(text, "Unrouted traffic") {
		t.Fatalf("text = %q, want the configured display name", text)
	}
}

// TestDeliverDeferredRechecksTheGate verifies that a held page is judged again
// when it comes due: an incident that was acked, silenced or resolved while
// waiting must not produce a catch-up message.
func TestDeliverDeferredRechecksTheGate(t *testing.T) {
	ackedAt := time.Date(2026, 10, 18, 15, 0, 0, 0, time.UTC)
	resolvedAt := ackedAt
	cases := map[string]struct {
		change   func(*store.Incident)
		silence  bool
		wantSent bool
		wantRows string
	}{
		"still failing": {change: func(*store.Incident) {}, wantSent: true},
		"acked":         {change: func(i *store.Incident) { i.AckedAt = &ackedAt }, wantRows: "acked"},
		"resolved":      {change: func(i *store.Incident) { i.ResolvedAt = &resolvedAt }},
		"silenced":      {change: func(*store.Incident) {}, silence: true, wantRows: "silenced"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			channel := &fakeChannel{kind: KindTelegram}
			notifier, st, cl := newTestNotifier(t, channel)
			queued := incident(1, store.SeverityWarning)
			current := queued
			tc.change(&current)
			st.incidents = append(st.incidents, current)
			if tc.silence {
				st.silences = append(st.silences, store.Silence{
					Kind: store.SilenceOneShotMute, Rule: &queued.Rule, Subject: &queued.Subject,
					Reason: "noise", StartsAt: cl.Now().Add(-time.Hour), EndsAt: cl.Now().Add(time.Hour),
				})
			}
			notifier.deferIncident(channel, store.DeliveryOpened, queued, Decision{
				Status: GateHeld, ReadyAt: cl.Now().Add(-time.Second),
			})
			notifier.deliverDeferred(context.Background())
			if got := len(channel.messages()) == 1; got != tc.wantSent {
				t.Fatalf("catch-up sent = %v, want %v", got, tc.wantSent)
			}
			if tc.wantRows != "" {
				var found bool
				for _, d := range st.byChannel(KindTelegram) {
					found = found || d.Status == tc.wantRows
				}
				if !found {
					t.Fatalf("no %q delivery row recorded", tc.wantRows)
				}
			}
		})
	}
}

// TestDeliverDeferredHoldsWhileQuietHoursLast verifies that a cooldown that
// expires inside the quiet window does not page: the page stays deferred.
func TestDeliverDeferredHoldsWhileQuietHoursLast(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, cl := newTestNotifier(t, channel)
	from := time.Date(0, 1, 1, 14, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 16, 0, 0, 0, time.UTC)
	notifier.gate.QuietFrom, notifier.gate.QuietTo, notifier.gate.QuietZone = &from, &to, time.UTC
	queued := incident(1, store.SeverityWarning)
	st.incidents = append(st.incidents, queued)
	notifier.deferIncident(channel, store.DeliveryOpened, queued, Decision{
		Status: GateCooldown, ReadyAt: cl.Now().Add(-time.Second),
	})
	notifier.deliverDeferred(context.Background())
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want none during the quiet window", got)
	}
	cl.now = time.Date(2026, 10, 18, 16, 1, 0, 0, time.UTC)
	notifier.deliverDeferred(context.Background())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after the window = %d, want the catch-up", got)
	}
}

// TestResolutionSkipsPagesThatNeverWentOut verifies that an incident whose
// opening page was held announced nothing, so its resolution is not sent.
func TestResolutionSkipsPagesThatNeverWentOut(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, _ := newTestNotifier(t, channel)
	id := int64(7)
	if _, err := st.AddDelivery(context.Background(), store.Delivery{
		IncidentID: &id, Kind: store.DeliveryOpened, Channel: "telegram", Status: "held",
	}); err != nil {
		t.Fatalf("AddDelivery: %v", err)
	}
	resolved := incident(7, store.SeverityWarning)
	now := time.Now()
	resolved.ResolvedAt = &now
	notifier.IncidentsChanged(context.Background(), []rules.Change{{Incident: resolved, Kind: rules.ChangeResolved}})
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want none for a resolution of an unannounced incident", got)
	}
}

// TestDigestDoesNotResetStandDown verifies that only incident pages count as
// activity for the stand-down notice.
func TestDigestDoesNotResetStandDown(t *testing.T) {
	notifier, _, cl := newTestNotifier(t, &fakeChannel{kind: KindTelegram})
	notifier.cfg.Notify.DigestChannels = []string{"telegram"}
	before := cl.Now().Add(-5 * time.Hour)
	notifier.mu.Lock()
	notifier.lastPageAt = before
	notifier.mu.Unlock()
	notifier.SendDigest(context.Background(), "daily digest")
	notifier.mu.Lock()
	got := notifier.lastPageAt
	notifier.mu.Unlock()
	if !got.Equal(before) {
		t.Fatalf("lastPageAt = %v after a digest, want it unchanged (%v)", got, before)
	}
	notifier.IncidentsChanged(context.Background(), []rules.Change{{Incident: incident(1, store.SeverityCritical), Kind: rules.ChangeOpened}})
	notifier.mu.Lock()
	got = notifier.lastPageAt
	notifier.mu.Unlock()
	if !got.Equal(cl.Now()) {
		t.Fatalf("lastPageAt = %v after a page, want %v", got, cl.Now())
	}
}

func TestQuietSpan(t *testing.T) {
	for window, want := range map[time.Duration]string{
		24 * time.Hour: "24 h", 90 * time.Minute: "90 min", 30 * time.Minute: "30 min", time.Hour: "1 h",
	} {
		if got := quietSpan(window); got != want {
			t.Errorf("quietSpan(%v) = %q, want %q", window, got, want)
		}
	}
}

// TestLimiterIsSafeForConcurrentUse is meant to run under -race: pages and the
// background loop share one limiter per channel.
func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	l := &limiter{limit: 1000, window: time.Hour}
	now := time.Now()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if l.allow(now) {
					l.undo()
				}
			}
		}()
	}
	wg.Wait()
}

// TestHTTPSMSRetriesOnlyFailedRecipients verifies that one failing number is
// reported on its own and does not stop the others.
func TestHTTPSMSRetriesOnlyFailedRecipients(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.Query().Get("to")
		mu.Lock()
		calls[to]++
		mu.Unlock()
		if to == "+2" {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	channel := &httpSMSChannel{
		url: srv.URL + "?to={{to}}", method: http.MethodGet, encoding: "raw",
		to: []string{"+1", "+2", "+3"}, client: srv.Client(),
	}
	results := channel.SendEach(context.Background(), Message{SMS: "hello"}, nil)
	failed := map[string]bool{}
	for _, result := range results {
		failed[result.chatID] = result.err != nil
	}
	if len(results) != 3 || failed["+1"] || !failed["+2"] || failed["+3"] {
		t.Fatalf("results = %+v, want only +2 failed and +3 still attempted", results)
	}
	retry := channel.SendEach(context.Background(), Message{SMS: "hello"}, []string{"+2"})
	if len(retry) != 1 || calls["+1"] != 1 || calls["+3"] != 1 || calls["+2"] != 2 {
		t.Fatalf("calls = %v, retry = %+v; want only +2 repeated", calls, retry)
	}
}

// TestHTTPSMSPlaceholdersAreSubstitutedOnce verifies that a message which
// itself contains a placeholder cannot pull the API key into the request.
func TestHTTPSMSPlaceholdersAreSubstitutedOnce(t *testing.T) {
	srv := newServer(t, http.StatusOK, "ok")
	channel := &httpSMSChannel{
		url: srv.URL + "/send", method: http.MethodPost, encoding: "json",
		body: `{"text":"{{message}}","key":"{{api_key}}"}`, apiKey: "secret-key",
		to: []string{"+1"}, client: srv.Client(),
	}
	if err := channel.Send(context.Background(), Message{SMS: "GET /{{api_key}}"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	body := srv.received()[0].body
	if !strings.Contains(body, `"text":"GET /{{api_key}}"`) || !strings.Contains(body, `"key":"secret-key"`) {
		t.Fatalf("body = %q, want the message kept literal and the key in its own field", body)
	}
}

// TestFailInterruptedClosesPendingDeliveries verifies that rows a previous run
// left pending are failed at startup, and finished rows are untouched.
func TestFailInterruptedClosesPendingDeliveries(t *testing.T) {
	notifier, st, _ := newTestNotifier(t, &fakeChannel{kind: KindTelegram})
	ctx := context.Background()
	id := int64(1)
	pending, _ := st.AddDelivery(ctx, store.Delivery{IncidentID: &id, Kind: store.DeliveryOpened, Channel: "telegram", Status: "pending"})
	sent, _ := st.AddDelivery(ctx, store.Delivery{IncidentID: &id, Kind: store.DeliveryOpened, Channel: "telegram", Status: "sent"})
	if err := notifier.FailInterrupted(ctx); err != nil {
		t.Fatalf("FailInterrupted: %v", err)
	}
	if got := st.deliveries[pending]; got.Status != "failed" || got.Error == nil {
		t.Fatalf("pending delivery = %+v, want failed with a reason", got)
	}
	if got := st.deliveries[sent]; got.Status != "sent" {
		t.Fatalf("sent delivery = %+v, want it untouched", got)
	}
}
