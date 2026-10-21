// Package-level gate: the single place that decides whether an incident
// change becomes a page right now, is deferred, or is suppressed.
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
//  4. cooldown   the same (rule, subject) paged within the cooldown
//  5. recorded   send now
type Gate struct {
	Store  GateStore
	Logger *slog.Logger

	// Cooldown is the minimum time between two pages for the same
	// (rule, subject). CooldownByRule overrides it per rule.
	Cooldown       time.Duration
	CooldownByRule map[string]time.Duration

	// PageCritical lets critical incidents bypass the quiet window.
	PageCritical bool

	// QuietFrom and QuietTo form the optional quiet window in QuietZone. A
	// nil side disables that edge; an overnight window (from > to) spans
	// midnight.
	QuietFrom *time.Time
	QuietTo   *time.Time
	QuietZone *time.Location
}

// Evaluate reports the disposition of one incident change.
func (g *Gate) Evaluate(ctx context.Context, inputs DecisionInputs) Decision {
	now := inputs.Now

	// 1. an acked incident never pages, at any severity.
	if g.Store != nil {
		if current, err := g.Store.IncidentByID(ctx, inputs.Incident.ID); err == nil &&
			current != nil && current.Acked() {
			return Decision{Status: GateAcked, Channel: inputs.Channel, Reason: "acked"}
		}
	}

	// 2. an active silence covers the incident.
	if g.Store != nil {
		if silence, err := g.Store.FindActiveSilence(
			ctx, inputs.Incident.Rule, inputs.Incident.Subject, inputs.Incident.AppID, now,
		); err == nil && silence != nil {
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

	// 4. a page for the same (rule, subject) inside the cooldown is deferred
	// to the end of that cooldown.
	if window := g.cooldownFor(inputs.Incident.Rule); window > 0 && g.Store != nil {
		if last, err := g.Store.LastSent(ctx, inputs.Incident.Rule, inputs.Incident.Subject); err == nil &&
			last != nil && last.SentAt != nil {
			if readyAt := last.SentAt.Add(window); now.Before(readyAt) {
				return Decision{Status: GateCooldown, Channel: inputs.Channel, Reason: "pager cooldown", ReadyAt: readyAt}
			}
		}
	}

	// 5. send now.
	return Decision{Status: GateRecorded, Channel: inputs.Channel}
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
	if g.QuietFrom == nil && g.QuietTo == nil {
		return time.Time{}, false
	}
	zone := g.QuietZone
	if zone == nil {
		zone = time.UTC
	}
	local := now.In(zone)

	fromMin, toMin := -1, -1
	if g.QuietFrom != nil {
		fromMin = g.QuietFrom.Hour()*60 + g.QuietFrom.Minute()
	}
	if g.QuietTo != nil {
		toMin = g.QuietTo.Hour()*60 + g.QuietTo.Minute()
	}
	nowMin := local.Hour()*60 + local.Minute()

	inWindow := func() bool {
		switch {
		case fromMin < 0:
			return nowMin < toMin
		case toMin < 0:
			return nowMin >= fromMin
		case fromMin == toMin:
			return false
		case fromMin < toMin:
			return nowMin >= fromMin && nowMin < toMin
		default:
			return nowMin >= fromMin || nowMin < toMin
		}
	}
	if !inWindow() {
		return time.Time{}, false
	}

	// The window closes at the upcoming To boundary. It is today when that
	// is still ahead of now, otherwise tomorrow.
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	switch {
	case toMin < 0:
		// Only a start is set, so the window never closes on its own.
		return time.Time{}, false
	case toMin > nowMin:
		return midnight.Add(time.Duration(toMin) * time.Minute), true
	default:
		return midnight.AddDate(0, 0, 1).Add(time.Duration(toMin) * time.Minute), true
	}
}
