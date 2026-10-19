//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// insertHourlyRoute adds one route_hour row.
func insertHourlyRoute(t *testing.T, st *Store, routeID int64, ts time.Time, requests, s5xx int64, bucket int, count int64) {
	t.Helper()
	latency := make([]int32, Buckets)
	latency[bucket] = int32(count)
	if _, err := st.pool.Exec(context.Background(), `
		insert into route_hour (route_id, ts, requests, s5xx, latency) values ($1, $2, $3, $4, $5)
		on conflict (route_id, ts) do update set requests = excluded.requests, s5xx = excluded.s5xx, latency = excluded.latency`,
		routeID, ts, requests, s5xx, latency); err != nil {
		t.Fatalf("insert route_hour: %v", err)
	}
}

func TestComputeBaselines(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	// A zone at UTC+03:30 so that the local hour differs from the UTC hour.
	location := time.FixedZone("UTC+3:30", 3*3600+30*60)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, location)
	routeID := insertRoute(t, st, "shop@docker")
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	at := func(days int, hour int) time.Time {
		return today.AddDate(0, 0, -days).Add(time.Duration(hour) * time.Hour)
	}
	// Five qualifying days at local 09:00 with error rates 1% to 5%.
	for day := 1; day <= 5; day++ {
		requests := int64(100)
		insertHourlyRoute(t, st, routeID, at(day, 9).UTC(), requests, int64(day), 8, requests)
	}
	// One day below the minimum request count.
	insertHourlyRoute(t, st, routeID, at(6, 9).UTC(), 2, 2, 8, 2)
	// An incident overlaps the fifth day's hour.
	if _, err := st.pool.Exec(ctx, `
		insert into incidents (rule, subject, route_id, severity, opened_at, resolved_at, summary)
		values ('route.errors', 'shop@docker', $1, 'warning', $2, $3, 'incident')`,
		routeID, at(5, 9).UTC().Add(10*time.Minute), at(5, 9).UTC().Add(30*time.Minute)); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	// Only two days for local 10:00, which is below BaselineMinDays.
	for day := 1; day <= 2; day++ {
		insertHourlyRoute(t, st, routeID, at(day, 10).UTC(), 100, 1, 8, 100)
	}
	if err := st.ComputeBaselines(ctx, location, 20, now); err != nil {
		t.Fatalf("ComputeBaselines() error = %v", err)
	}
	var errorRate, p95 *float64
	var days int
	err := st.pool.QueryRow(ctx, `select error_rate, p95_ms, days_used from baselines where route_id = $1 and hour_of_day = 9`,
		routeID).Scan(&errorRate, &p95, &days)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	// The incident day and the low-traffic day are excluded; four remain.
	if days != 4 {
		t.Errorf("days used = %d, want 4", days)
	}
	if errorRate == nil || p95 == nil {
		t.Fatalf("baseline = %v/%v, want the median values", errorRate, p95)
	}
	// Median of 1%, 2%, 3% and 4% is 2.5%.
	if *errorRate < 0.0249 || *errorRate > 0.0251 {
		t.Errorf("error rate = %v, want 0.025", *errorRate)
	}
	// Every hour has 100 observations in the 300 ms bucket, so p95 is inside
	// [200ms, 300ms): 100 values, the 95th at 95% of the bucket.
	if *p95 < 200 || *p95 > 300 {
		t.Errorf("p95 = %v ms, want a value in the 200-300 ms bucket", *p95)
	}
	// Local 10:00 has only two qualifying days: stored as missing.
	err = st.pool.QueryRow(ctx, `select error_rate, p95_ms, days_used from baselines where route_id = $1 and hour_of_day = 10`,
		routeID).Scan(&errorRate, &p95, &days)
	if err != nil {
		t.Fatalf("read second baseline: %v", err)
	}
	if days != 2 {
		t.Errorf("days used = %d, want 2", days)
	}
	if errorRate != nil || p95 != nil {
		t.Errorf("baseline = %v/%v, want missing values", errorRate, p95)
	}
	// Every route and hour has a row.
	var count int
	if err := st.pool.QueryRow(ctx, `select count(*) from baselines where route_id = $1`, routeID).Scan(&count); err != nil {
		t.Fatalf("count baselines: %v", err)
	}
	if count != 24 {
		t.Errorf("baseline rows = %d, want 24", count)
	}
}

func TestBaselinesUseTheLocalHour(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	location := time.FixedZone("UTC+3:30", 3*3600+30*60)
	now := time.Date(2026, 10, 18, 6, 0, 0, 0, location)
	routeID := insertRoute(t, st, "shop@docker")
	// Local 09:00 is 05:30 UTC; the UTC hour of these rows is 5.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	for day := 1; day <= 3; day++ {
		ts := today.AddDate(0, 0, -day).Add(9 * time.Hour)
		if ts.UTC().Hour() != 5 {
			t.Fatalf("test setup: local 09:00 is UTC hour %d, want 5", ts.UTC().Hour())
		}
		insertHourlyRoute(t, st, routeID, ts.UTC(), 100, 2, 8, 100)
	}
	if err := st.ComputeBaselines(ctx, location, 20, now); err != nil {
		t.Fatalf("ComputeBaselines() error = %v", err)
	}
	var errorRate *float64
	if err := st.pool.QueryRow(ctx, `select error_rate from baselines where route_id = $1 and hour_of_day = 9`,
		routeID).Scan(&errorRate); err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if errorRate == nil || *errorRate != 0.02 {
		t.Errorf("local 09:00 baseline = %v, want the rows stored at UTC 05:00", errorRate)
	}
	var utcHour *float64
	if err := st.pool.QueryRow(ctx, `select error_rate from baselines where route_id = $1 and hour_of_day = 5`,
		routeID).Scan(&utcHour); err != nil {
		t.Fatalf("read UTC hour baseline: %v", err)
	}
	if utcHour != nil {
		t.Errorf("UTC hour 5 baseline = %v, want missing: the rows belong to local 09:00", utcHour)
	}
}

func TestRetention(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	routeID := insertRoute(t, st, "shop@docker")
	old, fresh := now.Add(-20*24*time.Hour), now.Add(-time.Hour)
	// 6000 old minute rows, to exercise the batching, and one fresh row.
	if _, err := st.pool.Exec(ctx, `
		insert into route_minute (route_id, ts, requests)
		select $1, $2::timestamptz + (n || ' minutes')::interval, 1 from generate_series(1, 6000) as n`,
		routeID, old); err != nil {
		t.Fatalf("insert old route_minute: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `insert into route_minute (route_id, ts, requests) values ($1, $2, 1)`,
		routeID, fresh); err != nil {
		t.Fatalf("insert fresh route_minute: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `insert into path_window (route_id, ts, method, pattern, requests) values ($1, $2, 'GET', '/', 1)`,
		routeID, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatalf("insert old path_window: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `insert into route_hour (route_id, ts, requests) values ($1, $2, 1)`,
		routeID, now.Add(-401*24*time.Hour)); err != nil {
		t.Fatalf("insert old route_hour: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `insert into events (ts, kind, severity, subject, title) values ($1, 'deploy', 'info', 'x', 'x')`,
		now.Add(-401*24*time.Hour)); err != nil {
		t.Fatalf("insert old event: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `insert into events (ts, kind, severity, subject, title) values ($1, 'deploy', 'info', 'x', 'x')`,
		now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("insert fresh event: %v", err)
	}
	for _, name := range []string{"gone.service", "alive.service"} {
		seen := now.Add(-8 * 24 * time.Hour)
		if name == "alive.service" {
			seen = now.Add(-time.Minute)
		}
		if _, err := st.pool.Exec(ctx, `insert into units (name, active_state, sub_state, result, last_seen) values ($1, 'failed', 'failed', 'exit-code', $2)`, name, seen); err != nil {
			t.Fatalf("insert unit: %v", err)
		}
	}
	for domain, checked := range map[string]time.Time{"old.example.com": now.Add(-8 * 24 * time.Hour), "new.example.com": now.Add(-time.Hour)} {
		if _, err := st.pool.Exec(ctx, `insert into certs (domain, checked_at, status) values ($1, $2, 'ok')`, domain, checked); err != nil {
			t.Fatalf("insert cert: %v", err)
		}
	}
	policy := Retention{Minute: 14 * 24 * time.Hour, Paths: 7 * 24 * time.Hour, Hour: 400 * 24 * time.Hour, Events: 400 * 24 * time.Hour}
	if err := st.Retention(ctx, policy, now); err != nil {
		t.Fatalf("Retention() error = %v", err)
	}
	var minutes, paths, hours, events int
	if err := st.pool.QueryRow(ctx, `select count(*) from route_minute`).Scan(&minutes); err != nil {
		t.Fatalf("count route_minute: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from path_window`).Scan(&paths); err != nil {
		t.Fatalf("count path_window: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from route_hour`).Scan(&hours); err != nil {
		t.Fatalf("count route_hour: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if minutes != 1 {
		t.Errorf("route_minute rows = %d, want the fresh row only", minutes)
	}
	if paths != 0 {
		t.Errorf("path_window rows = %d, want none", paths)
	}
	if hours != 0 {
		t.Errorf("route_hour rows = %d, want none", hours)
	}
	if events != 1 {
		t.Errorf("events = %d, want the fresh event only", events)
	}
	var units, certs int
	if err := st.pool.QueryRow(ctx, `select count(*) from units`).Scan(&units); err != nil {
		t.Fatalf("count units: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from units where name = 'alive.service'`).Scan(&certs); err != nil || units != 1 || certs != 1 {
		t.Errorf("units = %d, alive = %d (%v), want only the unit seen recently", units, certs, err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from certs`).Scan(&certs); err != nil {
		t.Fatalf("count certs: %v", err)
	}
	if certs != 1 {
		t.Errorf("certs = %d, want only the recently checked domain", certs)
	}
}

func TestResolution(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	minuteRetention := 14 * 24 * time.Hour
	tests := []struct {
		name    string
		r       Range
		wantSrc RangeSource
		wantBin time.Duration
	}{
		{name: "one hour", r: Range{From: now.Add(-time.Hour), To: now}, wantSrc: SourceMinutes, wantBin: time.Minute},
		{name: "fourteen days", r: Range{From: now.Add(-14 * 24 * time.Hour), To: now}, wantSrc: SourceMinutes, wantBin: 34 * time.Minute},
		{name: "fifteen days", r: Range{From: now.Add(-15 * 24 * time.Hour), To: now}, wantSrc: SourceHours, wantBin: time.Hour},
		{name: "older than the retention", r: Range{From: now.Add(-20 * 24 * time.Hour), To: now.Add(-16 * 24 * time.Hour)}, wantSrc: SourceHours, wantBin: time.Hour},
		{name: "many points", r: Range{From: now.Add(-time.Hour), To: now, Points: 6000}, wantSrc: SourceMinutes, wantBin: time.Minute},
		{name: "empty range", r: Range{From: now, To: now}, wantSrc: SourceMinutes, wantBin: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, bin := Resolution(tt.r, minuteRetention, now)
			if source != tt.wantSrc || bin != tt.wantBin {
				t.Errorf("Resolution() = %v, %v; want %v, %v", source, bin, tt.wantSrc, tt.wantBin)
			}
		})
	}
}

func TestRouteAndSlotSeries(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	routeID := insertRoute(t, st, "shop@docker")
	for i, requests := range []int64{10, 20, 30} {
		latency := make([]int32, Buckets)
		latency[2] = int32(requests)
		if err := st.FlushBuckets(ctx, Flush{Minutes: []RouteMinute{{
			RouteID: routeID, TS: base.Add(time.Duration(i) * time.Minute), Requests: requests,
			S2xx: requests, Bytes: requests * 10, Latency: latency,
		}}}); err != nil {
			t.Fatalf("FlushBuckets() error = %v", err)
		}
	}
	bins, err := st.RouteSeries(ctx, []int64{routeID}, Range{From: base, To: base.Add(3 * time.Minute)}, SourceMinutes, 2*time.Minute)
	if err != nil {
		t.Fatalf("RouteSeries() error = %v", err)
	}
	if len(bins) != 2 {
		t.Fatalf("bins = %d, want 2", len(bins))
	}
	if bins[0].Requests != 30 || bins[0].S2xx != 30 || bins[0].Bytes != 300 {
		t.Errorf("first bin = %+v, want the first two minutes summed", bins[0])
	}
	if bins[0].Latency[2] != 30 {
		t.Errorf("first histogram = %v, want 30 observations", bins[0].Latency)
	}
	if bins[1].Requests != 30 {
		t.Errorf("second bin = %+v, want the third minute", bins[1])
	}
	// No routes selected: no query.
	if bins, err := st.RouteSeries(ctx, nil, Range{From: base, To: base.Add(time.Minute)}, SourceMinutes, time.Minute); err != nil || bins != nil {
		t.Errorf("RouteSeries(nil) = %v, %v; want nothing", bins, err)
	}

	appID, err := st.UpsertApp(ctx, "shop", base)
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	slotID, err := st.UpsertSlot(ctx, Slot{AppID: appID, Key: "api #1", Service: "api", Replica: 1}, base)
	if err != nil {
		t.Fatalf("UpsertSlot() error = %v", err)
	}
	for i, cpu := range []int{100, 300} {
		if err := st.InsertSlotMinute(ctx, SlotMinute{SlotID: slotID, TS: base.Add(time.Duration(i) * time.Minute),
			CPUMillicores: cpu, MemBytes: int64(cpu) * 10, NetRxBytes: 10, NetTxBytes: 20, Running: true}); err != nil {
			t.Fatalf("InsertSlotMinute() error = %v", err)
		}
	}
	slots, err := st.SlotSeries(ctx, []int64{slotID}, Range{From: base, To: base.Add(2 * time.Minute)}, SourceMinutes, 2*time.Minute)
	if err != nil {
		t.Fatalf("SlotSeries() error = %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("slot bins = %d, want 1", len(slots))
	}
	if slots[0].CPUAvg != 200 || slots[0].CPUMax != 300 || slots[0].NetRx != 20 {
		t.Errorf("slot bin = %+v, want the average, maximum and sums", slots[0])
	}
}

func TestHostAndDiskSeries(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	for i, cpu := range []float64{10, 30} {
		if err := st.InsertHostMinute(ctx, HostMinute{TS: base.Add(time.Duration(i) * time.Minute), CPUPercent: cpu,
			Load1: 1, MemTotal: 100, MemAvailable: 50, SwapTotal: 10, SwapUsed: 1}); err != nil {
			t.Fatalf("InsertHostMinute() error = %v", err)
		}
		if err := st.InsertDiskMinute(ctx, DiskMinute{Disk: "/", TS: base.Add(time.Duration(i) * time.Minute),
			SizeBytes: 100, UsedBytes: int64(cpu), AvailBytes: 100 - int64(cpu), Inodes: 10, InodesUsed: 1}); err != nil {
			t.Fatalf("InsertDiskMinute() error = %v", err)
		}
	}
	hosts, err := st.HostSeries(ctx, Range{From: base, To: base.Add(2 * time.Minute)}, SourceMinutes, 2*time.Minute)
	if err != nil {
		t.Fatalf("HostSeries() error = %v", err)
	}
	if len(hosts) != 1 || hosts[0].CPUAvg != 20 || hosts[0].CPUMax != 30 {
		t.Errorf("host bins = %+v, want one bin with 20/30 CPU", hosts)
	}
	if hosts[0].SwapUsedAvg != 1 {
		t.Errorf("swap average = %v, want the seeded 1 byte", hosts[0].SwapUsedAvg)
	}
	disks, err := st.DiskSeries(ctx, []string{"/"}, Range{From: base, To: base.Add(2 * time.Minute)}, SourceMinutes, 2*time.Minute)
	if err != nil {
		t.Fatalf("DiskSeries() error = %v", err)
	}
	if len(disks) != 1 || disks[0].UsedBytesMax != 30 || disks[0].AvailBytesMin != 70 {
		t.Errorf("disk bins = %+v, want the maximum used and minimum available", disks)
	}
}
