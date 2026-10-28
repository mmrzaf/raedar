// Package notify delivers incident messages and the daily digest through
// Telegram, SMS and webhooks.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
)

// Store is the slice of the store the notifier needs.
type Store interface {
	// AddDelivery records a new delivery attempt.
	AddDelivery(ctx context.Context, delivery store.Delivery) (int64, error)
	// UpdateDelivery records the outcome of one attempt.
	UpdateDelivery(ctx context.Context, delivery store.Delivery) error
	// Events returns the events of one kind in a time range.
	Events(ctx context.Context, kind store.EventKind, from, to time.Time) ([]store.Event, error)
	// ActiveSilences returns the silences in force at now.
	ActiveSilences(ctx context.Context, now time.Time) ([]store.Silence, error)
	// Announcements counts the episodes a subject announced on a channel.
	Announcements(ctx context.Context, rule, subject, channel string, since time.Time) (int, *time.Time, error)
	// IncidentByID returns one incident with its current state.
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	// IncidentsForNotify returns open incidents and recent resolutions.
	IncidentsForNotify(ctx context.Context, since time.Time) ([]store.Incident, error)
	// PagesForEpisode returns the pages of one incident episode.
	PagesForEpisode(ctx context.Context, incidentID, episode int64) ([]store.Page, error)
	// CreatePage inserts one durable page, or returns the existing row.
	CreatePage(ctx context.Context, page store.Page) (store.Page, error)
	// UpdatePage writes the mutable state of one page.
	UpdatePage(ctx context.Context, page store.Page) error
	// CancelPage cancels one pending page.
	CancelPage(ctx context.Context, id int64, reason string) error
	// CancelPendingPages cancels the pending pages of an incident.
	CancelPendingPages(ctx context.Context, incidentID int64, reason string) error
	// DuePages returns the pages that may run at now.
	DuePages(ctx context.Context, now time.Time, limit int) ([]store.Page, error)
	// SentOpeningAt reports when an opening page already reached a recipient.
	SentOpeningAt(ctx context.Context, incidentID int64, openedAt time.Time, channel, recipient string) (*time.Time, error)
	// FailPendingDeliveries marks deliveries left pending by an earlier run
	// as failed.
	FailPendingDeliveries(ctx context.Context, reason string) (int64, error)
	// OpenIncidentCount returns the number of currently open incidents.
	OpenIncidentCount(ctx context.Context) (int, error)
}

// Model provides the app names and alert flags in messages.
type Model interface {
	// Model returns the current discovery model.
	Model() *discovery.Model
}

// Notifier delivers incident messages and the digest through the configured
// channels. Incident pages are durable rows in the pages table; the notifier
// only decides when a page may run and records what happened.
type Notifier struct {
	cfg       *config.Config
	store     Store
	model     Model
	logger    *slog.Logger
	clock     clock.Clock
	channels  map[Kind]Channel
	formatter formatter
	limiters  map[Kind]*limiter
	gate      *Gate

	// workMu serializes the planner and the dispatcher. They run from the
	// rules goroutine and the background loop, and both may send a page.
	workMu sync.Mutex

	mu              sync.Mutex
	lastPageAt      time.Time
	lastStandDownAt time.Time
}

// limiter counts the messages sent through one channel in a sliding window.
// It is safe for concurrent use.
type limiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	sent []time.Time
}

// allow reports whether a message may be sent now, and records it.
func (l *limiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	if len(l.sent) >= l.limit {
		return false
	}
	l.sent = append(l.sent, now)
	return true
}

// undo gives back the slot the last allow took, for a message that was not
// sent after all.
func (l *limiter) undo() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.sent) > 0 {
		l.sent = l.sent[:len(l.sent)-1]
	}
}

// prune drops the entries outside the window. The caller holds mu.
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
		cfg:        cfg,
		store:      st,
		model:      model,
		logger:     logger,
		clock:      clock,
		channels:   newChannels(cfg, proxied, direct, clock),
		limiters:   make(map[Kind]*limiter),
		lastPageAt: clock.Now(),
	}
	notifier.formatter = formatter{
		host:      cfg.HostName,
		publicURL: cfg.PublicURL,
		location:  cfg.Location,
	}
	if !cfg.SubjectNames.Empty() {
		notifier.formatter.names = cfg.SubjectNames.Display
	}
	notifier.gate = &Gate{
		Store:          st,
		Logger:         logger,
		Cooldown:       cfg.Notify.Silent.PagerCooldown,
		CooldownByRule: cfg.Notify.Silent.PagerCooldownByRule,
		FlapWindow:     cfg.Notify.Silent.FlapWindow,
		FlapMaxPages:   cfg.Notify.Silent.FlapMaxPages,
		PageCritical:   cfg.Notify.Silent.QuietPageCritical,
		QuietFrom:      silentFrom(cfg.Notify.Silent.QuietFrom),
		QuietTo:        silentFrom(cfg.Notify.Silent.QuietTo),
		QuietZone:      cfg.Notify.Silent.QuietZone,
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

// silentFrom turns an optional TimeOfDay into a clock time at the start of
// today. The gate only reads Hour and Minute, so the date does not matter.
func silentFrom(at *config.TimeOfDay) *time.Time {
	if at == nil {
		return nil
	}
	t := time.Date(0, 1, 1, at.Hour, at.Minute, 0, 0, time.UTC)
	return &t
}

// IncidentsChanged hints the planner about the incidents one evaluation
// changed, then runs the dispatcher once so a fresh page goes out without
// waiting for the next tick. The periodic planner is the real guarantee; this
// only lowers latency.
func (n *Notifier) IncidentsChanged(ctx context.Context, changes []rules.Change) {
	if len(changes) == 0 {
		return
	}
	n.workMu.Lock()
	defer n.workMu.Unlock()
	n.reconcileChanges(ctx, changes)
	n.deliverDue(ctx)
}

// Run reconciles and dispatches pages until ctx is done. The startup pass
// seeds pages from existing incidents and deliveries, so an incident that was
// opened while Raedar was down (or by an older version) is not forgotten.
func (n *Notifier) Run(ctx context.Context) error {
	n.workMu.Lock()
	n.reconcileAll(ctx)
	n.deliverDue(ctx)
	n.workMu.Unlock()

	dispatch := time.NewTicker(time.Second)
	defer dispatch.Stop()
	plan := time.NewTicker(time.Minute)
	defer plan.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-dispatch.C:
			n.workMu.Lock()
			n.deliverDue(ctx)
			n.workMu.Unlock()
			n.maybeStandDown(ctx)
		case <-plan.C:
			n.workMu.Lock()
			n.reconcileAll(ctx)
			n.deliverDue(ctx)
			n.workMu.Unlock()
			n.maybeStandDown(ctx)
		}
	}
}

// markPaged records a successful incident page for the stand-down
// announcement.
func (n *Notifier) markPaged(now time.Time) {
	n.mu.Lock()
	n.lastPageAt = now
	n.mu.Unlock()
}

// FailInterrupted marks the deliveries an earlier run left pending as failed.
// Their durable pages remain pending, so the dispatcher retries the page; the
// audit trail shows the interrupted attempt.
func (n *Notifier) FailInterrupted(ctx context.Context) error {
	count, err := n.store.FailPendingDeliveries(ctx, "interrupted by a restart")
	if err != nil {
		return err
	}
	if count > 0 {
		n.logger.Warn("marked interrupted deliveries as failed", slog.Int64("deliveries", count))
	}
	return nil
}

// maybeStandDown sends one "all quiet" message per quiet period. It runs
// when no incident has paged for StandDownAfter and nothing is open, so an
// operator who hears nothing can still tell the pipeline is alive.
func (n *Notifier) maybeStandDown(ctx context.Context) {
	window := n.cfg.Notify.Silent.StandDownAfter
	if window <= 0 {
		return
	}
	now := n.clock.Now()
	n.mu.Lock()
	lastPage := n.lastPageAt
	lastStandDown := n.lastStandDownAt
	n.mu.Unlock()
	if !lastPage.IsZero() && now.Sub(lastPage) < window {
		return
	}
	if !lastStandDown.IsZero() && (lastPage.IsZero() || lastStandDown.After(lastPage)) {
		return
	}
	count, err := n.store.OpenIncidentCount(ctx)
	if err != nil || count > 0 {
		return
	}
	text := fmt.Sprintf("%s · all quiet for %s", n.cfg.HostName, quietSpan(window))
	message := Message{
		Text:     text,
		SMS:      truncateSMS("RAEDAR " + text),
		Document: digestDocument(n.cfg.HostName, now, text),
	}
	for _, channel := range n.digestChannels() {
		if err := channel.Send(ctx, message); err != nil {
			n.logger.Warn("stand-down message", slog.String("channel", string(channel.Kind())),
				slog.String("error", SafeError(err).Error()))
		}
	}
	n.mu.Lock()
	n.lastStandDownAt = now
	n.mu.Unlock()
}

// quietSpan formats a stand-down window as whole hours when it is one, and as
// minutes otherwise.
func quietSpan(window time.Duration) string {
	if window%time.Hour == 0 {
		return fmt.Sprintf("%d h", window/time.Hour)
	}
	return fmt.Sprintf("%d min", int(window.Round(time.Minute)/time.Minute))
}

// SendDigest delivers the daily digest through the digest channels. A digest
// is not an incident page: it is sent once and not retried, because the next
// digest is the retry.
func (n *Notifier) SendDigest(ctx context.Context, text string) {
	now := n.clock.Now()
	message := Message{
		Text:     text,
		SMS:      truncateSMS(text),
		Document: digestDocument(n.cfg.HostName, now, text),
	}
	for _, channel := range n.digestChannels() {
		n.sendDirect(ctx, channel, store.DeliveryDigest, message, now)
	}
}

// sendDirect sends one message to every recipient of a channel and records
// the attempts. Digest and test paths use it; incident pages use the durable
// dispatcher.
func (n *Notifier) sendDirect(ctx context.Context, channel Channel, kind store.DeliveryKind, message Message, now time.Time) {
	chatIDs := channel.ChatIDs()
	if len(chatIDs) == 0 {
		chatIDs = []string{""}
	}
	for _, recipient := range chatIDs {
		id, err := n.store.AddDelivery(ctx, store.Delivery{
			Kind:      kind,
			Channel:   string(channel.Kind()),
			Recipient: recipient,
			CreatedAt: now,
			Status:    "pending",
		})
		if err != nil {
			n.logger.Warn("record direct delivery", slog.String("error", err.Error()))
		}
		messageID, sendErr := n.sendOne(ctx, channel, message, recipient)
		if id == 0 {
			continue
		}
		delivery := store.Delivery{ID: id, Kind: kind, Channel: string(channel.Kind()),
			Recipient: recipient, Attempts: 1}
		if sendErr != nil {
			text := SafeError(sendErr).Error()
			delivery.Status = "failed"
			delivery.Error = &text
			n.logger.Warn("direct delivery failed", slog.String("channel", string(channel.Kind())),
				slog.String("error", text))
		} else {
			delivery.Status = "sent"
			delivery.SentAt = &now
			if channel.Kind() == KindTelegram {
				if chat, err := strconv.ParseInt(recipient, 10, 64); err == nil {
					delivery.TelegramChatID = &chat
				}
				if messageID != 0 {
					delivery.TelegramMessageID = &messageID
				}
			}
		}
		if err := n.store.UpdateDelivery(ctx, delivery); err != nil {
			n.logger.Warn("update direct delivery", slog.String("error", err.Error()))
		}
	}
}

// SendReply answers an operator's Telegram command. The inbox goroutine
// calls this with the chat_id of the command.
func (n *Notifier) SendReply(ctx context.Context, chatID int64, text string) error {
	channel, ok := n.channels[KindTelegram].(*telegramChannel)
	if !ok {
		return fmt.Errorf("no Telegram channel is configured")
	}
	return channel.SendReply(ctx, strconv.FormatInt(chatID, 10), text)
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
				slog.String("error", SafeError(err).Error()))
		}
	}
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
