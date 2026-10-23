package rules

import (
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/source/tlscheck"
	"github.com/mmrzaf/raedar/internal/store"
)

func testEngine(rules config.RulesConfig) *Engine {
	return &Engine{cfg: &config.Config{Rules: rules}}
}

func testSnapshot(routes ...*discovery.Route) *snapshot {
	s := &snapshot{
		routes:     make(map[int64]*routeInfo),
		windows:    make(map[time.Duration]map[int64]*store.RouteWindow),
		baselines:  make(map[int64]store.Baseline),
		pathErrors: make(map[int64]pathTop),
		pathSlow:   make(map[int64]pathTop),
	}
	for _, route := range routes {
		s.routes[route.ID] = &routeInfo{
			route:   route,
			app:     route.App,
			routeID: route.ID,
		}
	}
	return s
}

func testRoute(id int64, name string, domains ...string) *discovery.Route {
	return &discovery.Route{ID: id, Name: name, Domains: domains}
}

func TestRouteReadingsSkipsCatchAllAndIgnoredRoutes(t *testing.T) {
	app := &discovery.App{Name: "shop", Alerts: true}
	shop := testRoute(2, "shop@docker", "shop.example.com")
	shop.App = app
	s := testSnapshot(testRoute(1, "fallback@file"), testRoute(3, discovery.UnroutedRoute), shop)
	engine := testEngine(config.RulesConfig{})
	readings := engine.routeReadings(time.Now(), s)
	for _, reading := range readings {
		if reading.subject == "fallback@file" || reading.subject == discovery.UnroutedRoute {
			t.Errorf("route %q produced a reading", reading.subject)
		}
	}
	if len(readings) == 0 {
		t.Fatal("shop route produced no readings")
	}

	ignored := testEngine(config.RulesConfig{RouteIgnore: []string{"shop@*"}})
	if readings := ignored.routeReadings(time.Now(), s); len(readings) != 0 {
		t.Errorf("ignored route produced %d readings", len(readings))
	}
}

func TestRouteSlowIgnoresLongLivedTraffic(t *testing.T) {
	app := &discovery.App{Name: "gitman", Alerts: true}
	route := testRoute(1, "gitman@docker", "git.example.com")
	route.App = app
	s := testSnapshot(route)
	var latency store.Histogram
	for i := 0; i < 90; i++ {
		latency.Add(20)
	}
	for i := 0; i < 10; i++ {
		latency.Add(30000)
	}
	s.windows[routeSlowWindow] = map[int64]*store.RouteWindow{
		1: {RouteID: 1, Requests: 100, Latency: latency},
	}
	engine := testEngine(config.RulesConfig{
		SlowMinRequests:  1,
		SlowMinP95:       time.Millisecond,
		SlowFactor:       1,
		LongRequest:      10 * time.Second,
		LongRequestShare: 5,
	})
	got := engine.routeSlowReading(s.routes[1], s, time.Millisecond, store.SeverityWarning, false)
	if got.firing {
		t.Fatal("long-lived route fired route.slow")
	}
	if longLived, _ := got.details["long_lived"].(bool); !longLived {
		t.Errorf("details long_lived = %v, want true", got.details["long_lived"])
	}
}

func TestCertReadingsSkipRemovedRoutes(t *testing.T) {
	routeID := int64(1)
	engine := testEngine(config.RulesConfig{})
	s := testSnapshot()
	s.certs = []store.Cert{{
		Domain:    "old.example.com",
		RouteID:   &routeID,
		Status:    tlscheck.StatusDefault,
		CheckedAt: time.Now(),
	}}
	if readings := engine.certReadings(time.Now(), s); len(readings) != 0 {
		t.Fatalf("removed route produced %d certificate readings", len(readings))
	}

	s.routes[routeID] = &routeInfo{route: testRoute(routeID, "old@docker", "old.example.com"), routeID: routeID}
	if readings := engine.certReadings(time.Now(), s); len(readings) != 2 {
		t.Fatalf("live route produced %d certificate readings, want 2", len(readings))
	}
}
