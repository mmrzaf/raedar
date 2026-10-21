package notify

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// Severity indicators.
const (
	emojiCritical = "🔴"
	emojiWarning  = "🟠"
	emojiResolved = "✅"
)

// maxSMS is the longest SMS message.
const maxSMS = 160

// maxGroup is the number of incidents one message lists.
const maxGroup = 5

// group is the incidents of one kind of change that are sent together.
type group struct {
	// kind is what the group announces.
	kind store.DeliveryKind
	// incidents are the incidents in the group.
	incidents []store.Incident
	// reopened marks the incidents that opened again.
	reopened map[int64]bool
}

// sorted returns the incidents ordered by severity, then by opening time.
func (g group) sorted() []store.Incident {
	incidents := append([]store.Incident(nil), g.incidents...)
	sortIncidents(incidents)
	return incidents
}

// sortIncidents orders the incidents by severity, then by opening time.
func sortIncidents(incidents []store.Incident) {
	for i := 1; i < len(incidents); i++ {
		for j := i; j > 0; j-- {
			if !incidentBefore(incidents[j], incidents[j-1]) {
				break
			}
			incidents[j], incidents[j-1] = incidents[j-1], incidents[j]
		}
	}
}

// incidentBefore reports whether a comes before b.
func incidentBefore(a, b store.Incident) bool {
	if severityRank(a.Severity) != severityRank(b.Severity) {
		return severityRank(a.Severity) < severityRank(b.Severity)
	}
	return a.OpenedAt.Before(b.OpenedAt)
}

// severityRank orders severities, critical first.
func severityRank(severity store.Severity) int {
	if severity == store.SeverityCritical {
		return 0
	}
	return 1
}

// document is the webhook JSON body.
type document struct {
	Version   int                `json:"version"`
	Host      string             `json:"host"`
	Kind      string             `json:"kind"`
	SentAt    time.Time          `json:"sent_at"`
	URL       string             `json:"url,omitempty"`
	Text      string             `json:"text,omitempty"`
	Incidents []documentIncident `json:"incidents,omitempty"`
}

// documentIncident is one incident in the webhook body.
type documentIncident struct {
	ID         int64          `json:"id"`
	Rule       string         `json:"rule"`
	Subject    string         `json:"subject"`
	App        string         `json:"app"`
	Severity   store.Severity `json:"severity"`
	OpenedAt   time.Time      `json:"opened_at"`
	ResolvedAt *time.Time     `json:"resolved_at"`
	Reopened   bool           `json:"reopened"`
	Title      string         `json:"title"`
	Summary    string         `json:"summary"`
	Context    []string       `json:"context"`
	Details    map[string]any `json:"details"`
}

// formatter turns incidents into messages.
type formatter struct {
	// host is the host name in messages.
	host string
	// publicURL is the UI's public URL, empty when messages contain no links.
	publicURL string
	// location is the time zone of the displayed times.
	location *time.Location
	// deploys holds the deploy context of an app.
	deploys map[int64]deployContext
	// names renames an incident subject for display.
	names func(string) string
	// now is the time the message is built at.
	now time.Time
}

// subjectName returns the display name of an incident: the app name when
// the incident has one, otherwise the configured subject name or the
// subject itself.
func (f formatter) subjectName(incident store.Incident) string {
	if name := incidentApp(incident); name != "" {
		return name
	}
	subject := incident.Subject
	if f.names != nil {
		if name := f.names(subject); name != "" {
			return name
		}
	}
	return subject
}

// deployContext describes the deploy that preceded an incident.
type deployContext struct {
	app     string
	from    string
	to      string
	minutes int
}

// text returns the Telegram text of a group.
func (f formatter) text(g group) string {
	incidents := g.sorted()
	if len(incidents) > maxGroup {
		incidents = incidents[:maxGroup]
	}
	var blocks []string
	for _, incident := range incidents {
		blocks = append(blocks, f.block(incident, g.kind, g.reopened[incident.ID]))
	}
	if more := len(g.incidents) - len(incidents); more > 0 {
		blocks = append(blocks, fmt.Sprintf("and %d more incidents", more))
	}
	return strings.Join(blocks, "\n\n")
}

// block returns one incident's Telegram block.
func (f formatter) block(incident store.Incident, kind store.DeliveryKind, reopened bool) string {
	header := f.incidentHeader(incident, kind)
	if kind == store.DeliveryResolved {
		return header + " resolved after " + durationText(incidentDuration(incident))
	}
	var lines []string
	lines = append(lines, header)
	if summary := incidentSummary(incident); summary != "" {
		lines = append(lines, summary)
	}
	since := "since " + f.clock(incident.OpenedAt)
	if deploy, ok := f.deploys[appIDOf(incident)]; ok && kind != store.DeliveryResolved {
		since += fmt.Sprintf(" · %d min after deploy %s %s → %s", deploy.minutes, deploy.app, deploy.from, deploy.to)
	}
	lines = append(lines, since)
	lines = append(lines, contextLines(incident)...)
	if f.publicURL != "" && incident.ID != 0 {
		lines = append(lines, fmt.Sprintf("%s/incidents/%d", f.publicURL, incident.ID))
	}
	_ = reopened
	return strings.Join(lines, "\n")
}

// incidentHeader returns the first line of an incident's message.
func (f formatter) incidentHeader(incident store.Incident, kind store.DeliveryKind) string {
	emoji := emojiWarning
	if incident.Severity == store.SeverityCritical {
		emoji = emojiCritical
	}
	if kind == store.DeliveryResolved {
		emoji = emojiResolved
	}
	return fmt.Sprintf("%s %s · %s — %s", emoji, f.host, f.subjectName(incident), incident.Summary)
}

// clock returns a time of day in the configured zone.
func (f formatter) clock(at time.Time) string {
	return at.In(f.location).Format("15:04")
}

// incidentDuration returns how long an incident lasted.
func incidentDuration(incident store.Incident) time.Duration {
	if incident.ResolvedAt == nil {
		return 0
	}
	return incident.ResolvedAt.Sub(incident.OpenedAt)
}

// durationText returns a compact duration such as "18 min" or "2 h 5 min".
func durationText(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	minutes := int(d.Round(time.Minute) / time.Minute)
	hours := minutes / 60
	minutes -= hours * 60
	switch {
	case hours == 0:
		return fmt.Sprintf("%d min", minutes)
	case minutes == 0:
		return fmt.Sprintf("%d h", hours)
	default:
		return fmt.Sprintf("%d h %d min", hours, minutes)
	}
}

// incidentApp returns the app name of an incident from its details.
func incidentApp(incident store.Incident) string {
	return stringDetail(incident, "app")
}

// appIDOf returns the app ID of an incident, 0 when it has none.
func appIDOf(incident store.Incident) int64 {
	if incident.AppID == nil {
		return 0
	}
	return *incident.AppID
}

// incidentSummary returns the numeric line of an incident.
func incidentSummary(incident store.Incident) string {
	details := decodeDetails(incident)
	switch incident.Rule {
	case "route.errors":
		line := fmt.Sprintf("%s of requests fail", percent(details["rate"], 1))
		if baseline, ok := number(details["baseline"]); ok && baseline > 0 {
			line += fmt.Sprintf(" (usual at this hour: %s)", percent(details["baseline"], 1))
		}
		return line
	case "route.slow":
		line := fmt.Sprintf("p95 latency %s", milliseconds(details["p95_ms"]))
		if baseline, ok := number(details["baseline_ms"]); ok && baseline > 0 {
			line += fmt.Sprintf(" (usual at this hour: %s)", milliseconds(details["baseline_ms"]))
		}
		return line
	case "route.down":
		requests, _ := number(details["requests"])
		noBackend, _ := number(details["no_backend"])
		if requests > 0 {
			return fmt.Sprintf("no backend for %s of requests", percent(noBackend/requests, 0))
		}
		return "requests reached no backend"
	case "container.crashed":
		return exitText(details)
	case "container.restarting":
		crashes, _ := number(details["crashes"])
		return fmt.Sprintf("%d crashes in 10 minutes", int(crashes))
	case "container.stopped":
		return "stopped after a crash, " + exitText(details)
	case "container.unhealthy":
		return "health check is failing"
	case "container.memory":
		return fmt.Sprintf("memory at %s of the limit (%s of %s)",
			percent(details["usage"], 0), bytesText(details["used_bytes"]), bytesText(details["limit_bytes"]))
	case "host.disk", "host.inodes":
		return fmt.Sprintf("%s used (free %s)", percent(details["usage"], 1), bytesText(details["avail_bytes"]))
	case "host.disk_forecast":
		if days, ok := number(details["days_until_full"]); ok {
			return fmt.Sprintf("full in about %.1f days (growing %s a day)", days, bytesText(details["growth_bytes_per_day"]))
		}
		return "growing towards a full filesystem"
	case "host.memory":
		return fmt.Sprintf("%s of memory available", percent(details["available_percent"], 1))
	case "host.unit_failed":
		line := "unit failed"
		if result := stringDetail(incident, "result"); result != "" {
			line += ": " + result
		}
		if exit, ok := number(details["exit_status"]); ok {
			line += fmt.Sprintf(", exit status %d", int(exit))
		}
		return line
	case "cert.expiry":
		if days, ok := number(details["days_left"]); ok {
			if days < 0 {
				return "expired"
			}
			return fmt.Sprintf("expires in %d days", int(math.Ceil(days)))
		}
		return "expiring"
	case "cert.invalid":
		if stringDetail(incident, "status") == "default" {
			return "Traefik serves its default certificate"
		}
		return "the certificate does not cover the domain"
	case "peer.silent":
		return "no heartbeat received"
	case "raedar.source":
		return "source is failing (" + stringDetail(incident, "status") + ")"
	}
	return ""
}

// contextLines returns the context lines of an incident.
func contextLines(incident store.Incident) []string {
	details := decodeDetails(incident)
	switch incident.Rule {
	case "route.errors":
		if path, ok := details["path"].(string); ok && path != "" {
			return []string{"most errors: " + path}
		}
	case "route.slow":
		if path, ok := details["path"].(string); ok && path != "" {
			return []string{fmt.Sprintf("slowest: %s (%s)", path, milliseconds(details["path_p95_ms"]))}
		}
	case "container.crashed", "container.restarting", "container.stopped":
		return []string{exitText(details)}
	case "host.disk", "host.disk_forecast":
		var lines []string
		if growth, ok := number(details["growth_bytes_per_day"]); ok && growth > 0 {
			lines = append(lines, fmt.Sprintf("grown %s in 24 hours", bytesText(growth)))
		}
		if avail, ok := number(details["avail_bytes"]); ok {
			lines = append(lines, "free "+bytesText(avail))
		}
		return lines
	case "host.unit_failed":
		if result := details["result"].(string); result != "" {
			return []string{"result: " + result}
		}
	case "cert.expiry", "cert.invalid":
		var lines []string
		if issuer := details["issuer"].(string); issuer != "" {
			lines = append(lines, "issuer: "+issuer)
		}
		if notAfter, ok := details["not_after"].(string); ok && notAfter != "" {
			lines = append(lines, "expires: "+notAfter)
		}
		return lines
	}
	return nil
}

// exitText describes a container's exit.
func exitText(details map[string]any) string {
	line := "exit code unknown"
	if exit, ok := number(details["exit_code"]); ok {
		line = fmt.Sprintf("exit code %d", int(exit))
	}
	if oom, ok := details["oom"].(bool); ok && oom {
		line += ", out of memory"
	}
	return line
}

// smsText returns the one-line SMS of a group.
func (f formatter) smsText(g group) string {
	incidents := g.sorted()
	var text string
	switch len(incidents) {
	case 0:
		return ""
	case 1:
		incident := incidents[0]
		severity := strings.ToUpper(string(incident.Severity))
		name := f.subjectName(incident)
		summary := smsSummary(incident)
		if g.kind == store.DeliveryResolved {
			minutes := int(incidentDuration(incident).Round(time.Minute) / time.Minute)
			text = fmt.Sprintf("RAEDAR %s RESOLVED %s: %s (%d min)", f.host, name, summary, minutes)
		} else {
			text = fmt.Sprintf("RAEDAR %s %s %s: %s since %s", f.host, severity, name, summary, f.clock(incident.OpenedAt))
		}
	default:
		severity := "WARNING"
		for _, incident := range incidents {
			if incident.Severity == store.SeverityCritical {
				severity = "CRITICAL"
				break
			}
		}
		var names []string
		limit := len(incidents)
		if limit > 2 {
			limit = 2
		}
		for _, incident := range incidents[:limit] {
			names = append(names, f.subjectName(incident)+" "+shortRule(incident.Rule))
		}
		if remaining := len(incidents) - limit; remaining > 0 {
			names = append(names, fmt.Sprintf("+%d", remaining))
		}
		noun := "problems"
		if g.kind == store.DeliveryResolved {
			noun = "resolved"
		}
		text = fmt.Sprintf("RAEDAR %s %s %d %s: %s", f.host, severity, len(incidents), noun, strings.Join(names, ", "))
	}
	return truncateSMS(text)
}

// smsSummary returns the short SMS phrase of an incident.
func smsSummary(incident store.Incident) string {
	details := decodeDetails(incident)
	switch incident.Rule {
	case "route.errors":
		return fmt.Sprintf("%s of requests fail", percent(details["rate"], 0))
	case "route.slow":
		return fmt.Sprintf("slow p95 %s", milliseconds(details["p95_ms"]))
	case "route.down":
		requests, _ := number(details["requests"])
		noBackend, _ := number(details["no_backend"])
		if requests > 0 {
			return fmt.Sprintf("no backend for %s of requests", percent(noBackend/requests, 0))
		}
		return "no backend"
	case "container.crashed":
		return "container crashed (" + exitText(details) + ")"
	case "container.restarting":
		return "container restarting"
	case "container.stopped":
		return "container stopped"
	case "container.unhealthy":
		return "health check failing"
	case "container.memory":
		return fmt.Sprintf("memory at %s", percent(details["usage"], 0))
	case "host.disk":
		return fmt.Sprintf("disk %s full", percent(details["usage"], 0))
	case "host.inodes":
		return fmt.Sprintf("inodes %s used", percent(details["usage"], 0))
	case "host.disk_forecast":
		return "disk filling up"
	case "host.memory":
		return "memory low"
	case "host.unit_failed":
		return "unit failed"
	case "cert.expiry":
		return "certificate expiring"
	case "cert.invalid":
		return "certificate invalid"
	case "peer.silent":
		return "peer silent"
	case "raedar.source":
		return "source failing"
	}
	return incident.Summary
}

// shortRule returns a one-word label for a rule.
func shortRule(rule string) string {
	switch rule {
	case "route.down":
		return "down"
	case "route.errors":
		return "errors"
	case "route.slow":
		return "slow"
	case "container.crashed":
		return "crash"
	case "container.restarting":
		return "restarting"
	case "container.stopped":
		return "stopped"
	case "container.unhealthy":
		return "unhealthy"
	case "container.memory":
		return "memory"
	case "host.disk":
		return "disk"
	case "host.inodes":
		return "inodes"
	case "host.disk_forecast":
		return "disk"
	case "host.memory":
		return "memory"
	case "host.unit_failed":
		return "failed"
	case "cert.expiry":
		return "certificate"
	case "cert.invalid":
		return "certificate"
	case "peer.silent":
		return "silent"
	case "raedar.source":
		return "source"
	}
	return rule
}

// truncateSMS cuts a message to the SMS length, keeping it valid UTF-8.
func truncateSMS(text string) string {
	if len(text) <= maxSMS {
		return text
	}
	runes := []rune(text)
	for len(string(runes)) > maxSMS {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// document returns the webhook body of a group.
func (f formatter) document(g group) []byte {
	body := document{
		Version: 1,
		Host:    f.host,
		Kind:    "incident." + string(g.kind),
		SentAt:  f.now,
	}
	incidents := g.sorted()
	if len(incidents) > maxGroup {
		incidents = incidents[:maxGroup]
	}
	for _, incident := range incidents {
		body.Incidents = append(body.Incidents, documentIncident{
			ID:         incident.ID,
			Rule:       incident.Rule,
			Subject:    incident.Subject,
			App:        incidentApp(incident),
			Severity:   incident.Severity,
			OpenedAt:   incident.OpenedAt,
			ResolvedAt: incident.ResolvedAt,
			Reopened:   g.reopened[incident.ID],
			Title:      incident.Summary,
			Summary:    incidentSummary(incident),
			Context:    contextLines(incident),
			Details:    decodeDetails(incident),
		})
	}
	if len(body.Incidents) > 0 && f.publicURL != "" && body.Incidents[0].ID != 0 {
		body.URL = fmt.Sprintf("%s/incidents/%d", f.publicURL, body.Incidents[0].ID)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return []byte(`{"version":1}`)
	}
	return data
}

// decodeDetails decodes an incident's details.
func decodeDetails(incident store.Incident) map[string]any {
	details := map[string]any{}
	if len(incident.Details) > 0 {
		if err := json.Unmarshal(incident.Details, &details); err != nil {
			return map[string]any{}
		}
	}
	return details
}

// number reads a JSON number from details.
func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	}
	return 0, false
}

// stringDetail returns one string detail of an incident.
func stringDetail(incident store.Incident, key string) string {
	details := decodeDetails(incident)
	value, _ := details[key].(string)
	return value
}

// percent formats a fraction as a percentage.
func percent(value any, decimals int) string {
	fraction, ok := number(value)
	if !ok {
		return "0%"
	}
	return strconv.FormatFloat(fraction*100, 'f', decimals, 64) + "%"
}

// milliseconds formats a millisecond value.
func milliseconds(value any) string {
	number, ok := number(value)
	if !ok {
		return "0 ms"
	}
	if number >= 60000 {
		return "over 60 s"
	}
	return fmt.Sprintf("%.0f ms", number)
}

// bytesText formats a byte count in binary units.
func bytesText(value any) string {
	number, ok := number(value)
	if !ok || number <= 0 {
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
