// The gate is the single place that decides whether an incident change
// becomes a page right now, is deferred, or is suppressed.
package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// GateStatus is the disposition the gate takes on one
// (incident, change-kind, channel) tuple.
type GateStatus string

const (
	// GateRecorded sends the page now.
	GateRecorded GateStatus = "recorded"
	// GateSilenced suppresses the page because a silence covers the
	// incident. No message is ever sent.
	GateSilenced GateStatus = "silenced"
	// GateAcked suppresses the page because the operator acked the
	// incident. No message is ever sent.
	GateAcked GateStatus = "acked"
	// GateHeld defers the page until the quiet window ends.
	GateHeld GateStatus = "held"
	// GateCooldown defers the page until the cooldown expires.
	GateCooldown GateStatus = "cooldown"
)

// Decision is what the gate decided for one tuple. ReadyAt is set for the
// deferred statuses (held, cooldown) and is the time the deferred page
// becomes eligible to send. Reason is shown in the deliveries page.
type Decision struct {
	Status  GateStatus
	Channel Kind
	Reason  string
	ReadyAt time.Time
	// Drop suppresses a held or cooldown page without queueing a catch-up.
	// The gate sets it for a flapping incident that has used its page budget.
	Drop bool
}

// DecisionInputs is the input for one gate evaluation.
type DecisionInputs struct {
	Incident   store.Incident
	ChangeKind store.DeliveryKind
	Channel    Kind
	Now        time.Time
}

// GateStore is the slice of the store the gate needs.
type GateStore interface {
	// FindActiveSilence reports the most specific active silence for the
	// incident's rule, subject and app at now, or nil when none matches.
	FindActiveSilence(ctx context.Context, rule, subject string, appID *int64, now time.Time) (*store.Silence, error)
	// IncidentByID returns one incident with its current ack state.
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	// LastSent returns the most recent sent delivery for the (rule,
	// subject) pair, or nil when none exists.
	LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error)
}

// Gate decides the disposition of each incident change.
//
// Order of checks:
//
//  1. acked      the operator already saw this incident
//  2. silenced   a silence covers the incident
//  3. held       the quiet window is open and the incident is not critical
//  4. flap       the same (rule, subject) already used its page budget in the
//     flap window; the page is recorded and dropped
//  5. cooldown   an opening page for the same (rule, subject) went out within
//     the cooldown; escalations and resolutions are never delayed by it
//  6. recorded   send now
//
// A failing store lookup never blocks a page: the gate logs it and carries
// on to the next check, because a missed page costs more than a duplicate.
type Gate struct {
	Store  GateStore
	Logger *slog.Logger

	// Cooldown is the minimum time between two pages for the same
	// (rule, subject). CooldownByRule overrides it per rule.
	Cooldown       time.Duration
	CooldownByRule map[string]time.Duration

	// FlapWindow and FlapMaxPages keep a flapping subject to at most
	// FlapMaxPages opening pages per window. A resolution is also suppressed
	// once the subject has flapped. Zero disables the check; RecentOpenCount
	// is supplied by the store.
	FlapWindow      time.Duration
	FlapMaxPages    int
	RecentOpenCount func(ctx context.Context, rule, subject, channel string, since time.Time) (int, error)

	// PageCritical lets critical incidents bypass the quiet window.
	PageCritical bool

	// QuietFrom and QuietTo form the optional quiet window in QuietZone.
	// Both are required together (the configuration enforces it); an
	// overnight window (from > to) spans midnight.
	QuietFrom *time.Time
	QuietTo   *time.Time
	QuietZone *time.Location
}

// Evaluate reports the disposition of one incident change.
func (g *Gate) Evaluate(ctx context.Context, inputs DecisionInputs) Decision {
	now := inputs.Now

	// 1. an acked incident never pages, at any severity.
	if g.Store != nil {
		current, err := g.Store.IncidentByID(ctx, inputs.Incident.ID)
		if err != nil {
			g.warn("gate: read incident", err)
		} else if current != nil && current.Acked() {
			return Decision{Status: GateAcked, Channel: inputs.Channel, Reason: "acked"}
		}
	}

	// 2. an active silence covers the incident.
	if g.Store != nil {
		silence, err := g.Store.FindActiveSilence(
			ctx, inputs.Incident.Rule, inputs.Incident.Subject, inputs.Incident.AppID, now)
		if err != nil {
			g.warn("gate: read silences", err)
		} else if silence != nil {
			return Decision{Status: GateSilenced, Channel: inputs.Channel, Reason: silence.Reason}
		}
	}

	// 3. quiet hours hold warnings until the window closes. Critical
	// incidents bypass the hold unless the operator disabled that.
	if endsAt, inside := g.quietEndsAt(now); inside {
		if !(g.PageCritical && inputs.Incident.Severity == store.SeverityCritical) {
			return Decision{Status: GateHeld, Channel: inputs.Channel, Reason: "quiet hours", ReadyAt: endsAt}
		}
	}

	// 4. a subject that has used its page budget in the flap window is
	// recorded and dropped. Resolutions are dropped too once the subject has
	// flapped, so a crash loop does not produce a resolution message for
	// every cycle.
	if g.FlapWindow > 0 && g.FlapMaxPages > 0 && g.RecentOpenCount != nil &&
		(inputs.ChangeKind == store.DeliveryOpened || inputs.ChangeKind == store.DeliveryResolved) {
		count, err := g.RecentOpenCount(ctx, inputs.Incident.Rule, inputs.Incident.Subject,
			string(inputs.Channel), now.Add(-g.FlapWindow))
		if err != nil {
			g.warn("gate: count recent pages", err)
		} else if inputs.ChangeKind == store.DeliveryOpened && count >= g.FlapMaxPages {
			return Decision{Status: GateCooldown, Channel: inputs.Channel, Reason: "flapping", Drop: true}
		} else if inputs.ChangeKind == store.DeliveryResolved && count > g.FlapMaxPages {
			return Decision{Status: GateCooldown, Channel: inputs.Channel, Reason: "flapping", Drop: true}
		}
	}

	// 5. an opening page for the same (rule, subject) inside the cooldown is
	// deferred to the end of that cooldown.
	if window := g.cooldownFor(inputs.Incident.Rule); window > 0 && g.Store != nil &&
		inputs.ChangeKind == store.DeliveryOpened {
		last, err := g.Store.LastSent(ctx, inputs.Incident.Rule, inputs.Incident.Subject)
		if err != nil {
			g.warn("gate: read last page", err)
		} else if last != nil && last.SentAt != nil {
			if readyAt := last.SentAt.Add(window); now.Before(readyAt) {
				return Decision{Status: GateCooldown, Channel: inputs.Channel, Reason: "pager cooldown", ReadyAt: readyAt}
			}
		}
	}

	// 6. send now.
	return Decision{Status: GateRecorded, Channel: inputs.Channel}
}

// warn logs a failed store lookup.
func (g *Gate) warn(message string, err error) {
	if g.Logger != nil {
		g.Logger.Warn(message, slog.String("error", err.Error()))
	}
}

// cooldownFor returns the cooldown that applies to a rule.
func (g *Gate) cooldownFor(rule string) time.Duration {
	if override, ok := g.CooldownByRule[rule]; ok {
		return override
	}
	return g.Cooldown
}

// quietEndsAt reports the end of the quiet window that contains now. The
// second result is false when no quiet window is configured or now is
// outside it.
func (g *Gate) quietEndsAt(now time.Time) (time.Time, bool) {
	if g.QuietFrom == nil || g.QuietTo == nil {
		return time.Time{}, false
	}
	zone := g.QuietZone
	if zone == nil {
		zone = time.UTC
	}
	local := now.In(zone)
	fromMin := g.QuietFrom.Hour()*60 + g.QuietFrom.Minute()
	toMin := g.QuietTo.Hour()*60 + g.QuietTo.Minute()
	nowMin := local.Hour()*60 + local.Minute()

	var inside bool
	switch {
	case fromMin < toMin:
		inside = nowMin >= fromMin && nowMin < toMin
	default: // overnight window
		inside = nowMin >= fromMin || nowMin < toMin
	}
	if !inside {
		return time.Time{}, false
	}

	// The window closes at the upcoming To boundary: today when it is still
	// ahead of now, otherwise tomorrow.
	// Built from calendar fields so a DST change does not shift the boundary.
	day := local.Day()
	if toMin <= nowMin {
		day++
	}
	return time.Date(local.Year(), local.Month(), day, toMin/60, toMin%60, 0, 0, zone), true
}
