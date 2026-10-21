package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// GateStatus is one of the dispositions the paging gate can take on an
// (incident, change-kind, channel) tuple. The pager writes one deliveries
// row per tuple, with the matching status.
//
//	recorded	send right now
//	silenced	blocked by an active silence row, no Telegram message
//	cooldown	blocked by the per-(rule, subject) cooldown
//	held		blocked because the change falls inside the quiet hours
//	stand_down	suppressed because the host has been all-quiet long enough
//	acked		suppressed because the incident is acked
//	escalated_blocked
//				a severity escalation that the operator already saw
type GateStatus string

const (
	GateRecorded         GateStatus = "recorded"
	GateSilenced         GateStatus = "silenced"
	GateCooldown         GateStatus = "cooldown"
	GateHeld             GateStatus = "held"
	GateStandDown        GateStatus = "stand_down"
	GateAcked            GateStatus = "acked"
	GateEscalatedBlocked GateStatus = "escalated_blocked"
)

// Decision is the disposition of one (incident, change-kind, channel) tuple
// after the pager evaluated it. A nil Message means the pager must not call
// Channel.Send; the gate produces only recorded deliveries rows.
type Decision struct {
	// Status is the disposition.
	Status GateStatus
	// Channel is the channel the pager decides for (telegram / sms / webhook).
	Channel Kind
	// Message is the rendered message; nil when the pager must not send.
	Message *Message
	// Reason is a short note shown in 'raeder deliveries'.
	Reason string
}

// DecisionInputs is the input the pager passes to Gate.Evaluate. The pager
// builds one of these per (incident, change-kind, channel) and reads the
// resulting Decision.
type DecisionInputs struct {
	// Incident is the incident the change refers to.
	Incident store.Incident
	// ChangeKind is what changed: opened, escalated, resolved, digested, tested.
	ChangeKind store.DeliveryKind
	// Channel is the channel the pager would deliver to.
	Channel Kind
	// Now is the time the pager makes the decision.
	Now time.Time
}

// Gate decides the disposition of each (incident, change-kind, channel)
// tuple. Concrete decisions are layered on top of a struct value: the
// spine phase just records, the silencing phase adds ack/silence/cooldown,
// and so on. Every feature plugs into Evaluate so the pipeline stays in
// one place.
type Gate struct {
	// Store reads incidents, deliveries and silences.
	Store GateStore
	// Logger records evaluation traces (rate-limited by the caller).
	Logger *slog.Logger

	// Cooldown, when non-zero, is the minimum time between two pages for the
	// same (rule, subject). The gate looks at the most recent sent
	// delivery for the (rule, subject) and decides based on that.
	Cooldown time.Duration
	// CooldownByRule overrides Cooldown for specific rules. Use this to
	// silence route-level pages quickly (15m) while letting host-level
	// pages breathe (1h).
	CooldownByRule map[string]time.Duration

	// PageCritical, when true, lets critical incidents bypass quiet hours.
	// Critical bypass is the default.
	PageCritical bool

	// QuietFrom/QuietTo form an optional [from, to) window in QuietZone, with
	// QuietZone defaulting to UTC when nil. A nil from or to disables the
	// matching side: 22:00 → 07:00 means 22:00 to midnight and midnight to
	// 07:00 the next day.
	QuietFrom *time.Time
	QuietTo   *time.Time
	QuietZone *time.Location

	// StandDownAfter, when non-zero, is the time after which a single
	// "all quiet for N h" message may go out. The pager tracks the
	// last-sent-at per host in memory.
	StandDownAfter time.Duration
	// lastPagedAt is the time the last incident was paged successfully
	// on this host. Updated by the gate whenever a Decision returns
	// GateRecorded. Stand-down uses it to decide whether to post.
	lastPagedAt time.Time
}

// GateStore is the slice of Store the gate uses.
type GateStore interface {
	// FindActiveSilence reports the most-specific active silence for the
	// (rule, subject) at now, or nil when nothing matches.
	FindActiveSilence(ctx context.Context, rule, subject string, now time.Time) (*store.Silence, error)
	// Acked returns the incident with its ack metadata; the gate reads
	// ack_at to decide whether to suppress an incident entirely.
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	// LastSent returns the most recent sent delivery for the (rule,
	// subject) pair, or nil when no row exists. Used by the cooldown
	// branch.
	LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error)
	// OpenIncidentCount returns the number of currently open incidents.
	// Used by the stand-down branch to avoid posting the "all quiet"
	// message while a problem is still live.
	OpenIncidentCount(ctx context.Context) (int, error)
}

// Evaluate reports the disposition of inputs. The pager calls Evaluate for
// every incident × change-kind × channel tuple it would have delivered.
//
// Order of checks:
//  1. acked           the incident is acked, never page
//  2. silenced        an active silence covers (rule, subject)
//  3. held            quiet hours hold the page (critical bypasses by default)
//  4. cooldown        pager cooldown covers (rule, subject)
//  5. stand_down      stand-down window absorbs the page
//  6. recorded        send the message (or — for held/cooldown/stand_down —
//     deliver the deferred message at the boundary)
func (g *Gate) Evaluate(ctx context.Context, inputs DecisionInputs, message Message) Decision {
	// 1. acked
	if inputs.Incident.Acked() && g.Store != nil {
		if fresh, err := g.Store.IncidentByID(ctx, inputs.Incident.ID); err == nil && fresh != nil && fresh.Acked() {
			return Decision{Status: GateAcked, Channel: inputs.Channel, Reason: "acked"}
		}
	}
	// 2. silenced (only rule+subject can be silenced; the pager always
	// passes both, even when subject is "host" or a disk name).
	if g.Store != nil {
		if silence, err := g.Store.FindActiveSilence(ctx, inputs.Incident.Rule, inputs.Incident.Subject, inputs.Now); err == nil && silence != nil {
			return Decision{Status: GateSilenced, Channel: inputs.Channel, Reason: silence.Reason}
		}
	}
	// 3. held: quiet hours hold the page. Critical incidents bypass the
	// hold when PageCritical is true (the default).
	if g.inQuietWindow(inputs.Now) {
		bypass := g.PageCritical && inputs.Incident.Severity == store.SeverityCritical
		if !bypass {
			return Decision{Status: GateHeld, Channel: inputs.Channel, Reason: "quiet hours"}
		}
	}
	// 4. cooldown: the most recent sent delivery for the same (rule,
	// subject) is younger than the configured cooldown.
	if g.cooldownFor(inputs.Incident.Rule) > 0 && g.Store != nil {
		if last, err := g.Store.LastSent(ctx, inputs.Incident.Rule, inputs.Incident.Subject); err == nil && last != nil && last.SentAt != nil {
			if inputs.Now.Sub(*last.SentAt) < g.cooldownFor(inputs.Incident.Rule) {
				return Decision{Status: GateCooldown, Channel: inputs.Channel, Reason: "pager cooldown"}
			}
		}
	}
	// 5. stand-down: the host has been all-quiet for StandDownAfter and
	// the queue is otherwise empty. Critical incidents bypass this so a
	// flapping host cannot stay silent while it hurts.
	if g.StandDownAfter > 0 && g.Store != nil {
		if g.PageCritical && inputs.Incident.Severity == store.SeverityCritical {
			// critical bypasses stand-down
		} else if !g.lastPagedAt.IsZero() && inputs.Now.Sub(g.lastPagedAt) > g.StandDownAfter {
			if count, err := g.Store.OpenIncidentCount(ctx); err == nil && count <= 0 {
				return Decision{Status: GateStandDown, Channel: inputs.Channel, Reason: "stand-down"}
			}
		}
	}
	// 6. default: record + send. Update lastPagedAt so the next
	// stand-down check is anchored to the most recent page.
	g.lastPagedAt = inputs.Now
	return Decision{Status: GateRecorded, Channel: inputs.Channel, Message: &message, Reason: ""}
}

// cooldownFor returns the cooldown that applies to a given rule, with the
// per-rule override taking precedence over the global default.
func (g *Gate) cooldownFor(rule string) time.Duration {
	if override, ok := g.CooldownByRule[rule]; ok {
		return override
	}
	return g.Cooldown
}

// inQuietWindow reports whether at now falls inside the configured quiet
// hours. A nil QuietFrom or QuietTo disables the matching side. The
// function tolerates overnight windows (e.g. 22:00 to 07:00).
func (g *Gate) inQuietWindow(now time.Time) bool {
	if g.QuietFrom == nil && g.QuietTo == nil {
		return false
	}
	zone := g.QuietZone
	if zone == nil {
		zone = time.UTC
	}
	local := now.In(zone)
	h, m, _ := local.Clock()
	nowMin := h*60 + m
	fromMin := -1
	toMin := -1
	if g.QuietFrom != nil {
		fromMin = g.QuietFrom.Hour()*60 + g.QuietFrom.Minute()
	}
	if g.QuietTo != nil {
		toMin = g.QuietTo.Hour()*60 + g.QuietTo.Minute()
	}
	switch {
	case fromMin < 0:
		return nowMin < toMin
	case toMin < 0:
		return nowMin >= fromMin
	case fromMin == toMin:
		return false
	case fromMin < toMin:
		return nowMin >= fromMin && nowMin < toMin
	default: // overnight window
		return nowMin >= fromMin || nowMin < toMin
	}
}
