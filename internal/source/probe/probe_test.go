package probe

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

func route(rule string, domains []string, app *discovery.App) *discovery.Route {
	return &discovery.Route{ID: 1, Name: "shop@docker", Kind: "http", Rule: rule, Domains: domains, TLS: true, App: app}
}

func TestPlan(t *testing.T) {
	app := &discovery.App{Name: "shop"}
	tests := []struct {
		name       string
		route      *discovery.Route
		wantHost   string
		wantPath   string
		wantReason string
	}{
		{name: "plain host", route: route("Host(`shop.example.com`)", []string{"shop.example.com"}, app), wantHost: "shop.example.com", wantPath: "/"},
		{name: "path prefix from the rule", route: route("Host(`shop.example.com`) && PathPrefix(`/api`)", []string{"shop.example.com"}, app), wantHost: "shop.example.com", wantPath: "/api"},
		{name: "label path wins", route: route("Host(`a.example.com`)", []string{"a.example.com"}, &discovery.App{Name: "a", ProbePath: "/healthz"}), wantHost: "a.example.com", wantPath: "/healthz"},
		{name: "header matcher cannot be satisfied", route: route("Host(`a.example.com`) && Header(`X-Tenant`, `1`)", []string{"a.example.com"}, app), wantReason: "header"},
		{name: "no exact hostname", route: route("HostRegexp(`.+`)", nil, app), wantReason: "hostname"},
		{name: "wildcard host", route: route("Host(`*.example.com`)", []string{"*.example.com"}, app), wantReason: "hostname"},
		{name: "opted out", route: route("Host(`a.example.com`)", []string{"a.example.com"}, &discovery.App{Name: "a", ProbeOff: true}), wantReason: "raedar.probe=false"},
		{name: "no TLS", route: &discovery.Route{ID: 1, Name: "x@docker", Kind: "http", Domains: []string{"a.example.com"}, App: app}, wantReason: "TLS"},
		{name: "tcp", route: &discovery.Route{ID: 1, Name: "x@docker", Kind: "tcp", TLS: true, App: app}, wantReason: "HTTP"},
		{name: "bad path label", route: route("Host(`a.example.com`)", []string{"a.example.com"}, &discovery.App{Name: "a", ProbePath: "//evil.example"}), wantReason: "invalid probe path"},
		{name: "bad expectation", route: route("Host(`a.example.com`)", []string{"a.example.com"}, &discovery.App{Name: "a", ProbeExpect: "ok"}), wantReason: "raedar.probe.expect"},
		{name: "catch-all", route: &discovery.Route{ID: 1, Name: "fallback@file", Kind: "http", TLS: true, Domains: []string{"a.example.com"}, App: app}, wantReason: "catch-all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, reason := Plan(tt.route, "/")
			if tt.wantReason != "" {
				if !strings.Contains(reason, tt.wantReason) {
					t.Fatalf("reason = %q, want it to contain %q", reason, tt.wantReason)
				}
				return
			}
			if reason != "" || target.Host != tt.wantHost || target.Path != tt.wantPath {
				t.Errorf("Plan() = %+v, %q; want %s%s", target, reason, tt.wantHost, tt.wantPath)
			}
		})
	}
}

func TestExpectation(t *testing.T) {
	if !DefaultExpectation.Accepts(401) || !DefaultExpectation.Accepts(302) || DefaultExpectation.Accepts(500) {
		t.Error("the default must accept protected routes (401/403) and redirects but not 5xx")
	}
	expect, err := ParseExpectation("200, 301-302")
	if err != nil {
		t.Fatalf("ParseExpectation() error = %v", err)
	}
	if !expect.Accepts(200) || !expect.Accepts(302) || expect.Accepts(404) {
		t.Error("explicit expectation not applied")
	}
	for _, bad := range []string{"", "x", "99", "600", "300-200"} {
		if _, err := ParseExpectation(bad); err == nil {
			t.Errorf("ParseExpectation(%q) accepted", bad)
		}
	}
}

func TestAdvanceDebouncesFailuresAndRecovery(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	good := Outcome{Result: store.ProbeHealthy, Status: 200}
	bad := Outcome{Result: store.ProbeUnreachable, Error: "timeout"}
	var state *store.ProbeState
	step := func(outcome Outcome) string {
		now = now.Add(time.Minute)
		next := Advance(state, 1, "shop.example.com/", outcome, now)
		state = &next
		return next.State
	}
	// Too little evidence is unknown, not healthy.
	if got := step(bad); got != store.ProbeUnknown {
		t.Errorf("one failure on a new route = %s, want unknown", got)
	}
	if got := step(good); got != store.ProbeHealthy {
		t.Errorf("first success = %s, want healthy", got)
	}
	for i := 0; i < failAfter-1; i++ {
		if got := step(bad); got != store.ProbeHealthy {
			t.Fatalf("failure %d = %s, want healthy until %d in a row", i+1, got, failAfter)
		}
	}
	since := state.Since
	if got := step(bad); got != store.ProbeUnreachable {
		t.Errorf("failure %d = %s, want unreachable", failAfter, got)
	}
	if !state.Since.After(since) {
		t.Error("since must move when the route turns unreachable")
	}
	if got := step(Outcome{Result: store.ProbeUnhealthy, Status: 503}); got != store.ProbeUnhealthy {
		t.Errorf("a 503 while failing = %s, want unhealthy", got)
	}
	failingSince := state.Since
	if got := step(good); got != store.ProbeUnhealthy {
		t.Errorf("one success while failing = %s, want it to stay failing", got)
	}
	if got := step(good); got != store.ProbeHealthy {
		t.Errorf("%d successes = %s, want healthy", recoverAfter, got)
	}
	if !state.Since.After(failingSince) {
		t.Error("since must move on recovery")
	}
}

// fakeTraefik serves one route over TLS and returns the client and address.
func fakeTraefik(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return NewClient(server.Listener.Addr().String(), "raedar-test", 2*time.Second, roots), server
}

func TestCheckClassifiesAnswers(t *testing.T) {
	var hostSeen, agentSeen, authSeen atomic.Value
	client, _ := fakeTraefik(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostSeen.Store(r.Host)
		agentSeen.Store(r.UserAgent())
		authSeen.Store(r.Header.Get("Authorization") + r.Header.Get("Cookie"))
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/protected":
			w.WriteHeader(http.StatusUnauthorized)
		case "/redirect":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		case "/slow":
			time.Sleep(3 * time.Second)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	check := func(path string) Outcome {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return client.Check(ctx, Target{Host: "127.0.0.1", Path: path, Expect: DefaultExpectation})
	}
	if got := check("/ok"); got.Result != store.ProbeHealthy || got.Status != 200 {
		t.Errorf("/ok = %+v, want healthy", got)
	}
	if got := check("/protected"); got.Result != store.ProbeHealthy || got.Status != 401 {
		t.Errorf("a protected route = %+v, want healthy (alive)", got)
	}
	if got := check("/boom"); got.Result != store.ProbeUnhealthy || got.Status != 500 {
		t.Errorf("/boom = %+v, want unhealthy", got)
	}
	// A redirect is reported as it is and never followed to its target.
	if got := check("/redirect"); got.Result != store.ProbeHealthy || got.Status != 302 {
		t.Errorf("/redirect = %+v, want the 302 itself", got)
	}
	if got := check("/slow"); got.Result != store.ProbeUnreachable || got.Error != "timeout" {
		t.Errorf("/slow = %+v, want an unreachable timeout", got)
	}
	if agentSeen.Load() != "raedar-test" || authSeen.Load() != "" {
		t.Errorf("checks must send only a user agent: agent=%v credentials=%q", agentSeen.Load(), authSeen.Load())
	}
}

func TestCheckConnectsOnlyToTheConfiguredAddress(t *testing.T) {
	client, server := fakeTraefik(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// The hostname is unresolvable; the check still reaches the fixed address,
	// and fails certificate verification there instead of resolving anything.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := client.Check(ctx, Target{Host: "shop.invalid", Path: "/", Expect: DefaultExpectation})
	if got.Result != store.ProbeUnreachable || got.Error != "TLS certificate not accepted" {
		t.Errorf("Check() = %+v, want a certificate failure from the fixed address", got)
	}
	server.Close()
	got = client.Check(ctx, Target{Host: "127.0.0.1", Path: "/", Expect: DefaultExpectation})
	if got.Result != store.ProbeUnreachable {
		t.Errorf("Check() against a closed port = %+v, want unreachable", got)
	}
}
