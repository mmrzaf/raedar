//go:build integration

package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestRollup(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	routeID := insertRoute(t, st, "shop@docker")
	var slotID int64
	if appID, err := st.UpsertApp(ctx, "shop", base); err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	} else if slotID, err = st.UpsertSlot(ctx, Slot{AppID: appID, Key: "api #1", Service: "api", Replica: 1}, base); err != nil {
		t.Fatalf("UpsertSlot() error = %v", err)
	}
	first := make([]int32, Buckets)
	first[2] = 3
	second := make([]int32, Buckets)
	second[2], second[5] = 2, 1
	for i, minute := range []time.Duration{0, time.Minute} {
		latency := first
		if i == 1 {
			latency = second
		}
		if err := st.FlushBuckets(ctx, Flush{
			Minutes: []RouteMinute{{
				RouteID: routeID, TS: base.Add(minute), Requests: 10, S1xx: 1, S2xx: 7, S4xx: 1, S5xx: 1,
				NoBackend: 2, Bytes: 100, Latency: latency,
			}},
			Windows: []PathWindow{{RouteID: routeID, TS: base.Add(minute), Method: "GET", Pattern: "/api",
				Requests: 10, Latency: latency}},
		}); err != nil {
			t.Fatalf("FlushBuckets() error = %v", err)
		}
	}
	limit := int64(512 << 20)
	if err := st.InsertSlotMinute(ctx, SlotMinute{SlotID: slotID, TS: base, CPUMillicores: 100, MemBytes: 1000,
		MemLimitBytes: &limit, NetRxBytes: 10, NetTxBytes: 20, Running: true}); err != nil {
		t.Fatalf("InsertSlotMinute() error = %v", err)
	}
	if err := st.InsertSlotMinute(ctx, SlotMinute{SlotID: slotID, TS: base.Add(time.Minute), CPUMillicores: 300,
		MemBytes: 3000, MemLimitBytes: &limit, NetRxBytes: 30, NetTxBytes: 40, Running: true}); err != nil {
		t.Fatalf("InsertSlotMinute() error = %v", err)
	}
	if err := st.InsertHostMinute(ctx, HostMinute{TS: base, CPUPercent: 10, Load1: 0.5, Load5: 0.4, Load15: 0.3,
		MemTotal: 100, MemAvailable: 50, SwapTotal: 10, SwapUsed: 2}); err != nil {
		t.Fatalf("InsertHostMinute() error = %v", err)
	}
	if err := st.InsertHostMinute(ctx, HostMinute{TS: base.Add(time.Minute), CPUPercent: 30, Load1: 1.5, Load5: 1.4,
		Load15: 1.3, MemTotal: 100, MemAvailable: 20, SwapTotal: 10, SwapUsed: 6}); err != nil {
		t.Fatalf("InsertHostMinute() error = %v", err)
	}
	if err := st.InsertDiskMinute(ctx, DiskMinute{Disk: "/", TS: base, SizeBytes: 100, UsedBytes: 10,
		AvailBytes: 90, Inodes: 10, InodesUsed: 1}); err != nil {
		t.Fatalf("InsertDiskMinute() error = %v", err)
	}
	if err := st.InsertDiskMinute(ctx, DiskMinute{Disk: "/", TS: base.Add(time.Minute), SizeBytes: 100,
		UsedBytes: 40, AvailBytes: 60, Inodes: 10, InodesUsed: 4}); err != nil {
		t.Fatalf("InsertDiskMinute() error = %v", err)
	}

	if err := st.Rollup(ctx, base.Add(65*time.Minute), time.UTC); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	var requests, s5xx, noBackend, bytes int64
	var latency []int32
	err := st.pool.QueryRow(ctx, `select requests, s5xx, no_backend, bytes, latency from route_hour where route_id = $1 and ts = $2`,
		routeID, base).Scan(&requests, &s5xx, &noBackend, &bytes, &latency)
	if err != nil {
		t.Fatalf("read route_hour: %v", err)
	}
	if requests != 20 || s5xx != 2 || noBackend != 4 || bytes != 200 {
		t.Errorf("route_hour = %d requests, %d 5xx, %d without backend, %d bytes; want 20, 2, 4, 200",
			requests, s5xx, noBackend, bytes)
	}
	wantLatency := make([]int32, Buckets)
	wantLatency[2], wantLatency[5] = 5, 1
	if !reflect.DeepEqual(latency, wantLatency) {
		t.Errorf("latency = %v, want %v", latency, wantLatency)
	}
	var cpuAvg, cpuMax float64
	var memAvg, memMax float64
	err = st.pool.QueryRow(ctx, `select cpu_avg, cpu_max, mem_avg, mem_max from slot_hour where slot_id = $1 and ts = $2`,
		slotID, base).Scan(&cpuAvg, &cpuMax, &memAvg, &memMax)
	if err != nil {
		t.Fatalf("read slot_hour: %v", err)
	}
	if cpuAvg != 200 || cpuMax != 300 || memAvg != 2000 || memMax != 3000 {
		t.Errorf("slot_hour = %v/%v CPU, %v/%v memory; want 200/300 and 2000/3000", cpuAvg, cpuMax, memAvg, memMax)
	}
	var hostCPU, hostMax, loadAvg, loadMax float64
	var availableMin int64
	var swapMax int64
	err = st.pool.QueryRow(ctx, `select cpu_avg, cpu_max, load1_avg, load1_max, mem_available_min, swap_used_max
		from host_hour where ts = $1`, base).Scan(&hostCPU, &hostMax, &loadAvg, &loadMax, &availableMin, &swapMax)
	if err != nil {
		t.Fatalf("read host_hour: %v", err)
	}
	if hostCPU != 20 || hostMax != 30 || loadAvg != 1 || loadMax != 1.5 || availableMin != 20 || swapMax != 6 {
		t.Errorf("host_hour = %v/%v CPU, %v/%v load, %d available, %d swap; want the documented aggregates",
			hostCPU, hostMax, loadAvg, loadMax, availableMin, swapMax)
	}
	var size, usedMax, availMin, inodesUsedMax int64
	err = st.pool.QueryRow(ctx, `select size_bytes, used_bytes_max, avail_bytes_min, inodes_used_max
		from disk_hour where disk = '/' and ts = $1`, base).Scan(&size, &usedMax, &availMin, &inodesUsedMax)
	if err != nil {
		t.Fatalf("read disk_hour: %v", err)
	}
	if size != 100 || usedMax != 40 || availMin != 60 || inodesUsedMax != 4 {
		t.Errorf("disk_hour = %d/%d/%d/%d, want 100/40/60/4", size, usedMax, availMin, inodesUsedMax)
	}

	// A rerun after more minute data replaces the hour instead of doubling it.
	if err := st.FlushBuckets(ctx, Flush{Minutes: []RouteMinute{{
		RouteID: routeID, TS: base.Add(2 * time.Minute), Requests: 5, S2xx: 5,
	}}}); err != nil {
		t.Fatalf("second FlushBuckets() error = %v", err)
	}
	if err := st.Rollup(ctx, base.Add(65*time.Minute), time.UTC); err != nil {
		t.Fatalf("second Rollup() error = %v", err)
	}
	if err := st.pool.QueryRow(ctx, `select requests from route_hour where route_id = $1 and ts = $2`,
		routeID, base).Scan(&requests); err != nil {
		t.Fatalf("read replaced route_hour: %v", err)
	}
	if requests != 25 {
		t.Errorf("requests after the second rollup = %d, want 25", requests)
	}
}

func TestRollupRecomputesOnlyCompleteHours(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := time.Date(2026, 10, 18, 12, 5, 0, 0, time.UTC)
	routeID := insertRoute(t, st, "shop@docker")
	for _, hour := range []time.Duration{1, 2, 3, 4} {
		start := now.Truncate(time.Hour).Add(-hour * time.Hour)
		if err := st.FlushBuckets(ctx, Flush{Minutes: []RouteMinute{{RouteID: routeID, TS: start, Requests: 1}}}); err != nil {
			t.Fatalf("FlushBuckets() error = %v", err)
		}
	}
	if err := st.Rollup(ctx, now, time.UTC); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	var hours []time.Time
	rows, err := st.pool.Query(ctx, `select ts from route_hour order by ts`)
	if err != nil {
		t.Fatalf("read route_hour: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan route_hour: %v", err)
		}
		hours = append(hours, ts.UTC())
	}
	want := []time.Time{
		now.Truncate(time.Hour).Add(-3 * time.Hour),
		now.Truncate(time.Hour).Add(-2 * time.Hour),
		now.Truncate(time.Hour).Add(-time.Hour),
	}
	if !reflect.DeepEqual(hours, want) {
		t.Errorf("rolled up hours = %v, want the last three complete hours %v", hours, want)
	}
}

func TestRollupFollowsLocalHours(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	tehran, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	routeID := insertRoute(t, st, "shop@docker")
	// Local 13:40 and 14:10 are in different local hours but 10:10 and 10:40
	// UTC are in the same UTC hour.
	for _, ts := range []time.Time{
		time.Date(2026, 10, 18, 10, 10, 0, 0, time.UTC),
		time.Date(2026, 10, 18, 10, 40, 0, 0, time.UTC),
	} {
		if err := st.FlushBuckets(ctx, Flush{Minutes: []RouteMinute{{RouteID: routeID, TS: ts, Requests: 5}}}); err != nil {
			t.Fatalf("FlushBuckets() error = %v", err)
		}
	}
	if err := st.Rollup(ctx, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), tehran); err != nil {
		t.Fatalf("Rollup() error = %v", err)
	}
	rows, err := st.pool.Query(ctx, `select ts, requests from route_hour order by ts`)
	if err != nil {
		t.Fatalf("read route_hour: %v", err)
	}
	defer rows.Close()
	type hour struct {
		ts       time.Time
		requests int64
	}
	var got []hour
	for rows.Next() {
		var h hour
		if err := rows.Scan(&h.ts, &h.requests); err != nil {
			t.Fatalf("scan: %v", err)
		}
		h.ts = h.ts.UTC()
		got = append(got, h)
	}
	want := []hour{
		{time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC), 5},
		{time.Date(2026, 10, 18, 10, 30, 0, 0, time.UTC), 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("route_hour = %v, want rows on local hour starts %v", got, want)
	}
}

func TestRealignRollupsRebuildsMisalignedHours(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	tehran, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	routeID := insertRoute(t, st, "shop@docker")
	minute := time.Date(2026, 10, 18, 10, 10, 0, 0, time.UTC)
	if err := st.FlushBuckets(ctx, Flush{Minutes: []RouteMinute{{RouteID: routeID, TS: minute, Requests: 7}}}); err != nil {
		t.Fatalf("FlushBuckets() error = %v", err)
	}
	// What an older version wrote: a row on the UTC hour, plus one far older
	// than the minute tables cover, which cannot be rebuilt and must stay.
	utcHour := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	ancient := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{utcHour, ancient} {
		insertHourlyRoute(t, st, routeID, ts, 99, 0, 2, 99)
	}
	hours, err := st.RealignRollups(ctx, tehran, 14*24*time.Hour, now)
	if err != nil {
		t.Fatalf("RealignRollups() error = %v", err)
	}
	if hours == 0 {
		t.Error("RealignRollups() rebuilt no hours, want the misaligned row to trigger a rebuild")
	}
	var stamps []time.Time
	rows, err := st.pool.Query(ctx, `select ts from route_hour order by ts`)
	if err != nil {
		t.Fatalf("read route_hour: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stamps = append(stamps, ts.UTC())
	}
	want := []time.Time{ancient, time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)}
	if !reflect.DeepEqual(stamps, want) {
		t.Errorf("route_hour = %v, want the rebuilt local hour and the untouched old row %v", stamps, want)
	}
	again, err := st.RealignRollups(ctx, tehran, 14*24*time.Hour, now)
	if err != nil || again != 0 {
		t.Errorf("second RealignRollups() = %d, %v, want nothing to do", again, err)
	}
}
