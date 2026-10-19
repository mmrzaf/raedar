// Package notify delivers incident messages and the daily digest through
// Telegram, SMS and webhooks.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
)

// retryDelays returns the delays before a failed delivery is retried.
func retryDelays() []time.Duration {
	return []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}
}

// Store records deliveries and reads an incident's earlier deliveries.
type Store interface {
	// AddDelivery records a new delivery.
	AddDelivery(ctx context.Context, delivery store.Delivery) (int64, error)
	// UpdateDelivery records the outcome of one attempt.
	UpdateDelivery(ctx context.Context, delivery store.Delivery) error
	// Deliveries returns the deliveries of an incident.
	Deliveries(ctx context.Context, incidentID int64) ([]store.Delivery, error)
	// Events returns the events of one kind in a time range.
	Events(ctx context.Context, kind store.EventKind, from, to time.Time) ([]store.Event, error)
}

// Model provides the app names in messages.
type Model interface {
	// Model returns the current discovery model.
	Model() *discovery.Model
}

// Notifier delivers incident messages and the digest through the configured
// channels.
type Notifier struct {
	cfg       *config.Config
	store     Store
	model     Model
	logger    *slog.Logger
	clock     clock.Clock
	channels  map[Kind]Channel
	formatter formatter
	limiters  map[Kind]*limiter

	mu      sync.Mutex
	retries []*pendingDelivery
	held    map[Kind]int
}

// pendingDelivery is one message waiting for a retry.
type pendingDelivery struct {
	channel     Channel
	message     Message
	incidentIDs []int64
	attempt     int
	at          time.Time
}

// limiter counts the messages sent through one channel in a sliding window.
type limiter struct {
	limit  int
	window time.Duration
	sent   []time.Time
}

// allow reports whether a message may be sent now, and records it.
func (l *limiter) allow(now time.Time) bool {
	l.prune(now)
	if len(l.sent) >= l.limit {
		return false
	}
	l.sent = append(l.sent, now)
	return true
}

// prune drops the entries outside the window.
func (l *limiter) prune(now time.Time) {
	cutoff := now.Add(-l.window)
	kept := l.sent[:0]
	for _, at := range l.sent {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	l.sent = kept
}

// New returns a notifier for the configured channels.
func New(cfg *config.Config, st Store, model Model, logger *slog.Logger, clock clock.Clock) (*Notifier, error) {
	proxied, err := OutboundClient(cfg, true)
	if err != nil {
		return nil, err
	}
	direct, err := OutboundClient(cfg, false)
	if err != nil {
		return nil, err
	}
	notifier := &Notifier{
		cfg:      cfg,
		store:    st,
		model:    model,
		logger:   logger,
		clock:    clock,
		channels: newChannels(cfg, proxied, direct, clock),
		limiters: make(map[Kind]*limiter),
		held:     make(map[Kind]int),
	}
	notifier.formatter = formatter{
		host:      cfg.HostName,
		publicURL: cfg.PublicURL,
		location:  cfg.Location,
	}
	for kind := range notifier.channels {
		limit := cfg.Notify.RateLimit
		if kind == KindSMS {
			limit = cfg.Notify.SMSRateLimit
		}
		notifier.limiters[kind] = &limiter{limit: limit, window: time.Hour}
	}
	return notifier, nil
}

// deliveryKind returns the delivery kind that announces a change.
func deliveryKind(kind rules.ChangeKind) store.DeliveryKind {
	switch kind {
	case rules.ChangeEscalated:
		return store.DeliveryEscalated
	case rules.ChangeResolved:
		return store.DeliveryResolved
	default:
		return store.DeliveryOpened
	}
}

// IncidentsChanged delivers the changes of one evaluation, grouping the
// incidents of the same kind into one message per channel.
func (n *Notifier) IncidentsChanged(ctx context.Context, changes []rules.Change) {
	if len(changes) == 0 {
		return
	}
	groups := make(map[store.DeliveryKind]*group)
	var order []store.DeliveryKind
	for _, change := range changes {
		incident := change.Incident
		incident.Details = n.withAppName(incident)
		kind := deliveryKind(change.Kind)
		g := groups[kind]
		if g == nil {
			g = &group{kind: kind, reopened: make(map[int64]bool)}
			groups[kind] = g
			order = append(order, kind)
		}
		g.incidents = append(g.incidents, incident)
		if change.Reopened {
			g.reopened[incident.ID] = true
		}
	}
	for _, kind := range order {
		n.sendGroup(ctx, groups[kind])
	}
}

// sendGroup builds the message of one group and delivers it to its channels.
func (n *Notifier) sendGroup(ctx context.Context, g *group) {
	f := n.formatter
	f.now = n.clock.Now()
	f.deploys = n.deployContexts(ctx, g)
	message := Message{
		Text:     f.text(*g),
		SMS:      f.smsText(*g),
		Document: f.document(*g),
	}
	for _, channel := range n.channelsFor(ctx, g) {
		n.deliver(ctx, channel, g.kind, g.incidents, message)
	}
}

// channelsFor returns the channels a group is delivered to.
func (n *Notifier) channelsFor(ctx context.Context, g *group) []Channel {
	seen := make(map[Kind]bool)
	var channels []Channel
	add := func(kind Kind) {
		channel := n.channels[kind]
		if channel == nil || seen[kind] {
			return
		}
		seen[kind] = true
		channels = append(channels, channel)
	}
	if g.kind == "resolved" {
		// A resolution goes to the channels its opening message went to.
		for _, incident := range g.incidents {
			deliveries, err := n.store.Deliveries(ctx, incident.ID)
			if err != nil {
				n.logger.Warn("read incident deliveries", slog.Int64("incident", incident.ID), slog.String("error", err.Error()))
				continue
			}
			for _, delivery := range deliveries {
				if delivery.Kind == store.DeliveryOpened || delivery.Kind == store.DeliveryEscalated || delivery.Kind == store.DeliveryTest {
					add(Kind(delivery.Channel))
				}
			}
		}
		return channels
	}
	for _, incident := range g.incidents {
		for _, kind := range n.routing(incident.Severity) {
			add(kind)
		}
	}
	return channels
}

// routing returns the channels of one severity.
func (n *Notifier) routing(severity store.Severity) []Kind {
	if severity == store.SeverityCritical {
		if n.cfg.Notify.CriticalChannels == nil {
			return n.configured()
		}
		return toKinds(n.cfg.Notify.CriticalChannels)
	}
	return toKinds(n.cfg.Notify.WarningChannels)
}

// configured returns the configured channels in a fixed order.
func (n *Notifier) configured() []Kind {
	var kinds []Kind
	for _, kind := range []Kind{KindTelegram, KindSMS, KindWebhook} {
		if n.channels[kind] != nil {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// toKinds turns channel names into kinds.
func toKinds(names []string) []Kind {
	kinds := make([]Kind, 0, len(names))
	for _, name := range names {
		kinds = append(kinds, Kind(name))
	}
	return kinds
}

// digestChannels returns the configured channels of the digest.
func (n *Notifier) digestChannels() []Channel {
	var channels []Channel
	for _, kind := range toKinds(n.cfg.Notify.DigestChannels) {
		if channel := n.channels[kind]; channel != nil {
			channels = append(channels, channel)
		}
	}
	return channels
}

// deliver sends one message through a channel unless the rate limit holds it.
func (n *Notifier) deliver(ctx context.Context, channel Channel, kind store.DeliveryKind, incidents []store.Incident, message Message) {
	limit := n.limiters[channel.Kind()]
	if limit != nil && !limit.allow(n.clock.Now()) {
		n.mu.Lock()
		n.held[channel.Kind()] += len(incidents)
		n.mu.Unlock()
		n.logger.Warn("notification rate limit reached", slog.String("channel", string(channel.Kind())),
			slog.Int("incidents", len(incidents)))
		return
	}
	// One delivery row per incident; a message without incidents, like the
	// digest, gets a single row that belongs to no incident.
	subjects := make([]*int64, 0, max(len(incidents), 1))
	for _, incident := range incidents {
		id := incident.ID
		subjects = append(subjects, &id)
	}
	if len(subjects) == 0 {
		subjects = append(subjects, nil)
	}
	var ids []int64
	for _, incidentID := range subjects {
		deliveryID, err := n.store.AddDelivery(ctx, store.Delivery{
			IncidentID: incidentID,
			Kind:       kind,
			Channel:    string(channel.Kind()),
			CreatedAt:  n.clock.Now(),
			Status:     "pending",
		})
		if err != nil {
			n.logger.Warn("record delivery", slog.String("error", err.Error()))
			continue
		}
		ids = append(ids, deliveryID)
	}
	n.attempt(ctx, channel, message, ids, 0)
}

// attempt sends one message and schedules a retry when it fails.
func (n *Notifier) attempt(ctx context.Context, channel Channel, message Message, ids []int64, failed int) {
	err := channel.Send(ctx, message)
	now := n.clock.Now()
	for _, id := range ids {
		delivery := store.Delivery{ID: id, Attempts: failed + 1, Status: "sent"}
		switch {
		case err == nil:
			delivery.SentAt = &now
		case failed >= len(retryDelays()):
			delivery.Status = "failed"
		default:
			delivery.Status = "pending"
		}
		if err != nil {
			text := safeError(err).Error()
			delivery.Error = &text
		}
		if updateErr := n.store.UpdateDelivery(ctx, delivery); updateErr != nil {
			n.logger.Warn("update delivery", slog.String("error", updateErr.Error()))
		}
	}
	if err == nil {
		return
	}
	if failed >= len(retryDelays()) {
		n.logger.Warn("notification failed", slog.String("channel", string(channel.Kind())),
			slog.Int("attempts", failed+1), slog.String("error", safeError(err).Error()))
		return
	}
	delay := retryDelays()[failed]
	n.mu.Lock()
	n.retries = append(n.retries, &pendingDelivery{
		channel:     channel,
		message:     message,
		incidentIDs: ids,
		attempt:     failed + 1,
		at:          now.Add(delay),
	})
	n.mu.Unlock()
	n.logger.Warn("notification will be retried", slog.String("channel", string(channel.Kind())),
		slog.Duration("in", delay), slog.String("error", safeError(err).Error()))
}

// Run delivers retries and held summaries until ctx is done.
func (n *Notifier) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n.deliverDue(ctx)
			n.deliverHeld(ctx)
		}
	}
}

// deliverDue sends the retries whose delay passed.
func (n *Notifier) deliverDue(ctx context.Context) {
	now := n.clock.Now()
	n.mu.Lock()
	var due []*pendingDelivery
	remaining := n.retries[:0]
	for _, item := range n.retries {
		if item.at.After(now) {
			remaining = append(remaining, item)
			continue
		}
		due = append(due, item)
	}
	n.retries = remaining
	n.mu.Unlock()
	for _, item := range due {
		n.attempt(ctx, item.channel, item.message, item.incidentIDs, item.attempt)
	}
}

// deliverHeld sends the held summary of a channel once its limit allows it.
func (n *Notifier) deliverHeld(ctx context.Context) {
	now := n.clock.Now()
	kinds := make([]Kind, 0, len(n.channels))
	for kind := range n.channels {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, kind := range kinds {
		n.mu.Lock()
		held := n.held[kind]
		n.mu.Unlock()
		if held == 0 {
			continue
		}
		limit := n.limiters[kind]
		if limit != nil && !limit.allow(now) {
			continue
		}
		summary := fmt.Sprintf("%d more incidents, see Raedar", held)
		document := digestDocument(n.cfg.HostName, now, summary)
		message := Message{
			Text:     summary,
			SMS:      truncateSMS(fmt.Sprintf("RAEDAR %s: %s", n.cfg.HostName, summary)),
			Document: document,
		}
		if err := n.channels[kind].Send(ctx, message); err != nil {
			n.logger.Warn("send held summary", slog.String("channel", string(kind)), slog.String("error", safeError(err).Error()))
			continue
		}
		n.mu.Lock()
		n.held[kind] -= held
		n.mu.Unlock()
	}
}

// SendDigest delivers the daily digest through the digest channels.
func (n *Notifier) SendDigest(ctx context.Context, text string) {
	now := n.clock.Now()
	message := Message{
		Text:     text,
		SMS:      truncateSMS(text),
		Document: digestDocument(n.cfg.HostName, now, text),
	}
	for _, channel := range n.digestChannels() {
		n.deliver(ctx, channel, store.DeliveryDigest, nil, message)
	}
}

// deployContexts returns the deploy context of the incidents' apps.
func (n *Notifier) deployContexts(ctx context.Context, g *group) map[int64]deployContext {
	deploys := make(map[int64]deployContext)
	for _, incident := range g.incidents {
		if incident.AppID == nil || g.kind == "resolved" {
			continue
		}
		events, err := n.store.Events(ctx, store.EventDeploy, incident.OpenedAt.Add(-30*time.Minute), incident.OpenedAt)
		if err != nil || len(events) == 0 {
			continue
		}
		event := events[len(events)-1]
		payload := struct {
			Changes []struct {
				NewImage string `json:"new_image"`
				Service  string `json:"service"`
			} `json:"changes"`
		}{}
		if err := json.Unmarshal(event.Data, &payload); err != nil || len(payload.Changes) == 0 {
			continue
		}
		app := n.appName(incident.AppID)
		first := payload.Changes[0]
		deploys[*incident.AppID] = deployContext{
			app:     app,
			from:    first.Service,
			to:      first.NewImage,
			minutes: int(incident.OpenedAt.Sub(event.TS).Round(time.Minute) / time.Minute),
		}
	}
	return deploys
}

// withAppName adds the app name to the incident details.
func (n *Notifier) withAppName(incident store.Incident) []byte {
	details := decodeDetails(incident)
	if name := n.appName(incident.AppID); name != "" {
		details["app"] = name
	}
	data, err := json.Marshal(details)
	if err != nil {
		return incident.Details
	}
	return data
}

// appName returns the name of an app.
func (n *Notifier) appName(id *int64) string {
	if id == nil || n.model == nil {
		return ""
	}
	model := n.model.Model()
	if model == nil {
		return ""
	}
	for _, app := range model.Apps {
		if app.ID == *id {
			return app.Name
		}
	}
	return ""
}

// digestDocument returns the webhook body of a text message.
func digestDocument(host string, at time.Time, text string) []byte {
	body := document{Version: 1, Host: host, Kind: "digest", SentAt: at, Text: text}
	data, err := json.Marshal(body)
	if err != nil {
		return []byte(`{"version":1}`)
	}
	return data
}

// TestResult is the outcome of one test delivery.
type TestResult struct {
	// Channel is the channel that was tested.
	Channel Kind
	// Err is why the test delivery failed; nil on success.
	Err error
}

// Test sends a test message through the configured channels, or through one
// channel.
func Test(ctx context.Context, cfg *config.Config, logger *slog.Logger, clock clock.Clock, only string) ([]TestResult, error) {
	proxied, err := OutboundClient(cfg, true)
	if err != nil {
		return nil, err
	}
	direct, err := OutboundClient(cfg, false)
	if err != nil {
		return nil, err
	}
	channels := newChannels(cfg, proxied, direct, clock)
	kinds := make([]Kind, 0, len(channels))
	for kind := range channels {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	now := clock.Now()
	text := fmt.Sprintf("Raedar test message from %s", cfg.HostName)
	document := document{Version: 1, Host: cfg.HostName, Kind: "test", SentAt: now, Text: text}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode test message: %w", err)
	}
	message := Message{
		Text:     text,
		SMS:      truncateSMS(fmt.Sprintf("RAEDAR %s test message", cfg.HostName)),
		Document: data,
	}
	var results []TestResult
	matched := false
	for _, kind := range kinds {
		if only != "" && string(kind) != only {
			continue
		}
		matched = true
		results = append(results, TestResult{Channel: kind, Err: channels[kind].Send(ctx, message)})
	}
	if only != "" && !matched {
		return nil, fmt.Errorf("channel %q is not configured", only)
	}
	return results, nil
}

// DatabaseDown reports a database outage. The message is built and sent from
// memory, without a delivery record, because the database cannot store one.
func (n *Notifier) DatabaseDown(ctx context.Context, since time.Time) {
	incident := n.databaseIncident("database writes are failing", since)
	n.sendFromMemory(ctx, incident, store.DeliveryOpened)
}

// DatabaseUp reports that the database works again.
func (n *Notifier) DatabaseUp(ctx context.Context) {
	now := n.clock.Now()
	incident := n.databaseIncident("database writes work again", now)
	incident.ResolvedAt = &now
	n.sendFromMemory(ctx, incident, store.DeliveryResolved)
}

// databaseIncident returns the synthetic incident of a database outage.
func (n *Notifier) databaseIncident(summary string, at time.Time) store.Incident {
	return store.Incident{
		Rule:     "raedar.database",
		Subject:  n.cfg.HostName,
		Severity: store.SeverityCritical,
		OpenedAt: at,
		Summary:  summary,
		Details:  []byte(`{}`),
	}
}

// sendFromMemory delivers one incident through the channels of its severity.
func (n *Notifier) sendFromMemory(ctx context.Context, incident store.Incident, kind store.DeliveryKind) {
	f := n.formatter
	f.now = n.clock.Now()
	g := group{kind: kind, incidents: []store.Incident{incident}, reopened: map[int64]bool{}}
	message := Message{Text: f.text(g), SMS: f.smsText(g), Document: f.document(g)}
	for _, channel := range n.severityChannels(incident.Severity) {
		limit := n.limiters[channel.Kind()]
		if limit != nil && !limit.allow(n.clock.Now()) {
			n.logger.Warn("database notification rate limited", slog.String("channel", string(channel.Kind())))
			continue
		}
		if err := channel.Send(ctx, message); err != nil {
			n.logger.Warn("send database notification", slog.String("channel", string(channel.Kind())),
				slog.String("error", safeError(err).Error()))
		}
	}
}

// severityChannels returns the configured channels of one severity, in a
// fixed order.
func (n *Notifier) severityChannels(severity store.Severity) []Channel {
	seen := make(map[Kind]bool)
	var channels []Channel
	for _, kind := range n.routing(severity) {
		channel := n.channels[kind]
		if channel == nil || seen[kind] {
			continue
		}
		seen[kind] = true
		channels = append(channels, channel)
	}
	return channels
}
