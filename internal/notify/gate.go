// The gate is the single place that decides whether a page may be sent now,
// must wait, or is deliberately dropped. It is pure policy: it reads the
// incident, the active silences and the announcement history, and returns a
// decision. The dispatcher owns the durable page state and applies it.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// Action is what the gate wants the dispatcher to do with one page.
type Action string

const (
	// ActionSend sends the page now.
	ActionSend Action = "send"
	// ActionWait keeps the page pending until ReadyAt.
	ActionWait Action = "wait"
	// ActionDrop cancels the page. The reason says why.
	ActionDrop Action = "drop"
)

// Decision is the gate's verdict for one page.
type Decision struct {
	Action    Action
	ReadyAt   time.Time
	Reason    string
	SilenceID *int64
}

// DecisionInputs is the input for one gate evaluation.
type DecisionInputs struct {
	Incident store.Incident
	Kind     store.DeliveryKind
	Channel  Kind
	Now      time.Time
	// Attempts is how many times this page was already attempted. A retry
	// of a failed send is not a new opening: cooldown and flap must not
	// delay it again.
	Attempts int
}

// GateStore is the slice of the store the gate needs.
type GateStore interface {
	// ActiveSilences returns the silences in force at now.
	ActiveSilences(ctx context.Context, now time.Time) ([]store.Silence, error)
	// Announcements returns how many episodes of this rule, subject and
	// channel were announced since a time, and when the last one was.
	Announcements(ctx context.Context, rule, subject, channel string, since time.Time) (int, *time.Time, error)
}

// Gate decides the disposition of one page.
//
// Order of checks:
//
//  1. acked      the operator already saw this episode
//  2. silenced   a silence covers the incident
//  3. held       the quiet window is open and the incident is not critical
//  4. flap       the subject used its page budget
//  5. cooldown   an opening page for the same (rule, subject) went out recently
//  6. send
//
// A failing store lookup never blocks a page: the gate logs it and carries on
// to the next check, because a missed page costs more than a duplicate.
type Gate struct {
	Store  GateStore
	Logger *slog.Logger

	// Cooldown is the minimum time between two opening pages for the same
	// (rule, subject). CooldownByRule overrides it per rule.
	Cooldown       time.Duration
	CooldownByRule map[string]time.Duration

	// FlapWindow and FlapMaxPages keep a flapping subject to at most
	// FlapMaxPages announced episodes per window. Only pages the operator
	// actually saw count. Critical incidents bypass the budget.
	FlapWindow   time.Duration
	FlapMaxPages int

	// PageCritical lets critical incidents bypass the quiet window.
	PageCritical bool

	// QuietFrom and QuietTo form the optional quiet window in QuietZone.
	// Both are required together (the configuration enforces it); an
	// overnight window (from > to) spans midnight.
	QuietFrom *time.Time
	QuietTo   *time.Time
	QuietZone *time.Location
}

// Evaluate reports the disposition of one page.
func (g *Gate) Evaluate(ctx context.Context, inputs DecisionInputs) Decision {
	now := inputs.Now
	incident := inputs.Incident

	// 1. an acked episode never pages, at any severity.
	if incident.Acked() {
		return Decision{Action: ActionDrop, Reason: "acked"}
	}

	// 2. an active silence covers the incident. Auto-deploy silences are
	// deploy noise, not an operator decision: they never hide a critical
	// incident or a resolution. Manual silences apply to every severity.
	if g.Store != nil {
		active, err := g.Store.ActiveSilences(ctx, now)
		if err != nil {
			g.warn("gate: read silences", err)
		} else {
			applicable := make([]store.Silence, 0, len(active))
			for _, silence := range active {
				if silenceApplies(silence, incident.Severity, inputs.Kind) {
					applicable = append(applicable, silence)
				}
			}
			if silence := store.MatchSilence(applicable, incident.Rule, incident.Subject, incident.AppID); silence != nil {
				if inputs.Kind == store.DeliveryResolved {
					// A resolution held until a maintenance window ends is
					// stale; the incident is already visible as resolved.
					return Decision{Action: ActionDrop, Reason: "silenced: " + silence.Reason}
				}
				reason := fmt.Sprintf("silence #%d: %s", silence.ID, silence.Reason)
				return Decision{Action: ActionWait, ReadyAt: silence.EndsAt, Reason: reason, SilenceID: &silence.ID}
			}
		}
	}

	// 3. quiet hours hold warnings until the window closes. Critical
	// incidents bypass the hold unless the operator disabled that. A
	// resolution is not worth waking the operator for: send a critical one
	// they are already awake for, drop the rest.
	if endsAt, inside := g.quietEndsAt(now); inside {
		critical := incident.Severity == store.SeverityCritical
		if inputs.Kind == store.DeliveryResolved {
			if !(critical && g.PageCritical) {
				return Decision{Action: ActionDrop, Reason: "quiet hours"}
			}
		} else if !(g.PageCritical && critical) {
			return Decision{Action: ActionWait, ReadyAt: endsAt, Reason: "quiet hours"}
		}
	}

	// 4. a subject that has used its announced-page budget is dropped.
	// Silenced attempts do not count: the budget is about what the operator
	// saw. Critical incidents bypass it, so a crash loop is never lost
	// because an earlier warning used the last slot.
	if g.Store != nil && inputs.Kind == store.DeliveryOpened && inputs.Attempts == 0 &&
		g.FlapWindow > 0 && g.FlapMaxPages > 0 && incident.Severity != store.SeverityCritical {
		count, _, err := g.Store.Announcements(ctx, incident.Rule, incident.Subject,
			string(inputs.Channel), now.Add(-g.FlapWindow))
		if err != nil {
			g.warn("gate: count announcements", err)
		} else if count >= g.FlapMaxPages {
			return Decision{Action: ActionDrop, Reason: "flapping"}
		}
	}

	// 5. an opening page for the same (rule, subject) inside the cooldown
	// waits for it to expire. Escalations and resolutions are never delayed.
	if g.Store != nil && inputs.Kind == store.DeliveryOpened && inputs.Attempts == 0 {
		window := g.cooldownFor(incident.Rule)
		if window > 0 {
			_, last, err := g.Store.Announcements(ctx, incident.Rule, incident.Subject,
				string(inputs.Channel), now.Add(-window))
			if err != nil {
				g.warn("gate: read last announcement", err)
			} else if last != nil {
				if readyAt := last.Add(window); now.Before(readyAt) {
					return Decision{Action: ActionWait, ReadyAt: readyAt, Reason: "pager cooldown"}
				}
			}
		}
	}

	// 6. send now.
	return Decision{Action: ActionSend}
}

// silenceApplies reports whether a silence covers this kind of page and
// severity. Auto-deploy silences only cover warnings and announcements.
func silenceApplies(silence store.Silence, severity store.Severity, kind store.DeliveryKind) bool {
	if silence.Kind != store.SilenceAutoDeploy {
		return true
	}
	return severity != store.SeverityCritical && kind != store.DeliveryResolved
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
