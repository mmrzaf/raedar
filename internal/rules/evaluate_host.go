package rules

import (
	"sort"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/source/systemd"
	"github.com/mmrzaf/raedar/internal/source/tlscheck"
	"github.com/mmrzaf/raedar/internal/store"
)

// hostReadings evaluates the host rules.
func (e *Engine) hostReadings(now time.Time, s *snapshot) []reading {
	var readings []reading
	disks := make([]string, 0, len(s.disks))
	for disk := range s.disks {
		disks = append(disks, disk)
	}
	sort.Strings(disks)
	critical := make(map[string]bool)
	for _, name := range disks {
		samples := s.disks[name]
		if len(samples) == 0 {
			continue
		}
		latest := samples[len(samples)-1]
		usage := diskUsage(latest)
		readings = append(readings, diskReading(RuleHostDisk, name, usage, e.cfg, s.open, "filesystem almost full"))
		inodes := inodeUsage(latest)
		readings = append(readings, diskReading(RuleHostInodes, name, inodes, e.cfg, s.open, "inodes almost full"))
		if usage >= float64(e.cfg.Rules.DiskCrit) {
			critical[name] = true
		}
		forecast, ok := e.forecastReading(name, samples, e.cfg, s.open)
		if !ok {
			continue
		}
		// The forecast stays quiet while the filesystem is critical, unless it
		// is open and has to be able to resolve.
		if critical[name] {
			if _, open := s.open[store.IncidentKey{Rule: RuleHostDiskForecast, Subject: name}]; !open {
				continue
			}
		}
		readings = append(readings, forecast)
	}
	if s.host != nil && s.host.MemTotal > 0 {
		available := 100 * float64(s.host.MemAvailable) / float64(s.host.MemTotal)
		readings = append(readings, reading{
			rule:         RuleHostMemory,
			subject:      e.cfg.HostName,
			severity:     "warning",
			firing:       available < float64(e.cfg.Rules.HostMemoryMinAvailable),
			alerts:       true,
			openAfter:    5 * time.Minute,
			resolveAfter: e.cfg.Rules.ResolveAfter,
			summary:      "available memory low",
			details: map[string]any{
				"available_percent": available,
				"available_bytes":   s.host.MemAvailable,
				"total_bytes":       s.host.MemTotal,
				"threshold":         float64(e.cfg.Rules.HostMemoryMinAvailable),
			},
		})
	}
	return readings
}

// diskReading evaluates host.disk or host.inodes for one filesystem. Usage
// must drop two points below the warning threshold to resolve.
func diskReading(rule, subject string, usage float64, cfg *config.Config, open map[store.IncidentKey]store.Incident, summary string) reading {
	warn := float64(cfg.Rules.DiskWarn)
	crit := float64(cfg.Rules.DiskCrit)
	_, isOpen := open[store.IncidentKey{Rule: rule, Subject: subject}]
	firing := usage >= warn
	if isOpen {
		firing = usage >= warn-2
	}
	severity := "warning"
	if usage >= crit {
		severity = "critical"
	}
	return reading{
		rule:         rule,
		subject:      subject,
		severity:     severity,
		firing:       firing,
		alerts:       true,
		openSamples:  2,
		resolveAfter: 0,
		summary:      summary,
		details: map[string]any{
			"usage":     usage,
			"threshold": warn,
			"critical":  crit,
		},
	}
}

// forecastReading evaluates host.disk_forecast for one filesystem.
func (e *Engine) forecastReading(name string, samples []store.DiskMinute, cfg *config.Config, open map[store.IncidentKey]store.Incident) (reading, bool) {
	if len(samples) < 2 {
		return reading{}, false
	}
	first, latest := samples[0], samples[len(samples)-1]
	hours := latest.TS.Sub(first.TS).Hours()
	if hours < 12 || latest.SizeBytes <= 0 {
		return reading{}, false
	}
	growthPerHour := float64(latest.UsedBytes-first.UsedBytes) / hours
	usage := diskUsage(latest)
	horizon := cfg.Rules.DiskFullWithin
	limit := horizon
	if _, isOpen := open[store.IncidentKey{Rule: RuleHostDiskForecast, Subject: name}]; isOpen {
		limit = horizon * 3 / 2
	}
	var projection time.Duration
	projecting := growthPerHour > 0
	if projecting {
		projection = time.Duration(float64(latest.SizeBytes-latest.UsedBytes) / growthPerHour * float64(time.Hour))
	}
	firing := usage >= 50 && projecting && projection <= limit
	details := map[string]any{
		"used_bytes":           latest.UsedBytes,
		"size_bytes":           latest.SizeBytes,
		"avail_bytes":          latest.AvailBytes,
		"usage":                usage,
		"growth_bytes_per_day": growthPerHour * 24,
	}
	if projecting {
		details["days_until_full"] = projection.Hours() / 24
	}
	return reading{
		rule:         RuleHostDiskForecast,
		subject:      name,
		severity:     "warning",
		firing:       firing,
		alerts:       true,
		resolveAfter: 0,
		summary:      "filesystem filling up",
		details:      details,
	}, true
}

// diskUsage returns how full a filesystem is, as df reports it.
func diskUsage(sample store.DiskMinute) float64 {
	total := sample.UsedBytes + sample.AvailBytes
	if total <= 0 {
		return 0
	}
	return 100 * float64(sample.UsedBytes) / float64(total)
}

// inodeUsage returns how many of a filesystem's inodes are used.
func inodeUsage(sample store.DiskMinute) float64 {
	if sample.Inodes <= 0 {
		return 0
	}
	return 100 * float64(sample.InodesUsed) / float64(sample.Inodes)
}

// unitReadings evaluates host.unit_failed for every recorded unit.
func (e *Engine) unitReadings(now time.Time, s *snapshot) []reading {
	readings := make([]reading, 0, len(s.units))
	for _, unit := range s.units {
		severity := "warning"
		if systemd.Matches(e.cfg.Host.SystemdCritical, unit.Name) {
			severity = "critical"
		}
		details := map[string]any{
			"state":     unit.ActiveState,
			"sub_state": unit.SubState,
			"result":    unit.Result,
		}
		if unit.ExitStatus != nil {
			details["exit_status"] = *unit.ExitStatus
		}
		readings = append(readings, reading{
			rule:         RuleHostUnitFailed,
			subject:      unit.Name,
			severity:     severity,
			firing:       unit.ActiveState == "failed",
			alerts:       true,
			resolveAfter: 0,
			summary:      "unit failed",
			details:      details,
		})
	}
	return readings
}

// certReadings evaluates the certificate rules.
func (e *Engine) certReadings(now time.Time, s *snapshot) []reading {
	readings := make([]reading, 0, 2*len(s.certs))
	for _, cert := range s.certs {
		severity := "warning"
		expiring := false
		if cert.NotAfter != nil {
			remaining := cert.NotAfter.Sub(now)
			switch {
			case remaining <= 0, remaining <= e.cfg.Rules.CertCrit:
				severity, expiring = "critical", true
			case remaining <= e.cfg.Rules.CertWarn:
				severity, expiring = "warning", true
			}
		}
		details := map[string]any{
			"status":  cert.Status,
			"issuer":  cert.Issuer,
			"subject": cert.Subject,
		}
		if cert.NotAfter != nil {
			details["not_after"] = cert.NotAfter
			details["days_left"] = cert.NotAfter.Sub(now).Hours() / 24
		}
		if cert.Error != "" {
			details["error"] = cert.Error
		}
		readings = append(readings, reading{
			rule:         RuleCertExpiry,
			subject:      cert.Domain,
			routeID:      cert.RouteID,
			severity:     severity,
			firing:       expiring,
			alerts:       true,
			resolveAfter: 0,
			summary:      "certificate expiring",
			details:      details,
		})
		invalid := cert.Status == tlscheck.StatusDefault || cert.Status == tlscheck.StatusMismatch
		readings = append(readings, reading{
			rule:         RuleCertInvalid,
			subject:      cert.Domain,
			routeID:      cert.RouteID,
			severity:     "warning",
			firing:       invalid,
			alerts:       true,
			resolveAfter: 0,
			summary:      "certificate does not match the domain",
			details:      details,
		})
	}
	return readings
}

// peerReadings evaluates peer.silent for every configured peer. A peer that
// never reported counts as silent since Raedar started.
func (e *Engine) peerReadings(now time.Time, s *snapshot) []reading {
	last := make(map[string]time.Time, len(s.peers))
	for _, peer := range s.peers {
		last[peer.Name] = peer.LastHeartbeat
	}
	readings := make([]reading, 0, len(e.cfg.Heartbeat.Peers))
	for _, name := range e.cfg.Heartbeat.Peers {
		at, seen := last[name]
		if !seen {
			at = e.startedAt
		}
		readings = append(readings, reading{
			rule:         RulePeerSilent,
			subject:      name,
			severity:     "critical",
			firing:       now.Sub(at) >= e.cfg.Heartbeat.Timeout,
			alerts:       true,
			resolveAfter: 0,
			summary:      "peer is silent",
			details: map[string]any{
				"last_heartbeat": at,
				"timeout":        e.cfg.Heartbeat.Timeout.String(),
			},
		})
	}
	return readings
}

// sourceReadings evaluates raedar.source for every recorded source.
func (e *Engine) sourceReadings(now time.Time, s *snapshot) []reading {
	readings := make([]reading, 0, len(s.sources))
	for _, state := range s.sources {
		details := map[string]any{"status": state.Status, "since": state.UpdatedAt}
		if state.LastError != nil {
			details["error"] = *state.LastError
		}
		readings = append(readings, reading{
			rule:         RuleSourceFailed,
			subject:      state.Source,
			severity:     "warning",
			firing:       state.Status != store.StatusOK && now.Sub(state.UpdatedAt) >= 5*time.Minute,
			alerts:       true,
			resolveAfter: 0,
			summary:      "source failed",
			details:      details,
		})
	}
	return readings
}
