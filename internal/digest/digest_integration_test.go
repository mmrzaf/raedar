//go:build integration

package digest

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// testClock is a clock the tests set.
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

// testSchema creates a schema and returns a migrated store.
func testSchema(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	databaseURL, schema := testdb.NewSchema(t, "digest")
	schemaURL := testdb.WithSearchPath(t, databaseURL, schema)
	st, err := store.Open(ctx, schemaURL, discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return st
}

// testConfig returns the configuration with the documented defaults.
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
	cfg.HostName = "host-1"
	return cfg
}

func TestDigestAllQuiet(t *testing.T) {
	st := testSchema(t)
	clock := &testClock{now: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	text, err := New(st, testConfig(t), discardLogger(), clock).Text(context.Background())
	if err != nil {
		t.Fatalf("Text() error = %v", err)
	}
	if !strings.Contains(text, "All quiet.") {
		t.Errorf("digest = %q, want the all quiet headline", text)
	}
}

func TestDigestSummarizesThePreviousDay(t *testing.T) {
	st := testSchema(t)
	ctx := context.Background()
	clock := &testClock{now: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	cfg := testConfig(t)
	appID, err := st.UpsertApp(ctx, "shop", clock.now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	routeID, err := st.UpsertRoute(ctx, store.Route{Name: "shop@docker", AppID: &appID, Kind: "http", Origin: "labels"}, clock.now)
	if err != nil {
		t.Fatalf("UpsertRoute() error = %v", err)
	}
	// An incident that started and resolved in the last day.
	resolvedAt := clock.now.Add(-2 * time.Hour)
	incident := store.Incident{
		Rule: "route.errors", Subject: "shop@docker", AppID: &appID, RouteID: &routeID, Severity: "warning",
		OpenedAt: clock.now.Add(-3 * time.Hour), ResolvedAt: &resolvedAt, Summary: "5xx errors",
		Details: []byte(`{"app":"shop"}`),
	}
	if _, err := st.InsertIncident(ctx, incident); err != nil {
		t.Fatalf("InsertIncident() error = %v", err)
	}
	if _, err := st.InsertIncident(ctx, store.Incident{
		Rule: "route.slow", Subject: "shop@docker", AppID: &appID, RouteID: &routeID, Severity: "warning",
		OpenedAt: clock.now.Add(-time.Hour), Summary: "slow responses", Details: []byte(`{"app":"shop"}`),
	}); err != nil {
		t.Fatalf("InsertIncident() error = %v", err)
	}
	// Hourly request data for the last two days.
	hourly := make([]int32, store.Buckets)
	hourly[2] = 100
	if err := st.FlushBuckets(ctx, store.Flush{Minutes: []store.RouteMinute{{
		RouteID: routeID, TS: clock.now.Add(-time.Hour), Requests: 100, S2xx: 100, Latency: hourly,
	}}}); err != nil {
		t.Fatalf("FlushBuckets() error = %v", err)
	}
	if err := st.Rollup(ctx, clock.now, time.UTC); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	// A deploy and a failed unit.
	if _, err := st.AddEvent(ctx, store.Event{
		TS: clock.now.Add(-4 * time.Hour), Kind: "deploy", Severity: "info", AppID: &appID,
		Subject: "shop", Title: "deploy", Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("AddEvent() error = %v", err)
	}
	if err := st.UpsertUnit(ctx, store.Unit{
		Name: "backup.service", ActiveState: "failed", SubState: "failed", Result: "exit-code",
		LastSeen: clock.now,
	}); err != nil {
		t.Fatalf("UpsertUnit() error = %v", err)
	}
	// A filesystem with growth and a certificate that expires soon.
	for hour := 24; hour >= 0; hour-- {
		used := int64(50_000_000_000 + (24-hour)*100_000_000)
		if err := st.InsertDiskMinute(ctx, store.DiskMinute{
			Disk: "/", TS: clock.now.Add(-time.Duration(hour) * time.Hour), SizeBytes: 100_000_000_000,
			UsedBytes: used, AvailBytes: 100_000_000_000 - used, Inodes: 100, InodesUsed: 1,
		}); err != nil {
			t.Fatalf("InsertDiskMinute() error = %v", err)
		}
	}
	expiry := clock.now.Add(10 * 24 * time.Hour)
	if err := st.UpsertCert(ctx, store.Cert{
		Domain: "shop.example.com", CheckedAt: clock.now, Status: "expiring", NotAfter: &expiry,
		Issuer: "Test CA", Subject: "shop.example.com",
	}); err != nil {
		t.Fatalf("UpsertCert() error = %v", err)
	}
	if err := st.InsertDockerDisk(ctx, store.DockerDisk{TS: clock.now, ImagesBytes: 100, ImagesReclaimableBytes: 2 << 30}); err != nil {
		t.Fatalf("InsertDockerDisk() error = %v", err)
	}
	text, err := New(st, cfg, discardLogger(), clock).Text(context.Background())
	if err != nil {
		t.Fatalf("Text() error = %v", err)
	}
	for _, want := range []string{
		"Raedar digest — host-1",
		"incidents in the last 24 hours",
		"Open incidents:",
		"slow responses",
		"route.errors: 1",
		"Deploys:",
		"shop: 1",
		"Apps:",
		"shop:",
		"Filesystems:",
		"/: ",
		"Docker disk usage:",
		"2.0 GB reclaimable",
		"Certificates:",
		"shop.example.com: expires in 10 days",
		"Failed units:",
		"backup.service (exit-code)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("digest = %q, want it to contain %q", text, want)
		}
	}
}
