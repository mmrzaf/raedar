// Package digest composes the daily summary of the previous 24 hours.
package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// Composer builds the daily digest.
type Composer struct {
	store  *store.Store
	cfg    *config.Config
	logger *slog.Logger
	clock  clock.Clock
}

// New returns a digest composer.
func New(st *store.Store, cfg *config.Config, logger *slog.Logger, clock clock.Clock) *Composer {
	return &Composer{store: st, cfg: cfg, logger: logger, clock: clock}
}

// Text composes the digest of the previous 24 hours.
func (c *Composer) Text(ctx context.Context) (string, error) {
	now := c.clock.Now()
	from := now.Add(-24 * time.Hour)
	previousFrom := from.Add(-24 * time.Hour)
	location := c.cfg.Location
	var lines []string
	lines = append(lines, fmt.Sprintf("Raedar digest — %s — %s", c.cfg.HostName, now.In(location).Format("2006-01-02")))

	incidents, err := c.store.Incidents(ctx, from, now)
	if err != nil {
		return "", err
	}
	open, err := c.store.OpenIncidents(ctx)
	if err != nil {
		return "", err
	}
	lines = append(lines, c.headline(incidents, open))
	if len(open) > 0 {
		lines = append(lines, "Open incidents:")
		for _, incident := range sortedIncidents(open) {
			lines = append(lines, fmt.Sprintf("  • %s · %s since %s", c.subjectName(incident), incident.Summary,
				incident.OpenedAt.In(location).Format("15:04")))
		}
	}
	if len(incidents) > 0 {
		lines = append(lines, "Incidents of the last 24 hours:")
		for _, entry := range groupIncidents(incidents) {
			lines = append(lines, fmt.Sprintf("  %s: %d (longest %s)", entry.rule, entry.count, durationText(entry.longest)))
		}
	}

	deploys, err := c.store.Events(ctx, store.EventDeploy, from, now)
	if err != nil {
		return "", err
	}
	if len(deploys) > 0 {
		lines = append(lines, "Deploys:")
		counts := make(map[string]int)
		for _, event := range deploys {
			counts[event.Subject]++
		}
		for _, name := range sortedKeys(counts) {
			lines = append(lines, fmt.Sprintf("  %s: %d", name, counts[name]))
		}
	}

	apps, err := c.store.AppDayStats(ctx, from, now)
	if err != nil {
		return "", err
	}
	today, err := c.store.AppDayStats(ctx, previousFrom, from)
	if err != nil {
		return "", err
	}
	lines = append(lines, appLines(apps, today)...)

	disks, err := c.store.DiskSamples(ctx, from)
	if err != nil {
		return "", err
	}
	lines = append(lines, diskLines(disks, now)...)

	usage, err := c.store.DockerDiskLatest(ctx)
	if err != nil {
		c.logger.Debug("no Docker disk usage in the digest", slog.String("error", err.Error()))
	} else if usage != nil && usage.ImagesReclaimableBytes > 0 {
		lines = append(lines, "Docker disk usage:", "  "+bytesText(usage.ImagesReclaimableBytes)+" reclaimable")
	}

	certs, err := c.store.Certs(ctx)
	if err != nil {
		return "", err
	}
	lines = append(lines, certLines(certs, now)...)

	units, err := c.store.Units(ctx)
	if err != nil {
		return "", err
	}
	lines = append(lines, unitLines(units)...)
	return strings.Join(lines, "\n"), nil
}

// headline returns the first line of the digest.
func (c *Composer) headline(incidents []store.Incident, open map[store.IncidentKey]store.Incident) string {
	if len(incidents) == 0 && len(open) == 0 {
		return "All quiet."
	}
	if len(incidents) == 0 {
		return fmt.Sprintf("No new incidents; %d still open.", len(open))
	}
	return fmt.Sprintf("%d incidents in the last 24 hours, %d still open.", len(incidents), len(open))
}

// incidentGroup is the incidents of one rule.
type incidentGroup struct {
	rule    string
	count   int
	longest time.Duration
}

// groupIncidents groups incidents by rule.
func groupIncidents(incidents []store.Incident) []incidentGroup {
	groups := make(map[string]*incidentGroup)
	var order []string
	for _, incident := range incidents {
		group := groups[incident.Rule]
		if group == nil {
			group = &incidentGroup{rule: incident.Rule}
			groups[incident.Rule] = group
			order = append(order, incident.Rule)
		}
		group.count++
		if length := incidentLength(incident); length > group.longest {
			group.longest = length
		}
	}
	sort.Strings(order)
	entries := make([]incidentGroup, 0, len(order))
	for _, rule := range order {
		entries = append(entries, *groups[rule])
	}
	return entries
}

// incidentLength returns how long an incident lasted.
func incidentLength(incident store.Incident) time.Duration {
	if incident.ResolvedAt == nil {
		return 0
	}
	return incident.ResolvedAt.Sub(incident.OpenedAt)
}

// appLines returns the app section of the digest.
func appLines(today, yesterday []store.AppDayStats) []string {
	yesterdayStats := make(map[int64]store.AppDayStats, len(yesterday))
	for _, stats := range yesterday {
		yesterdayStats[stats.AppID] = stats
	}
	type entry struct {
		stats store.AppDayStats
		p95   float64
	}
	entries := make([]entry, 0, len(today))
	for _, stats := range today {
		p95, _ := stats.Latency.Percentile(95)
		entries = append(entries, entry{stats: stats, p95: p95})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].stats.Requests > entries[j].stats.Requests })
	var lines []string
	selected := 0
	for _, item := range entries {
		interesting := item.stats.Requests > 0 && item.stats.S5xx*100 >= item.stats.Requests
		if previous, ok := yesterdayStats[item.stats.AppID]; ok && item.stats.Requests > 0 && previous.Requests > 0 {
			p95, _ := previous.Latency.Percentile(95)
			if p95 > 0 && item.p95 > 1.5*p95 {
				interesting = true
			}
		}
		if selected >= 5 && !interesting {
			continue
		}
		selected++
		rate := 0.0
		if item.stats.Requests > 0 {
			rate = 100 * float64(item.stats.S5xx) / float64(item.stats.Requests)
		}
		lines = append(lines, fmt.Sprintf("  %s: %s requests, %.1f%% errors, p95 %.0f ms",
			item.stats.Name, countText(item.stats.Requests), rate, item.p95))
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{"Apps:"}, lines...)
}

// diskLines returns the filesystem section of the digest.
func diskLines(disks map[string][]store.DiskMinute, now time.Time) []string {
	names := make([]string, 0, len(disks))
	for name := range disks {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		samples := disks[name]
		if len(samples) == 0 {
			continue
		}
		first, latest := samples[0], samples[len(samples)-1]
		total := latest.UsedBytes + latest.AvailBytes
		if total <= 0 {
			continue
		}
		usage := 100 * float64(latest.UsedBytes) / float64(total)
		line := fmt.Sprintf("  %s: %.0f%% used", name, usage)
		growth := latest.UsedBytes - first.UsedBytes
		if growth != 0 {
			line += fmt.Sprintf(" (%+s in 24 h)", bytesText(abs(growth)))
		}
		if growth > 0 {
			hours := latest.TS.Sub(first.TS).Hours()
			if hours > 0 {
				perHour := float64(growth) / hours
				days := float64(latest.SizeBytes-latest.UsedBytes) / perHour / 24
				if days >= 0 && days < 3650 {
					line += fmt.Sprintf(", full in about %.0f days", days)
				}
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{"Filesystems:"}, lines...)
}

// certLines returns the certificate section of the digest.
func certLines(certs []store.Cert, now time.Time) []string {
	var lines []string
	for _, cert := range certs {
		if cert.NotAfter == nil {
			continue
		}
		days := cert.NotAfter.Sub(now).Hours() / 24
		if days > 30 {
			continue
		}
		if days < 0 {
			lines = append(lines, "  "+cert.Domain+": expired")
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: expires in %d days", cert.Domain, int(days)))
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{"Certificates:"}, lines...)
}

// unitLines returns the failed-unit section of the digest.
func unitLines(units []store.Unit) []string {
	var lines []string
	for _, unit := range units {
		if unit.ActiveState != "failed" {
			continue
		}
		line := "  " + unit.Name
		if unit.Result != "" {
			line += " (" + unit.Result + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{"Failed units:"}, lines...)
}

// sortedIncidents returns the open incidents ordered by severity and time.
func sortedIncidents(open map[store.IncidentKey]store.Incident) []store.Incident {
	incidents := make([]store.Incident, 0, len(open))
	for _, incident := range open {
		incidents = append(incidents, incident)
	}
	sort.Slice(incidents, func(i, j int) bool {
		if incidents[i].Severity != incidents[j].Severity {
			return incidents[i].Severity == store.SeverityCritical
		}
		return incidents[i].OpenedAt.Before(incidents[j].OpenedAt)
	})
	return incidents
}

// subjectName returns the app name of an incident, the configured
// display name for its subject, or the subject itself.
func (c *Composer) subjectName(incident store.Incident) string {
	details := map[string]any{}
	if len(incident.Details) > 0 {
		_ = json.Unmarshal(incident.Details, &details)
	}
	if name, ok := details["app"].(string); ok && name != "" {
		return name
	}
	if name := c.cfg.SubjectNames.Display(incident.Subject); name != "" && name != incident.Subject {
		return name
	}
	return discovery.DisplaySubject(incident.Subject)
}

// sortedKeys returns the keys of a map in order.
func sortedKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// durationText returns a compact duration.
func durationText(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	minutes := int(d.Round(time.Minute) / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%d min", minutes)
	}
	return fmt.Sprintf("%d h %d min", minutes/60, minutes%60)
}

// bytesText formats a byte count.
func bytesText(value int64) string {
	number := float64(value)
	if number <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	unit := 0
	for number >= 1024 && unit < len(units)-1 {
		number /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%.0f %s", number, units[unit])
	}
	return fmt.Sprintf("%.1f %s", number, units[unit])
}

// countText formats a request count.
func countText(value int64) string {
	if value >= 1000 {
		return fmt.Sprintf("%.1fk", float64(value)/1000)
	}
	return fmt.Sprintf("%d", value)
}

// abs returns the absolute value.
func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
