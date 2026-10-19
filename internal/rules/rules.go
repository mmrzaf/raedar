// Package rules evaluates the alerting rules and maintains incidents.
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/aggregate"
	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// The rule IDs.
const (
	// RuleRouteErrors alerts on the 5xx rate of a route.
	RuleRouteErrors = "route.errors"
	// RuleRouteSlow alerts on the p95 latency of a route.
	RuleRouteSlow = "route.slow"
	// RuleRouteDown alerts on requests that reached no backend.
	RuleRouteDown = "route.down"
	// RuleContainerCrashed reports a container that exited on its own.
	RuleContainerCrashed = "container.crashed"
	// RuleContainerRestarting reports a crash loop.
	RuleContainerRestarting = "container.restarting"
	// RuleContainerStopped reports a crashed container that stayed down.
	RuleContainerStopped = "container.stopped"
	// RuleContainerUnhealthy reports a container with a failing health check.
	RuleContainerUnhealthy = "container.unhealthy"
	// RuleContainerMemory reports a container close to its memory limit.
	RuleContainerMemory = "container.memory"
	// RuleHostDisk alerts on a filling filesystem.
	RuleHostDisk = "host.disk"
	// RuleHostDiskForecast alerts on a filesystem that will fill up soon.
	RuleHostDiskForecast = "host.disk_forecast"
	// RuleHostInodes alerts on inode usage.
	RuleHostInodes = "host.inodes"
	// RuleHostMemory alerts on low available memory.
	RuleHostMemory = "host.memory"
	// RuleHostUnitFailed reports a failed systemd unit.
	RuleHostUnitFailed = "host.unit_failed"
	// RuleCertExpiry alerts on a certificate that expires soon.
	RuleCertExpiry = "cert.expiry"
	// RuleCertInvalid alerts on a certificate that does not cover its domain.
	RuleCertInvalid = "cert.invalid"
	// RulePeerSilent alerts on a peer that stopped reporting.
	RulePeerSilent = "peer.silent"
	// RuleSourceFailed alerts on a source that stopped working.
	RuleSourceFailed = "raedar.source"
)

// evaluationDelay is how long after the minute boundary the rules run.
const evaluationDelay = 15 * time.Second

// reopenWindow is how long after resolving an incident is reopened instead of
// opened anew.
const reopenWindow = 30 * time.Minute

// databaseOutageAfter is how long the database must fail before the engine
// reports raedar.database.
const databaseOutageAfter = 2 * time.Minute

// Notifier receives incident changes.
type Notifier interface {
	// IncidentsChanged is called once per evaluation with every change, so
	// that a whole evaluation is grouped into one message.
	IncidentsChanged(ctx context.Context, changes []Change)
	// DatabaseDown reports that the database has failed for a while. It is
	// sent from memory because incidents cannot be stored.
	DatabaseDown(ctx context.Context, since time.Time)
	// DatabaseUp reports that the database works again.
	DatabaseUp(ctx context.Context)
}

// ChangeKind says how an incident changed.
type ChangeKind string

// The kinds of incident changes.
const (
	ChangeOpened    ChangeKind = "opened"
	ChangeEscalated ChangeKind = "escalated"
	ChangeResolved  ChangeKind = "resolved"
)

// Change describes what happened to one incident.
type Change struct {
	// Incident is the incident after the change.
	Incident store.Incident
	// Kind is opened, escalated or resolved.
	Kind ChangeKind
	// Reopened reports an incident that opened again within reopenWindow.
	Reopened bool
}

// Model provides the live model of the host.
type Model interface {
	// Model returns the current discovery model.
	Model() *discovery.Model
}

// Deltas provides the aggregator's unwritten measurements.
type Deltas interface {
	// Deltas returns the unwritten minute buckets at or after from.
	Deltas(from time.Time) []aggregate.Delta
}

// Engine evaluates the rules once a minute and maintains incidents.
type Engine struct {
	store    *store.Store
	model    Model
	deltas   Deltas
	cfg      *config.Config
	logger   *slog.Logger
	clock    clock.Clock
	notifier Notifier

	interval  time.Duration
	startedAt time.Time

	mu        sync.Mutex
	pending   map[store.IncidentKey]*pending
	closing   map[store.IncidentKey]time.Time
	lastCrash time.Time
	database  databaseWatch
}

// pending is a condition that is waiting for its for duration.
type pending struct {
	since   time.Time
	samples int
}

// New returns a rules engine.
func New(st *store.Store, model Model, deltas Deltas, cfg *config.Config, logger *slog.Logger, clock clock.Clock, notifier Notifier) *Engine {
	return &Engine{
		store:     st,
		model:     model,
		deltas:    deltas,
		cfg:       cfg,
		logger:    logger,
		clock:     clock,
		notifier:  notifier,
		interval:  time.Minute,
		startedAt: clock.Now(),
		pending:   make(map[store.IncidentKey]*pending),
		closing:   make(map[store.IncidentKey]time.Time),
	}
}

// Load records the newest crash event, so that crashes that happened before
// Raedar started do not open incidents.
func (e *Engine) Load(ctx context.Context) error {
	last, err := e.store.LatestEventTime(ctx, store.EventCrashed)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastCrash = last
	return nil
}

// Run evaluates the rules every minute, fifteen seconds after the boundary,
// until ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.Load(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.startedAt = e.clock.Now()
	e.mu.Unlock()
	for {
		now := e.clock.Now()
		next := now.Truncate(time.Minute).Add(time.Minute + evaluationDelay)
		select {
		case <-ctx.Done():
			return nil
		case <-e.clock.After(next.Sub(now)):
		}
		err := e.Evaluate(ctx)
		if err != nil {
			e.logger.Error("rule evaluation failed", slog.String("error", err.Error()))
		}
		outage, recovery := e.database.observe(e.clock.Now(), err != nil)
		if outage {
			e.notifier.DatabaseDown(ctx, e.database.since)
		}
		if recovery {
			e.notifier.DatabaseUp(ctx)
		}
	}
}

// Evaluate runs one evaluation of every rule.
func (e *Engine) Evaluate(ctx context.Context) error {
	now := e.clock.Now()
	snapshot, err := e.collect(ctx, now)
	if err != nil {
		return err
	}
	changes, err := e.advance(ctx, now, snapshot)
	if err != nil {
		return err
	}
	// Sending can take as long as a channel's timeout; it happens after the
	// engine's state is released so that it never delays the next evaluation.
	e.notify(ctx, changes)
	return nil
}

// advance applies one evaluation to the incidents and returns the changes.
func (e *Engine) advance(ctx context.Context, now time.Time, snapshot *snapshot) ([]Change, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var changes []Change
	evaluated := make(map[store.IncidentKey]bool)
	for _, reading := range e.readings(now, snapshot) {
		evaluated[store.IncidentKey{Rule: reading.rule, Subject: reading.subject}] = true
		change, err := e.apply(ctx, reading, now, snapshot)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	vanished, err := e.resolveVanished(ctx, now, snapshot, evaluated)
	if err != nil {
		return nil, err
	}
	changes = append(changes, vanished...)
	// Crash incidents come last, so that a crash loop opened in this
	// evaluation keeps the individual crashes quiet.
	crashChanges, err := e.evaluateCrashes(ctx, now, snapshot)
	if err != nil {
		return nil, err
	}
	return append(changes, crashChanges...), nil
}

// resolveVanished closes the open incidents that no rule evaluated this time.
// Every rule reports a reading for each subject it knows, so an incident
// without one belongs to a container, route, unit, certificate or peer that no
// longer exists, and nothing would ever resolve it. It closes after the same
// quiet period as any other incident, so a container that is briefly missing
// during a redeploy does not close its incident.
func (e *Engine) resolveVanished(ctx context.Context, now time.Time, snapshot *snapshot, evaluated map[store.IncidentKey]bool) ([]Change, error) {
	keys := make([]store.IncidentKey, 0, len(snapshot.open))
	for key, incident := range snapshot.open {
		if !evaluated[key] && !incident.OneShot {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Rule != keys[j].Rule {
			return keys[i].Rule < keys[j].Rule
		}
		return keys[i].Subject < keys[j].Subject
	})
	var changes []Change
	for _, key := range keys {
		incident := snapshot.open[key]
		change, err := e.apply(ctx, reading{
			rule:         key.Rule,
			subject:      key.Subject,
			alerts:       e.alertsFor(snapshot, incident),
			resolveAfter: e.cfg.Rules.ResolveAfter,
		}, now, snapshot)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	return changes, nil
}

// alertsFor reports whether messages are sent for an incident's app. An
// incident of an app that is gone too is announced, since it was announced
// when it opened unless the app asked for silence, which is no longer known.
func (e *Engine) alertsFor(snapshot *snapshot, incident store.Incident) bool {
	if incident.AppID == nil || snapshot.model == nil {
		return true
	}
	for _, app := range snapshot.model.Apps {
		if app.ID == *incident.AppID {
			return app.Alerts
		}
	}
	return true
}

// notify hands the changes to the notifier.
func (e *Engine) notify(ctx context.Context, changes []Change) {
	if e.notifier == nil {
		return
	}
	if len(changes) == 0 {
		return
	}
	e.notifier.IncidentsChanged(ctx, changes)
}

// apply advances the state of one rule's subject and returns the change.
func (e *Engine) apply(ctx context.Context, reading reading, now time.Time, snapshot *snapshot) (*Change, error) {
	key := store.IncidentKey{Rule: reading.rule, Subject: reading.subject}
	open, isOpen := snapshot.open[key]
	switch {
	case reading.firing && isOpen:
		delete(e.pending, key)
		delete(e.closing, key)
		if reading.severity == open.Severity {
			return nil, nil
		}
		incident := open
		incident.Severity = reading.severity
		incident.Summary = reading.summary
		incident.Details = encodeDetails(reading.details)
		if err := e.store.UpdateIncident(ctx, incident); err != nil {
			return nil, err
		}
		snapshot.open[key] = incident
		if !reading.alerts {
			return nil, nil
		}
		e.logger.Warn("incident escalated", slog.String("rule", reading.rule),
			slog.String("subject", reading.subject), slog.String("severity", string(reading.severity)))
		return &Change{Incident: incident, Kind: ChangeEscalated}, nil
	case reading.firing:
		delete(e.closing, key)
		state := e.pending[key]
		if state == nil {
			state = &pending{since: now}
			e.pending[key] = state
		}
		state.samples++
		if !satisfied(reading, state, now) {
			return nil, nil
		}
		delete(e.pending, key)
		incident, reopened, err := e.open(ctx, reading, now)
		if err != nil {
			return nil, err
		}
		snapshot.open[key] = *incident
		if !reading.alerts {
			return nil, nil
		}
		return &Change{Incident: *incident, Kind: ChangeOpened, Reopened: reopened}, nil
	default:
		delete(e.pending, key)
		if !isOpen {
			delete(e.closing, key)
			return nil, nil
		}
		if reading.resolveAfter > 0 {
			closedAt, ok := e.closing[key]
			if !ok {
				e.closing[key] = now
				return nil, nil
			}
			if now.Sub(closedAt) < reading.resolveAfter {
				return nil, nil
			}
		}
		delete(e.closing, key)
		if err := e.store.ResolveIncident(ctx, open.ID, now); err != nil {
			return nil, err
		}
		delete(snapshot.open, key)
		if !reading.alerts {
			return nil, nil
		}
		resolved := open
		resolved.ResolvedAt = &now
		e.logger.Warn("incident resolved", slog.String("rule", reading.rule), slog.String("subject", reading.subject))
		return &Change{Incident: resolved, Kind: ChangeResolved}, nil
	}
}

// satisfied reports whether a firing reading held long enough to open.
func satisfied(reading reading, state *pending, now time.Time) bool {
	if reading.openSamples > 0 && state.samples < reading.openSamples {
		return false
	}
	if reading.openAfter > 0 && now.Sub(state.since) < reading.openAfter {
		return false
	}
	return true
}

// open records an incident, reopening a recently resolved one when possible.
func (e *Engine) open(ctx context.Context, reading reading, now time.Time) (*store.Incident, bool, error) {
	recent, err := e.store.RecentResolvedIncident(ctx, reading.rule, reading.subject, now.Add(-reopenWindow))
	if err != nil {
		return nil, false, err
	}
	if recent != nil {
		incident := *recent
		incident.OpenedAt = now
		incident.ResolvedAt = nil
		incident.Severity = reading.severity
		incident.Summary = reading.summary
		incident.Details = encodeDetails(withReopened(reading.details))
		if err := e.store.ReopenIncident(ctx, incident); err != nil {
			return nil, false, err
		}
		e.logger.Warn("incident reopened", slog.String("rule", reading.rule), slog.String("subject", reading.subject))
		return &incident, true, nil
	}
	incident := store.Incident{
		Rule:     reading.rule,
		Subject:  reading.subject,
		AppID:    reading.appID,
		SlotID:   reading.slotID,
		RouteID:  reading.routeID,
		Severity: reading.severity,
		OpenedAt: now,
		Summary:  reading.summary,
	}
	incident.Details = encodeDetails(reading.details)
	id, err := e.store.InsertIncident(ctx, incident)
	if err != nil {
		return nil, false, err
	}
	incident.ID = id
	e.logger.Warn("incident opened", slog.String("rule", reading.rule),
		slog.String("subject", reading.subject), slog.String("severity", string(reading.severity)))
	return &incident, false, nil
}

// evaluateCrashes records the one-shot incidents of crashes seen since the
// last evaluation.
func (e *Engine) evaluateCrashes(ctx context.Context, now time.Time, snapshot *snapshot) ([]Change, error) {
	crashes, err := e.store.Events(ctx, store.EventCrashed, e.lastCrash, now.Add(time.Second))
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, event := range crashes {
		if !event.TS.After(e.lastCrash) {
			continue
		}
		e.lastCrash = event.TS
		if event.SlotID == nil {
			continue
		}
		slot, ok := snapshot.slots[*event.SlotID]
		if !ok {
			continue
		}
		// While a crash loop is open for the slot, single crashes stay quiet.
		if _, open := snapshot.open[store.IncidentKey{Rule: RuleContainerRestarting, Subject: slot.subject}]; open {
			continue
		}
		payload := struct {
			ExitCode int  `json:"exit_code"`
			OOM      bool `json:"oom"`
		}{}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("decode crash event %d: %w", event.ID, err)
		}
		severity := store.SeverityWarning
		if slot.critical {
			severity = store.SeverityCritical
		}
		resolvedAt := event.TS
		incident := store.Incident{
			Rule:       RuleContainerCrashed,
			Subject:    slot.subject,
			AppID:      slot.appID,
			SlotID:     idOf(slot.slotID),
			Severity:   severity,
			OpenedAt:   event.TS,
			ResolvedAt: &resolvedAt,
			OneShot:    true,
			Summary:    "container crashed",
			Details:    encodeDetails(map[string]any{"exit_code": payload.ExitCode, "oom": payload.OOM}),
		}
		id, err := e.store.InsertIncident(ctx, incident)
		if err != nil {
			return nil, err
		}
		incident.ID = id
		e.logger.Warn("container crashed", slog.String("slot", slot.subject),
			slog.Int("exit_code", payload.ExitCode), slog.Bool("oom", payload.OOM))
		if slot.alerts {
			changes = append(changes, Change{Incident: incident, Kind: ChangeOpened})
		}
	}
	return changes, nil
}

// reading is the outcome of one rule for one subject.
type reading struct {
	// rule and subject identify the incident.
	rule    string
	subject string
	// appID, slotID and routeID link the incident to the model.
	appID   *int64
	slotID  *int64
	routeID *int64
	// severity is the severity the incident has now.
	severity store.Severity
	// firing reports whether the condition holds now.
	firing bool
	// alerts reports whether the app allows messages.
	alerts bool
	// openAfter and openSamples are how long the condition must hold before
	// the incident opens.
	openAfter   time.Duration
	openSamples int
	// resolveAfter is how long the condition must be false before the
	// incident resolves.
	resolveAfter time.Duration
	// summary and details describe the reading.
	summary string
	details map[string]any
}

// encodeDetails encodes incident details as JSON, returning an empty object
// on failure.
func encodeDetails(details map[string]any) []byte {
	if details == nil {
		return []byte("{}")
	}
	data, err := json.Marshal(details)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// withReopened returns a copy of the details marked as reopened.
func withReopened(details map[string]any) map[string]any {
	copied := make(map[string]any, len(details)+1)
	for key, value := range details {
		copied[key] = value
	}
	copied["reopened"] = true
	return copied
}

// databaseWatch tracks how long the database has been failing.
type databaseWatch struct {
	// since is when the current outage started.
	since time.Time
	// notified reports whether the outage was already reported.
	notified bool
}

// observe records one evaluation outcome and reports whether the outage
// crossed the threshold or whether the database recovered.
func (w *databaseWatch) observe(now time.Time, failed bool) (outage, recovery bool) {
	if failed {
		if w.since.IsZero() {
			w.since = now
		}
		if !w.notified && now.Sub(w.since) >= databaseOutageAfter {
			w.notified = true
			return true, false
		}
		return false, false
	}
	if w.notified {
		recovered := true
		w.since = time.Time{}
		w.notified = false
		return false, recovered
	}
	w.since = time.Time{}
	return false, false
}
