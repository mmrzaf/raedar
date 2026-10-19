package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SlotMinute is one row of the slot_minute table.
type SlotMinute struct {
	// SlotID is the slot the sample belongs to.
	SlotID int64
	// TS is the start of the minute, in UTC.
	TS time.Time
	// CPUMillicores is the CPU the container used in the minute, in
	// millicores.
	CPUMillicores int
	// MemBytes is the container's working set memory.
	MemBytes int64
	// MemLimitBytes is the container's memory limit; nil when it has none.
	MemLimitBytes *int64
	// NetRxBytes and NetTxBytes are the network traffic in the minute.
	NetRxBytes, NetTxBytes int64
	// Running reports whether the container was running at the sample.
	Running bool
}

// HostMinute is one row of the host_minute table.
type HostMinute struct {
	// TS is the start of the minute, in UTC.
	TS time.Time
	// CPUPercent is the CPU usage of the host since the previous sample.
	CPUPercent float64
	// Load1, Load5 and Load15 are the load averages.
	Load1, Load5, Load15 float64
	// MemTotal and MemAvailable are the host's memory, in bytes.
	MemTotal, MemAvailable int64
	// SwapTotal and SwapUsed are the host's swap, in bytes.
	SwapTotal, SwapUsed int64
}

// DiskMinute is one row of the disk_minute table.
type DiskMinute struct {
	// Disk is the display name of the filesystem.
	Disk string
	// TS is the start of the minute, in UTC.
	TS time.Time
	// SizeBytes, UsedBytes and AvailBytes describe the filesystem.
	SizeBytes, UsedBytes, AvailBytes int64
	// Inodes and InodesUsed are the filesystem's inode counters.
	Inodes, InodesUsed int64
}

// DockerDisk is one row of the docker_disk table.
type DockerDisk struct {
	// TS is when Docker was asked.
	TS time.Time
	// ImagesBytes and ImagesReclaimableBytes describe the images.
	ImagesBytes, ImagesReclaimableBytes int64
	// BuildCacheBytes is the size of the build cache.
	BuildCacheBytes int64
	// VolumesBytes is the size of all volumes.
	VolumesBytes int64
	// ContainersBytes is the size of the containers' writable layers.
	ContainersBytes int64
}

// Unit is one row of the units table.
type Unit struct {
	// Name is the unit name.
	Name string
	// ActiveState and SubState are the systemd states.
	ActiveState, SubState string
	// Result is systemd's result of the last run.
	Result string
	// ExitStatus is the exit status of the last run; nil when there is none.
	ExitStatus *int
	// Since is when the unit entered its current state; nil when unknown.
	Since *time.Time
	// LastSeen is when the unit was last read.
	LastSeen time.Time
}

// Cert is one row of the certs table.
type Cert struct {
	// Domain is the hostname the certificate was checked for.
	Domain string
	// RouteID is a route that serves the domain; nil when none is known.
	RouteID *int64
	// CheckedAt is when the certificate was checked.
	CheckedAt time.Time
	// Status is ok, expiring, expired, default, mismatch, private or error.
	Status string
	// NotAfter is the certificate's expiry date; nil when unknown.
	NotAfter *time.Time
	// Issuer and Subject describe the certificate.
	Issuer, Subject string
	// SANs are the names the certificate covers.
	SANs []string
	// Error is why the check failed; empty when it did not.
	Error string
}

// InsertSlotMinute records one container sample, replacing a sample of the
// same minute.
func (s *Store) InsertSlotMinute(ctx context.Context, sample SlotMinute) error {
	_, err := s.pool.Exec(ctx, `
		insert into slot_minute (slot_id, ts, cpu_millicores, mem_bytes, mem_limit_bytes, net_rx_bytes, net_tx_bytes, running)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (slot_id, ts) do update set
			cpu_millicores = excluded.cpu_millicores,
			mem_bytes = excluded.mem_bytes,
			mem_limit_bytes = excluded.mem_limit_bytes,
			net_rx_bytes = excluded.net_rx_bytes,
			net_tx_bytes = excluded.net_tx_bytes,
			running = excluded.running`,
		sample.SlotID, sample.TS, sample.CPUMillicores, sample.MemBytes, sample.MemLimitBytes,
		sample.NetRxBytes, sample.NetTxBytes, sample.Running)
	if err != nil {
		return fmt.Errorf("insert slot sample: %w", err)
	}
	return nil
}

// InsertHostMinute records one host sample, replacing a sample of the same
// minute.
func (s *Store) InsertHostMinute(ctx context.Context, sample HostMinute) error {
	_, err := s.pool.Exec(ctx, `
		insert into host_minute (ts, cpu_percent, load1, load5, load15, mem_total, mem_available, swap_total, swap_used)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (ts) do update set
			cpu_percent = excluded.cpu_percent,
			load1 = excluded.load1,
			load5 = excluded.load5,
			load15 = excluded.load15,
			mem_total = excluded.mem_total,
			mem_available = excluded.mem_available,
			swap_total = excluded.swap_total,
			swap_used = excluded.swap_used`,
		sample.TS, sample.CPUPercent, sample.Load1, sample.Load5, sample.Load15,
		sample.MemTotal, sample.MemAvailable, sample.SwapTotal, sample.SwapUsed)
	if err != nil {
		return fmt.Errorf("insert host sample: %w", err)
	}
	return nil
}

// InsertDiskMinute records one filesystem sample, replacing a sample of the
// same minute.
func (s *Store) InsertDiskMinute(ctx context.Context, sample DiskMinute) error {
	_, err := s.pool.Exec(ctx, `
		insert into disk_minute (disk, ts, size_bytes, used_bytes, avail_bytes, inodes, inodes_used)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (disk, ts) do update set
			size_bytes = excluded.size_bytes,
			used_bytes = excluded.used_bytes,
			avail_bytes = excluded.avail_bytes,
			inodes = excluded.inodes,
			inodes_used = excluded.inodes_used`,
		sample.Disk, sample.TS, sample.SizeBytes, sample.UsedBytes, sample.AvailBytes,
		sample.Inodes, sample.InodesUsed)
	if err != nil {
		return fmt.Errorf("insert disk sample for %s: %w", sample.Disk, err)
	}
	return nil
}

// InsertDockerDisk records one Docker disk usage report.
func (s *Store) InsertDockerDisk(ctx context.Context, usage DockerDisk) error {
	_, err := s.pool.Exec(ctx, `
		insert into docker_disk (ts, images_bytes, images_reclaimable_bytes, build_cache_bytes, volumes_bytes, containers_bytes)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (ts) do update set
			images_bytes = excluded.images_bytes,
			images_reclaimable_bytes = excluded.images_reclaimable_bytes,
			build_cache_bytes = excluded.build_cache_bytes,
			volumes_bytes = excluded.volumes_bytes,
			containers_bytes = excluded.containers_bytes`,
		usage.TS, usage.ImagesBytes, usage.ImagesReclaimableBytes, usage.BuildCacheBytes,
		usage.VolumesBytes, usage.ContainersBytes)
	if err != nil {
		return fmt.Errorf("insert Docker disk usage: %w", err)
	}
	return nil
}

// UpsertUnit records the state of one systemd unit.
func (s *Store) UpsertUnit(ctx context.Context, unit Unit) error {
	_, err := s.pool.Exec(ctx, `
		insert into units (name, active_state, sub_state, result, exit_status, since, last_seen)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (name) do update set
			active_state = excluded.active_state,
			sub_state = excluded.sub_state,
			result = excluded.result,
			exit_status = excluded.exit_status,
			since = excluded.since,
			last_seen = excluded.last_seen`,
		unit.Name, unit.ActiveState, unit.SubState, unit.Result, unit.ExitStatus, unit.Since, unit.LastSeen)
	if err != nil {
		return fmt.Errorf("upsert unit %s: %w", unit.Name, err)
	}
	return nil
}

// UnitNames returns the names of the units that are recorded.
func (s *Store) UnitNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select name from units`)
	if err != nil {
		return nil, fmt.Errorf("read unit names: %w", err)
	}
	defer rows.Close()
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read unit names: %w", err)
	}
	return names, nil
}

// UpsertCert records the latest check of one domain.
func (s *Store) UpsertCert(ctx context.Context, cert Cert) error {
	sans := cert.SANs
	if sans == nil {
		sans = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		insert into certs (domain, route_id, checked_at, status, not_after, issuer, subject, sans, error)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (domain) do update set
			route_id = excluded.route_id,
			checked_at = excluded.checked_at,
			status = excluded.status,
			not_after = excluded.not_after,
			issuer = excluded.issuer,
			subject = excluded.subject,
			sans = excluded.sans,
			error = excluded.error`,
		cert.Domain, cert.RouteID, cert.CheckedAt, cert.Status, cert.NotAfter,
		cert.Issuer, cert.Subject, sans, nullableString(cert.Error))
	if err != nil {
		return fmt.Errorf("upsert certificate for %s: %w", cert.Domain, err)
	}
	return nil
}
