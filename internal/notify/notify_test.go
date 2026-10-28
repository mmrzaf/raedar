package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func TestTelegramChannelClassifiesRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.PostFormValue("chat_id") {
		case "blocked":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`)
		case "unavailable":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
		}
	}))
	defer srv.Close()
	channel := &telegramChannel{
		apiURL:  srv.URL,
		token:   "123:secret-token",
		chatIDs: []string{"blocked", "unavailable", "open"},
		client:  srv.Client(),
	}
	results := channel.SendEach(context.Background(), Message{Text: "hello"}, nil)
	if len(results) != 3 {
		t.Fatalf("results = %d, want one per chat", len(results))
	}
	var refused *refusedError
	if !errors.As(results[0].err, &refused) {
		t.Errorf("blocked chat error = %v, want a refusal", results[0].err)
	}
	if errors.As(results[1].err, &refused) || results[1].err == nil {
		t.Errorf("unavailable chat error = %v, want a retryable failure", results[1].err)
	}
	if results[2].err != nil || results[2].messageID != 7 {
		t.Errorf("open chat result = %+v, want the message id", results[2])
	}
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
	text = f.text(group{kind: "opened", incidents: []store.Incident{incident}, reopened: map[int64]bool{812: true}})
	if !strings.Contains(text, "5xx errors (again)") {
		t.Errorf("reopened text = %q, want the again marker", text)
	}
	resolvedAt := incident.OpenedAt.Add(18 * time.Minute)
	incident.ResolvedAt = &resolvedAt
	text = f.text(group{kind: "resolved", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	if !strings.HasPrefix(text, "✅ host-1 · shop — 5xx errors resolved after 18 min") {
		t.Errorf("resolved text = %q, want the resolved line", text)
	}
}

func TestTelegramRouteDownFormat(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC, now: time.Now()}
	incident := store.Incident{
		ID: 1, Rule: "route.down", Subject: "shop@docker", Severity: "critical",
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC), Summary: "no healthy backend",
		Details: []byte(`{"requests":20,"no_backend":20,"domains":["shop.example.com"],"app":"shop"}`),
	}
	text := f.text(group{kind: "opened", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	for _, want := range []string{
		"🔴 host-1 · shop — no healthy backend",
		"shop.example.com: no backend for 100% of requests",
		"action: check the container behind this route",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to contain %q", text, want)
		}
	}
}

func TestTelegramCertInvalidGuidance(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC, now: time.Now()}
	incident := store.Incident{
		ID: 1, Rule: "cert.invalid", Subject: "www.example.com", Severity: "warning",
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC),
		Summary:  "Traefik serves its default certificate",
		Details:  []byte(`{"status":"default"}`),
	}
	text := f.text(group{kind: "opened", incidents: []store.Incident{incident}, reopened: map[int64]bool{}})
	for _, want := range []string{
		"Traefik serves its default certificate",
		"If this host is behind a proxy, this may be expected.",
		"action: add a certificate, or silence this domain if it is proxied",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to contain %q", text, want)
		}
	}
}

func TestDefaultSubjectName(t *testing.T) {
	f := formatter{host: "host-1", location: time.UTC, now: time.Now()}
	incident := store.Incident{
		ID: 1, Rule: "route.down", Subject: "fallback@file", Severity: "warning",
		OpenedAt: time.Now(), Summary: "no healthy backend",
	}
	if got := f.subjectName(incident); got != "Unrouted traffic" {
		t.Errorf("subjectName() = %q, want %q", got, "Unrouted traffic")
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
