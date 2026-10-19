//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestSampleUpserts(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	appID, err := st.UpsertApp(ctx, "shop", base)
	if err != nil {
		t.Fatalf("UpsertApp() error = %v", err)
	}
	slotID, err := st.UpsertSlot(ctx, Slot{AppID: appID, Key: "api #1", Service: "api", Replica: 1}, base)
	if err != nil {
		t.Fatalf("UpsertSlot() error = %v", err)
	}
	limit := int64(512 << 20)
	if err := st.InsertSlotMinute(ctx, SlotMinute{
		SlotID: slotID, TS: base, CPUMillicores: 100, MemBytes: 1000, MemLimitBytes: &limit,
		NetRxBytes: 10, NetTxBytes: 20, Running: true,
	}); err != nil {
		t.Fatalf("InsertSlotMinute() error = %v", err)
	}
	// A second sample of the same minute replaces the first.
	if err := st.InsertSlotMinute(ctx, SlotMinute{
		SlotID: slotID, TS: base, CPUMillicores: 250, MemBytes: 2000,
		NetRxBytes: 30, NetTxBytes: 40, Running: true,
	}); err != nil {
		t.Fatalf("second InsertSlotMinute() error = %v", err)
	}
	var cpu, rx int
	var memory int64
	var memLimit *int64
	err = st.pool.QueryRow(ctx,
		`select cpu_millicores, mem_bytes, mem_limit_bytes, net_rx_bytes from slot_minute where slot_id = $1 and ts = $2`,
		slotID, base).Scan(&cpu, &memory, &memLimit, &rx)
	if err != nil {
		t.Fatalf("read slot_minute: %v", err)
	}
	if cpu != 250 || memory != 2000 || rx != 30 || memLimit != nil {
		t.Errorf("slot_minute = %d millicores, %d bytes, limit %v, rx %d; want the replaced sample", cpu, memory, memLimit, rx)
	}

	if err := st.InsertHostMinute(ctx, HostMinute{TS: base, CPUPercent: 12.5, Load1: 0.5, Load5: 0.4, Load15: 0.3,
		MemTotal: 1000, MemAvailable: 500, SwapTotal: 100, SwapUsed: 10}); err != nil {
		t.Fatalf("InsertHostMinute() error = %v", err)
	}
	if err := st.InsertHostMinute(ctx, HostMinute{TS: base, CPUPercent: 25, Load1: 0.6, Load5: 0.5, Load15: 0.4,
		MemTotal: 1000, MemAvailable: 400, SwapTotal: 100, SwapUsed: 20}); err != nil {
		t.Fatalf("second InsertHostMinute() error = %v", err)
	}
	var hostCPU float64
	if err := st.pool.QueryRow(ctx, `select cpu_percent from host_minute where ts = $1`, base).Scan(&hostCPU); err != nil {
		t.Fatalf("read host_minute: %v", err)
	}
	if hostCPU != 25 {
		t.Errorf("host CPU = %v, want the replaced sample", hostCPU)
	}

	if err := st.InsertDiskMinute(ctx, DiskMinute{Disk: "/", TS: base, SizeBytes: 100, UsedBytes: 50,
		AvailBytes: 50, Inodes: 10, InodesUsed: 5}); err != nil {
		t.Fatalf("InsertDiskMinute() error = %v", err)
	}
	if err := st.InsertDiskMinute(ctx, DiskMinute{Disk: "/", TS: base, SizeBytes: 100, UsedBytes: 60,
		AvailBytes: 40, Inodes: 10, InodesUsed: 6}); err != nil {
		t.Fatalf("second InsertDiskMinute() error = %v", err)
	}
	var used int64
	if err := st.pool.QueryRow(ctx, `select used_bytes from disk_minute where disk = '/' and ts = $1`, base).Scan(&used); err != nil {
		t.Fatalf("read disk_minute: %v", err)
	}
	if used != 60 {
		t.Errorf("used = %d, want the replaced sample", used)
	}

	if err := st.InsertDockerDisk(ctx, DockerDisk{TS: base, ImagesBytes: 100, ImagesReclaimableBytes: 10,
		BuildCacheBytes: 20, VolumesBytes: 30, ContainersBytes: 40}); err != nil {
		t.Fatalf("InsertDockerDisk() error = %v", err)
	}
	if err := st.InsertDockerDisk(ctx, DockerDisk{TS: base, ImagesBytes: 200, ImagesReclaimableBytes: 20,
		BuildCacheBytes: 30, VolumesBytes: 40, ContainersBytes: 50}); err != nil {
		t.Fatalf("second InsertDockerDisk() error = %v", err)
	}
	var images int64
	if err := st.pool.QueryRow(ctx, `select images_bytes from docker_disk where ts = $1`, base).Scan(&images); err != nil {
		t.Fatalf("read docker_disk: %v", err)
	}
	if images != 200 {
		t.Errorf("images = %d, want the replaced sample", images)
	}
}

func TestUnitUpserts(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	exit := 1
	if err := st.UpsertUnit(ctx, Unit{Name: "backup.service", ActiveState: "failed", SubState: "failed",
		Result: "exit-code", ExitStatus: &exit, Since: &base, LastSeen: base}); err != nil {
		t.Fatalf("UpsertUnit() error = %v", err)
	}
	if err := st.UpsertUnit(ctx, Unit{Name: "backup.service", ActiveState: "active", SubState: "dead",
		LastSeen: base.Add(time.Minute)}); err != nil {
		t.Fatalf("second UpsertUnit() error = %v", err)
	}
	var state, result string
	var exitStatus *int
	err := st.pool.QueryRow(ctx, `select active_state, result, exit_status from units where name = 'backup.service'`).
		Scan(&state, &result, &exitStatus)
	if err != nil {
		t.Fatalf("read units: %v", err)
	}
	if state != "active" || result != "" || exitStatus != nil {
		t.Errorf("unit = %q, %q, %v, want the updated state", state, result, exitStatus)
	}
	names, err := st.UnitNames(ctx)
	if err != nil {
		t.Fatalf("UnitNames() error = %v", err)
	}
	if len(names) != 1 || names[0] != "backup.service" {
		t.Errorf("UnitNames() = %v, want the recorded unit", names)
	}
}

func TestCertUpsert(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	expiry := base.Add(90 * 24 * time.Hour)
	if err := st.UpsertCert(ctx, Cert{Domain: "shop.example.com", CheckedAt: base, Status: "ok",
		NotAfter: &expiry, Issuer: "Test CA", Subject: "shop.example.com", SANs: []string{"shop.example.com"}}); err != nil {
		t.Fatalf("UpsertCert() error = %v", err)
	}
	if err := st.UpsertCert(ctx, Cert{Domain: "shop.example.com", CheckedAt: base.Add(time.Hour), Status: "expiring",
		NotAfter: &expiry, Issuer: "Test CA", Subject: "shop.example.com", SANs: []string{"shop.example.com"}}); err != nil {
		t.Fatalf("second UpsertCert() error = %v", err)
	}
	var status string
	var sans []string
	if err := st.pool.QueryRow(ctx, `select status, sans from certs where domain = 'shop.example.com'`).Scan(&status, &sans); err != nil {
		t.Fatalf("read certs: %v", err)
	}
	if status != "expiring" {
		t.Errorf("status = %q, want the replaced check", status)
	}
	if len(sans) != 1 || sans[0] != "shop.example.com" {
		t.Errorf("SANs = %v, want the recorded names", sans)
	}
}
