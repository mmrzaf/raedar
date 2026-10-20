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
	// same (rule, subject). The gate keeps a separate cooldown row in
	// silences; phase 3 wires the configuration.
	Cooldown time.Duration

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
}

// GateStore is the slice of Store the gate uses.
type GateStore interface {
	// FindActiveSilence reports the most-specific active silence for the
	// (rule, subject) at now, or nil when nothing matches.
	FindActiveSilence(ctx context.Context, rule, subject string, now time.Time) (*store.Silence, error)
	// Acked reports the incident's ack metadata.
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	// AddDelivery records a new delivery row in any status.
	AddDelivery(ctx context.Context, delivery store.Delivery) (int64, error)
}

// Evaluate reports the disposition of inputs. The pager calls Evaluate for
// every incident × change-kind × channel tuple it would have delivered.
// Returning Decision{Status: GateRecorded, Message: msg} keeps the old
// behaviour; the spine phase returns exactly that.
func (g *Gate) Evaluate(ctx context.Context, inputs DecisionInputs, message Message) Decision {
	// Future phases will override this default. Today: always record + send.
	return Decision{Status: GateRecorded, Channel: inputs.Channel, Message: &message, Reason: ""}
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
