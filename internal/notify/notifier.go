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
	// FindActiveSilence reports the most-specific active silence for the
	// (rule, subject) at now, or nil when nothing matches. Used by the
	// gate to suppress pages without changing the engine.
	FindActiveSilence(ctx context.Context, rule, subject string, appID *int64, now time.Time) (*store.Silence, error)
	// IncidentByID returns one incident, including its ack metadata.
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	// LastSent returns the most recent sent delivery for a (rule, subject)
	// pair, used by the gate to implement pager cooldown.
	LastSent(ctx context.Context, rule, subject string) (*store.Delivery, error)
	// OpenIncidentCount returns the number of currently open incidents,
	// used by the gate to decide whether to honour stand-down.
	OpenIncidentCount(ctx context.Context) (int, error)
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
	gate      *Gate

	mu      sync.Mutex
	retries []*pendingDelivery
	held    map[Kind]int
	// deferred holds pages the gate postponed (quiet hours, cooldown) until
	// their ReadyAt. Keyed by channel + rule + subject so repeated
	// suppressions collapse into one message.
	deferred map[string]*deferredPage
	// lastPageAt is the time of the most recent successful page, used by
	// the stand-down announcement. lastStandDownAt is when the last "all
	// quiet" message went out.
	lastPageAt      time.Time
	lastStandDownAt time.Time
}

// pendingDelivery is one message waiting for a retry.
type pendingDelivery struct {
	channel Channel
	message Message
	slots   []slot
	attempt int
	at      time.Time
}

// deferredPage is one page the gate postponed. The most recent incident
// for a key replaces the earlier one, so a flapping subject produces one
// catch-up message rather than a burst.
type deferredPage struct {
	channel   Channel
	kind      store.DeliveryKind
	incidents []store.Incident
	readyAt   time.Time
	status    GateStatus
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
		deferred: make(map[string]*deferredPage),
	}
	notifier.formatter = formatter{
		host:      cfg.HostName,
		publicURL: cfg.PublicURL,
		location:  cfg.Location,
	}
	notifier.gate = &Gate{
		Store:          st,
		Logger:         logger,
		Cooldown:       cfg.Notify.Silent.PagerCooldown,
		CooldownByRule: cfg.Notify.Silent.PagerCooldownByRule,
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

// silentFrom turns an optional TimeOfDay into a clock time at the start of
// today. The gate only reads Hour and Minute, so the date does not matter.
func silentFrom(at *config.TimeOfDay) *time.Time {
	if at == nil {
		return nil
	}
	t := time.Date(0, 1, 1, at.Hour, at.Minute, 0, 0, time.UTC)
	return &t
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
//
// Every incident of the group is evaluated by the gate for this channel.
// Recorded incidents are sent now; acked and silenced ones are recorded so
// the deliveries page keeps the audit trail; held and cooldown ones are
// recorded and queued for a single catch-up message when the gate's
// ReadyAt passes.
func (n *Notifier) sendGroup(ctx context.Context, g *group) {
	now := n.clock.Now()
	for _, channel := range n.channelsFor(ctx, g) {
		kept := make([]store.Incident, 0, len(g.incidents))
		for _, incident := range g.incidents {
			decision := n.gate.Evaluate(ctx, DecisionInputs{
				Incident:   incident,
				ChangeKind: g.kind,
				Channel:    channel.Kind(),
				Now:        now,
			})
			switch decision.Status {
			case GateRecorded:
				kept = append(kept, incident)
			case GateAcked, GateSilenced:
				n.recordSuppressed(ctx, channel, g.kind, incident, decision.Status, now)
			case GateHeld, GateCooldown:
				n.recordSuppressed(ctx, channel, g.kind, incident, decision.Status, now)
				n.deferIncident(channel, g.kind, incident, decision)
			}
		}
		if len(kept) == 0 {
			continue
		}
		n.sendMessage(ctx, channel, g.kind, *g, kept, now)
	}
}

// sendMessage renders a group of incidents for one channel and delivers it.
func (n *Notifier) sendMessage(ctx context.Context, channel Channel, kind store.DeliveryKind, g group, incidents []store.Incident, now time.Time) {
	f := n.formatter
	f.now = now
	f.deploys = n.deployContexts(ctx, &g)
	sub := g
	sub.incidents = incidents
	n.deliver(ctx, channel, kind, incidents, Message{
		Text:     f.text(sub),
		SMS:      f.smsText(sub),
		Document: f.document(sub),
	})
}

// recordSuppressed writes a delivery row for a page the gate suppressed.
func (n *Notifier) recordSuppressed(ctx context.Context, channel Channel, kind store.DeliveryKind, incident store.Incident, status GateStatus, now time.Time) {
	id := incident.ID
	if _, err := n.store.AddDelivery(ctx, store.Delivery{
		IncidentID: &id,
		Kind:       kind,
		Channel:    string(channel.Kind()),
		CreatedAt:  now,
		Status:     string(status),
	}); err != nil {
		n.logger.Warn("record suppressed delivery", slog.String("error", err.Error()))
	}
}

// deferIncident queues an incident for a catch-up message. Repeated
// suppressions of the same (channel, rule, subject) collapse into one
// entry, keeping the latest incident and the latest ready time.
func (n *Notifier) deferIncident(channel Channel, kind store.DeliveryKind, incident store.Incident, decision Decision) {
	key := string(channel.Kind()) + "\x00" + incident.Rule + "\x00" + incident.Subject
	n.mu.Lock()
	defer n.mu.Unlock()
	entry := n.deferred[key]
	if entry == nil {
		entry = &deferredPage{channel: channel, kind: kind}
		n.deferred[key] = entry
	}
	entry.incidents = []store.Incident{incident}
	entry.readyAt = decision.ReadyAt
	entry.status = decision.Status
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

// slot is one (incident × chat_id) pair the pager wants to track as a
// separate delivery. When the channel posts to N chats for M incidents,
// the pager creates N*M slots for Telegram, one slot per other channel
// kind (chat_id is empty there), or one slot per chat for the digest
// when there are no incidents.
type slot struct {
	id         int64
	incidentID *int64
	channel    string
	chatID     string
}

// deliver records one delivery row per (incident × chat_id) pair (one row
// for channels that do not fan out, one per chat for Telegram), then hands
// the resulting slots to attempt. Each row gets its own
// telegram_message_id after the channel returns, so reply-to lookup is
// deterministic regardless of how many chats the operator lists.
func (n *Notifier) deliver(ctx context.Context, channel Channel, kind store.DeliveryKind, incidents []store.Incident, message Message) {
	chatIDs := n.channelChatIDs(channel)
	if len(chatIDs) == 0 {
		chatIDs = []string{""}
	}
	now := n.clock.Now()
	// Rate limit before recording rows: a withheld message must not leave
	// pending rows behind that nothing will ever resolve.
	if limit := n.limiters[channel.Kind()]; limit != nil && !limit.allow(now) {
		n.mu.Lock()
		n.held[channel.Kind()] += len(incidents)
		n.mu.Unlock()
		n.logger.Warn("notification rate limit reached", slog.String("channel", string(channel.Kind())),
			slog.Int("incidents", len(incidents)))
		return
	}
	slots := make([]slot, 0, len(incidents)*len(chatIDs))
	writeRow := func(incidentID *int64, chatID string) bool {
		id, err := n.store.AddDelivery(ctx, store.Delivery{
			IncidentID: incidentID,
			Kind:       kind,
			Channel:    string(channel.Kind()),
			CreatedAt:  now,
			Status:     "pending",
		})
		if err != nil {
			n.logger.Warn("record delivery", slog.String("error", err.Error()))
			return false
		}
		slots = append(slots, slot{id: id, incidentID: incidentID, channel: string(channel.Kind()), chatID: chatID})
		return true
	}
	if len(incidents) == 0 {
		// Digest and test paths: one row per chat_id (or one row when
		// Telegram is not configured).
		for _, chatID := range chatIDs {
			writeRow(nil, chatID)
		}
	} else {
		for i := range incidents {
			id := incidents[i].ID
			for _, chatID := range chatIDs {
				writeRow(&id, chatID)
			}
		}
	}
	if len(slots) == 0 {
		return
	}
	n.attempt(ctx, channel, message, slots, 0)
}

// attempt sends one message, marks every delivery row with its own chat's
// outcome, and queues a retry for the chats that failed. A chat that
// already received the message is never retried, so one dead chat cannot
// duplicate pages to the working ones.
func (n *Notifier) attempt(ctx context.Context, channel Channel, message Message, slots []slot, failed int) {
	results := n.sendToChats(ctx, channel, message, slots)
	now := n.clock.Now()
	finalFailure := failed >= len(retryDelays())

	var retry []slot
	var lastErr error
	for _, s := range slots {
		outcome := outcomeFor(results, s.chatID)
		delivery := store.Delivery{ID: s.id, Attempts: failed + 1, Status: "sent"}
		switch {
		case outcome.err == nil:
			delivery.SentAt = &now
			if outcome.messageID != 0 {
				id := outcome.messageID
				delivery.TelegramMessageID = &id
			}
		case finalFailure:
			delivery.Status = "failed"
		default:
			delivery.Status = "pending"
			retry = append(retry, s)
		}
		if outcome.err != nil {
			text := safeError(outcome.err).Error()
			delivery.Error = &text
			lastErr = outcome.err
		}
		if updateErr := n.store.UpdateDelivery(ctx, delivery); updateErr != nil {
			n.logger.Warn("update delivery", slog.String("error", updateErr.Error()))
		}
	}
	if lastErr == nil {
		n.markPaged(now, len(slots) > 0)
		return
	}
	if finalFailure {
		n.logger.Warn("notification failed", slog.String("channel", string(channel.Kind())),
			slog.Int("attempts", failed+1), slog.String("error", safeError(lastErr).Error()))
		return
	}
	if len(retry) == 0 {
		return
	}
	delay := retryDelays()[failed]
	n.mu.Lock()
	n.retries = append(n.retries, &pendingDelivery{
		channel: channel,
		message: message,
		slots:   retry,
		attempt: failed + 1,
		at:      now.Add(delay),
	})
	n.mu.Unlock()
	n.logger.Warn("notification will be retried", slog.String("channel", string(channel.Kind())),
		slog.Int("chats", len(retry)), slog.Duration("in", delay), slog.String("error", safeError(lastErr).Error()))
}

// sendToChats posts a message through a channel and returns one result per
// recipient. Channels that do not fan out are reported at the empty chat.
func (n *Notifier) sendToChats(ctx context.Context, channel Channel, message Message, slots []slot) []chatSend {
	if fan, ok := channel.(fanOut); ok {
		return fan.SendEach(ctx, message, uniqueChats(slots))
	}
	return []chatSend{{err: channel.Send(ctx, message)}}
}

// uniqueChats returns the distinct chat ids of the slots, in first-seen
// order. Slots of a non-fanout channel carry the empty chat.
func uniqueChats(slots []slot) []string {
	seen := make(map[string]bool, len(slots))
	chats := make([]string, 0, len(slots))
	for _, s := range slots {
		if s.chatID == "" || seen[s.chatID] {
			continue
		}
		seen[s.chatID] = true
		chats = append(chats, s.chatID)
	}
	return chats
}

// outcomeFor returns the result for one chat. A non-fanout channel reports
// its single result at the empty chat, which is the chat id of its slots.
func outcomeFor(results []chatSend, chatID string) chatSend {
	for _, result := range results {
		if result.chatID == chatID {
			return result
		}
	}
	// Fall back to the single non-fanout result when there is one.
	if len(results) == 1 && results[0].chatID == "" {
		return results[0]
	}
	return chatSend{chatID: chatID, err: fmt.Errorf("no result for chat %q", chatID)}
}

// markPaged records a successful page for the stand-down announcement.
func (n *Notifier) markPaged(now time.Time, paged bool) {
	if !paged {
		return
	}
	n.mu.Lock()
	n.lastPageAt = now
	n.mu.Unlock()
}

// channelChatIDs returns the chat_ids this channel posts to. Channels
// without a fan-out list (webhook, telegram without chat_ids configured,
// SMS, generic test channels) return an empty slice and the pager
// treats them as one delivery row per incident.
func (n *Notifier) channelChatIDs(channel Channel) []string {
	return channel.ChatIDs()
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
			n.deliverDeferred(ctx)
			n.maybeStandDown(ctx)
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
		n.attempt(ctx, item.channel, item.message, item.slots, item.attempt)
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

// deliverDeferred sends the catch-up message for pages the gate held or
// cooled down once their ready time has passed. Incidents with the same
// channel, rule and subject collapse into one message.
func (n *Notifier) deliverDeferred(ctx context.Context) {
	now := n.clock.Now()
	n.mu.Lock()
	var due []*deferredPage
	for key, entry := range n.deferred {
		if entry.readyAt.After(now) {
			continue
		}
		due = append(due, entry)
		delete(n.deferred, key)
	}
	n.mu.Unlock()
	if len(due) == 0 {
		return
	}
	// Group by channel so one channel gets one catch-up message.
	byKind := make(map[Kind][]*deferredPage)
	var order []Kind
	for _, entry := range due {
		kind := entry.channel.Kind()
		if _, seen := byKind[kind]; !seen {
			order = append(order, kind)
		}
		byKind[kind] = append(byKind[kind], entry)
	}
	for _, kind := range order {
		entries := byKind[kind]
		incidents := make([]store.Incident, 0, len(entries))
		for _, entry := range entries {
			incidents = append(incidents, entry.incidents...)
		}
		header := fmt.Sprintf("%d held while quiet:", len(incidents))
		for _, entry := range entries {
			if entry.status == GateCooldown {
				header = fmt.Sprintf("%d still unresolved:", len(incidents))
			}
		}
		g := group{kind: entries[0].kind, incidents: incidents, reopened: map[int64]bool{}}
		f := n.formatter
		f.now = now
		f.deploys = n.deployContexts(ctx, &g)
		text := header + "\n\n" + f.text(g)
		message := Message{
			Text:     text,
			SMS:      truncateSMS(header + " raedar"),
			Document: f.document(g),
		}
		n.deliver(ctx, entries[0].channel, entries[0].kind, incidents, message)
	}
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
	// Only one notice per quiet period: once it has been sent after the
	// last page, stay silent.
	if !lastStandDown.IsZero() && (lastPage.IsZero() || lastStandDown.After(lastPage)) {
		return
	}
	count, err := n.store.OpenIncidentCount(ctx)
	if err != nil || count > 0 {
		return
	}
	hours := int(window.Round(time.Hour) / time.Hour)
	text := fmt.Sprintf("%s · all quiet for %d h", n.cfg.HostName, hours)
	message := Message{
		Text:     text,
		SMS:      truncateSMS("RAEDAR " + text),
		Document: digestDocument(n.cfg.HostName, now, text),
	}
	for _, channel := range n.digestChannels() {
		if err := channel.Send(ctx, message); err != nil {
			n.logger.Warn("stand-down message", slog.String("channel", string(channel.Kind())), slog.String("error", safeError(err).Error()))
		}
	}
	n.mu.Lock()
	n.lastStandDownAt = now
	n.mu.Unlock()
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

// SendReply answers an operator's Telegram command. The inbox goroutine
// calls this with the chat_id of the command. The Telegram channel is
// the only kind that supports replies; other channels return an error so
// the dispatcher can fall back to "no reply sent".
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
