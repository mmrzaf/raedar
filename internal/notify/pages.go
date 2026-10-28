package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
)

const (
	// pageRecheck bounds how long a page waits before its policy is evaluated
	// again. It makes an early silence deletion, a configuration change or a
	// newly created silence visible within a minute even when the original
	// wait was 30 days.
	pageRecheck = time.Minute
	// pageBatch is how many due pages one dispatcher pass handles.
	pageBatch = 200
	// notifyReconcileWindow is how far back the periodic planner looks for
	// resolved incidents whose resolution still needs a page.
	notifyReconcileWindow = 24 * time.Hour
	// oneShotMaxAge is how long a crash or other one-shot event may wait for
	// a page before it becomes stale.
	oneShotMaxAge = 24 * time.Hour
)

// pageTarget is one channel and recipient a page can go to.
type pageTarget struct {
	channel   Kind
	recipient string
}

// pageKey identifies a target in a map.
func pageKey(channel Kind, recipient string) string {
	return string(channel) + "\x00" + recipient
}

// reconcileAll applies the planner to every open incident and every incident
// resolved recently.
func (n *Notifier) reconcileAll(ctx context.Context) {
	incidents, err := n.store.IncidentsForNotify(ctx, n.clock.Now().Add(-notifyReconcileWindow))
	if err != nil {
		n.logger.Warn("planner: read incidents", slog.String("error", err.Error()))
		return
	}
	for i := range incidents {
		if err := n.reconcileIncident(ctx, incidents[i]); err != nil {
			n.logger.Warn("planner: reconcile incident",
				slog.Int64("incident", incidents[i].ID), slog.String("error", err.Error()))
		}
	}
}

// reconcileChanges applies the planner to the incidents a rules evaluation
// changed. The incident is read again so the planner always works on current
// state, even if another evaluation won the race.
func (n *Notifier) reconcileChanges(ctx context.Context, changes []rules.Change) {
	seen := make(map[int64]bool, len(changes))
	for _, change := range changes {
		if seen[change.Incident.ID] {
			continue
		}
		seen[change.Incident.ID] = true
		incident, err := n.store.IncidentByID(ctx, change.Incident.ID)
		if err != nil {
			n.logger.Warn("planner: read incident",
				slog.Int64("incident", change.Incident.ID), slog.String("error", err.Error()))
			continue
		}
		if incident == nil {
			continue
		}
		if err := n.reconcileIncident(ctx, *incident); err != nil {
			n.logger.Warn("planner: reconcile incident",
				slog.Int64("incident", incident.ID), slog.String("error", err.Error()))
		}
	}
}

// reconcileIncident creates or advances the pages of one incident episode.
func (n *Notifier) reconcileIncident(ctx context.Context, incident store.Incident) error {
	now := n.clock.Now()

	if incident.Acked() {
		return n.store.CancelPendingPages(ctx, incident.ID, "acked")
	}
	if !n.alertsAllowed(incident.AppID) {
		return n.store.CancelPendingPages(ctx, incident.ID, "app alerts disabled")
	}

	pages, err := n.store.PagesForEpisode(ctx, incident.ID, incident.Episode)
	if err != nil {
		return err
	}
	byKey := make(map[string]store.Page, len(pages))
	for _, page := range pages {
		byKey[pageKey(Kind(page.Channel), page.Recipient)] = page
	}

	if incident.ResolvedAt != nil && !incident.OneShot {
		return n.reconcileResolved(ctx, incident, pages, now)
	}

	// A one-shot resolved incident is an event, not an ongoing condition.
	// Its opening page is still worth sending, but it never gets a
	// resolution. Very old one-shots are dropped rather than delivered as
	// stale news.
	if incident.OneShot && incident.ResolvedAt != nil {
		if now.Sub(incident.OpenedAt) > oneShotMaxAge {
			return n.store.CancelPendingPages(ctx, incident.ID, "stale one-shot")
		}
		for _, page := range pages {
			if page.Status == store.PageAnnounced {
				page.Status = store.PageResolved
				page.ResolvedAt = &now
				page.Reason = ""
				page.SilenceID = nil
				if err := n.store.UpdatePage(ctx, page); err != nil {
					return err
				}
			}
		}
	}

	routed := n.routedTargets(incident.Severity)

	for key, target := range routed {
		page, ok := byKey[key]
		if !ok {
			if err := n.seedPage(ctx, incident, target, now); err != nil {
				return err
			}
			continue
		}
		if err := n.advancePage(ctx, incident, page, now); err != nil {
			return err
		}
	}

	// A channel that is no longer routed for this severity must not keep a
	// pending page: the operator removed it. Announced pages stay so the
	// eventual resolution still reaches the channel that saw the opening.
	for _, page := range pages {
		if page.Status != store.PagePending {
			continue
		}
		if _, ok := routed[pageKey(Kind(page.Channel), page.Recipient)]; !ok {
			if err := n.store.CancelPage(ctx, page.ID, "not routed"); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileResolved turns announced pages into resolutions and cancels pages
// that never announced anything. It never creates a page for a resolved
// incident.
func (n *Notifier) reconcileResolved(ctx context.Context, incident store.Incident, pages []store.Page, now time.Time) error {
	for _, page := range pages {
		switch page.Status {
		case store.PagePending:
			if err := n.store.CancelPage(ctx, page.ID, "resolved before announced"); err != nil {
				return err
			}
		case store.PageAnnounced:
			page.Status = store.PageResolving
			page.Kind = store.DeliveryResolved
			page.DueAt = now
			page.Reason = "resolved"
			page.SilenceID = nil
			if err := n.store.UpdatePage(ctx, page); err != nil {
				return err
			}
			n.logger.Info("resolution queued", slog.Int64("incident", incident.ID),
				slog.String("channel", page.Channel), slog.String("recipient", page.Recipient))
		}
	}
	return nil
}

// seedPage creates the first page of an episode, using an already-sent
// delivery from an earlier Raedar version or a crash to avoid paging twice.
func (n *Notifier) seedPage(ctx context.Context, incident store.Incident, target pageTarget, now time.Time) error {
	sentAt, err := n.store.SentOpeningAt(ctx, incident.ID, incident.OpenedAt, string(target.channel), target.recipient)
	if err != nil {
		n.logger.Warn("planner: read sent opening", slog.Int64("incident", incident.ID),
			slog.String("error", err.Error()))
	}
	page := store.Page{
		IncidentID: incident.ID,
		Episode:    incident.Episode,
		Channel:    string(target.channel),
		Recipient:  target.recipient,
		Status:     store.PagePending,
		Kind:       store.DeliveryOpened,
		Severity:   incident.Severity,
		DueAt:      now,
	}
	if sentAt != nil {
		page.Status = store.PageAnnounced
		page.AnnouncedAt = sentAt
		n.logger.Info("page seeded as announced", slog.Int64("incident", incident.ID),
			slog.String("channel", page.Channel), slog.String("recipient", page.Recipient))
	} else {
		n.logger.Info("page queued", slog.Int64("incident", incident.ID),
			slog.String("rule", incident.Rule), slog.String("subject", incident.Subject),
			slog.String("severity", string(incident.Severity)), slog.String("channel", page.Channel),
			slog.String("recipient", page.Recipient))
	}
	_, err = n.store.CreatePage(ctx, page)
	return err
}

// advancePage moves an existing page towards its next message.
func (n *Notifier) advancePage(ctx context.Context, incident store.Incident, page store.Page, now time.Time) error {
	switch page.Status {
	case store.PageCancelled:
		return nil
	case store.PageResolved:
		if incident.ResolvedAt != nil {
			return nil
		}
		// The incident reopened; the episode should have changed, but make
		// the page usable rather than leaving it silent.
		page.Status = store.PagePending
		page.Kind = store.DeliveryOpened
		page.Severity = incident.Severity
		page.DueAt = now
		page.Reason = "reopened"
		return n.store.UpdatePage(ctx, page)
	case store.PageResolving:
		if incident.ResolvedAt != nil {
			return nil
		}
		page.Status = store.PagePending
		page.Kind = store.DeliveryOpened
		page.Severity = incident.Severity
		page.DueAt = now
		page.Reason = "reopened"
		return n.store.UpdatePage(ctx, page)
	case store.PageAnnounced:
		if severityLevel(incident.Severity) > severityLevel(page.Severity) {
			page.Status = store.PagePending
			page.Kind = store.DeliveryEscalated
			page.Severity = incident.Severity
			page.DueAt = now
			page.Reason = "escalated"
			page.SilenceID = nil
			n.logger.Warn("escalation queued", slog.Int64("incident", incident.ID),
				slog.String("rule", incident.Rule), slog.String("subject", incident.Subject),
				slog.String("channel", page.Channel), slog.String("recipient", page.Recipient))
			return n.store.UpdatePage(ctx, page)
		}
		return nil
	case store.PagePending:
		if severityLevel(incident.Severity) > severityLevel(page.Severity) {
			page.Severity = incident.Severity
			page.DueAt = now
			page.Reason = "escalated"
			page.SilenceID = nil
			if page.AnnouncedAt != nil {
				page.Kind = store.DeliveryEscalated
			}
			return n.store.UpdatePage(ctx, page)
		}
		return nil
	}
	return nil
}

// routedTargets returns every channel and recipient the severity routes to.
func (n *Notifier) routedTargets(severity store.Severity) map[string]pageTarget {
	targets := make(map[string]pageTarget)
	for _, kind := range n.routing(severity) {
		channel := n.channels[kind]
		if channel == nil {
			continue
		}
		recipients := channel.ChatIDs()
		if len(recipients) == 0 {
			recipients = []string{""}
		}
		for _, recipient := range recipients {
			targets[pageKey(kind, recipient)] = pageTarget{channel: kind, recipient: recipient}
		}
	}
	return targets
}

// deliverDue runs the dispatcher once. It sends every page whose policy allows
// it, reschedules the rest, and never loses a page because a channel was down.
func (n *Notifier) deliverDue(ctx context.Context) {
	now := n.clock.Now()
	pages, err := n.store.DuePages(ctx, now, pageBatch)
	if err != nil {
		n.logger.Warn("dispatcher: read due pages", slog.String("error", err.Error()))
		return
	}
	if len(pages) == 0 {
		return
	}

	incidents := make(map[int64]*store.Incident)
	groups := make(map[string][]sendablePage)
	var order []string

	for _, page := range pages {
		incident, ok := incidents[page.IncidentID]
		if !ok {
			incident, err = n.store.IncidentByID(ctx, page.IncidentID)
			if err != nil {
				n.logger.Warn("dispatcher: read incident", slog.Int64("incident", page.IncidentID),
					slog.String("error", err.Error()))
				continue
			}
			incidents[page.IncidentID] = incident
		}
		if incident == nil {
			n.cancelPage(ctx, page, "incident removed", now)
			continue
		}
		if incident.Acked() {
			n.cancelPage(ctx, page, "acked", now)
			continue
		}
		if !n.alertsAllowed(incident.AppID) {
			n.cancelPage(ctx, page, "app alerts disabled", now)
			continue
		}
		if incident.ResolvedAt != nil && !incident.OneShot && page.Kind != store.DeliveryResolved {
			n.cancelPage(ctx, page, "resolved before announced", now)
			continue
		}
		if incident.ResolvedAt == nil && page.Kind == store.DeliveryResolved {
			n.cancelPage(ctx, page, "episode reopened", now)
			continue
		}
		if page.Kind != store.DeliveryResolved && incident.OneShot && incident.ResolvedAt != nil &&
			now.Sub(incident.OpenedAt) > oneShotMaxAge {
			n.cancelPage(ctx, page, "stale one-shot", now)
			continue
		}
		channel := n.channels[Kind(page.Channel)]
		if channel == nil {
			n.cancelPage(ctx, page, "channel removed", now)
			continue
		}
		if !recipientConfigured(channel, page.Recipient) {
			n.cancelPage(ctx, page, "recipient removed", now)
			continue
		}

		decision := n.gate.Evaluate(ctx, DecisionInputs{
			Incident: *incident,
			Kind:     page.Kind,
			Channel:  Kind(page.Channel),
			Now:      now,
			Attempts: page.Attempts,
		})
		switch decision.Action {
		case ActionDrop:
			n.cancelPage(ctx, page, decision.Reason, now)
		case ActionWait:
			n.waitPage(ctx, page, decision, now)
		case ActionSend:
			key := pageKey(Kind(page.Channel), page.Recipient) + "\x00" + string(page.Kind)
			if _, ok := groups[key]; !ok {
				order = append(order, key)
			}
			groups[key] = append(groups[key], sendablePage{page: page, incident: *incident})
		}
	}

	sort.Strings(order)
	for _, key := range order {
		n.sendPages(ctx, groups[key], now)
	}
}

// sendPages sends one message for every page in a group. All pages share a
// channel, recipient and kind, so one message can announce several incidents.
func (n *Notifier) sendPages(ctx context.Context, pages []sendablePage, now time.Time) {
	if len(pages) == 0 {
		return
	}
	channel := n.channels[Kind(pages[0].page.Channel)]
	if channel == nil {
		return
	}
	limit := n.limiters[channel.Kind()]
	if limit != nil && !limit.allow(now) {
		for _, item := range pages {
			n.reschedule(ctx, item.page, "rate limited", now, now.Add(pageRecheck), nil)
		}
		return
	}

	rows := make([]store.Delivery, 0, len(pages))
	ready := make([]sendablePage, 0, len(pages))
	for _, item := range pages {
		pageID := item.page.ID
		incidentID := item.incident.ID
		id, err := n.store.AddDelivery(ctx, store.Delivery{
			IncidentID: &incidentID,
			PageID:     &pageID,
			Kind:       item.page.Kind,
			Channel:    item.page.Channel,
			Recipient:  item.page.Recipient,
			CreatedAt:  now,
			Status:     "pending",
		})
		if err != nil {
			n.logger.Warn("dispatcher: record delivery", slog.Int64("incident", incidentID),
				slog.String("error", err.Error()))
			n.reschedule(ctx, item.page, "record delivery failed", now, now.Add(pageRecheck), nil)
			continue
		}
		rows = append(rows, store.Delivery{
			ID:         id,
			IncidentID: &incidentID,
			PageID:     &pageID,
			Kind:       item.page.Kind,
			Channel:    item.page.Channel,
			Recipient:  item.page.Recipient,
		})
		ready = append(ready, item)
	}
	if len(ready) == 0 {
		return
	}

	g := group{kind: store.DeliveryOpened, incidents: make([]store.Incident, 0, len(ready)), reopened: make(map[int64]bool)}
	if ready[0].page.Kind == store.DeliveryResolved {
		g.kind = store.DeliveryResolved
	} else if ready[0].page.Kind == store.DeliveryEscalated {
		g.kind = store.DeliveryEscalated
	}
	for _, item := range ready {
		g.incidents = append(g.incidents, item.incident)
		g.reopened[item.incident.ID] = item.page.Kind == store.DeliveryOpened && item.incident.Episode > 1
	}
	f := n.formatter
	f.now = now
	f.deploys = n.deployContexts(ctx, &g)
	message := Message{Text: f.text(g), SMS: f.smsText(g), Document: f.document(g)}

	messageID, err := n.sendOne(ctx, channel, message, ready[0].page.Recipient)
	if err != nil {
		if limit != nil {
			limit.undo()
		}
		safe := SafeError(err).Error()
		for _, row := range rows {
			row.Status = "failed"
			row.Attempts = 1
			row.Error = &safe
			if updateErr := n.store.UpdateDelivery(ctx, row); updateErr != nil {
				n.logger.Warn("dispatcher: update delivery", slog.String("error", updateErr.Error()))
			}
		}
		retryIn := time.Duration(0)
		for _, item := range ready {
			item.page.Attempts++
			delay := pageRetryDelay(item.page.Attempts)
			if retryIn == 0 {
				retryIn = delay
			}
			item.page.LastError = &safe
			n.reschedule(ctx, item.page, "send failed", now, now.Add(delay), nil)
		}
		n.logger.Warn("page delivery failed", slog.String("channel", string(channel.Kind())),
			slog.String("recipient", ready[0].page.Recipient),
			slog.Duration("retry_in", retryIn),
			slog.String("error", safe))
		return
	}

	for i := range rows {
		row := rows[i]
		row.Status = "sent"
		row.Attempts = 1
		row.SentAt = &now
		if channel.Kind() == KindTelegram {
			if chat, err := strconv.ParseInt(row.Recipient, 10, 64); err == nil {
				row.TelegramChatID = &chat
			}
			if messageID != 0 {
				row.TelegramMessageID = &messageID
			}
		}
		if updateErr := n.store.UpdateDelivery(ctx, row); updateErr != nil {
			n.logger.Warn("dispatcher: update delivery", slog.String("error", updateErr.Error()))
		}
	}
	for _, item := range ready {
		item.page.Attempts++
		item.page.LastError = nil
		item.page.Reason = ""
		item.page.SilenceID = nil
		item.page.DueAt = now
		if item.page.Kind == store.DeliveryResolved {
			item.page.Status = store.PageResolved
			item.page.ResolvedAt = &now
		} else {
			item.page.Status = store.PageAnnounced
			item.page.AnnouncedAt = &now
		}
		if updateErr := n.store.UpdatePage(ctx, item.page); updateErr != nil {
			n.logger.Warn("dispatcher: update page", slog.String("error", updateErr.Error()))
		}
	}
	n.markPaged(now)
}

// sendablePage is one page and its current incident.
type sendablePage struct {
	page     store.Page
	incident store.Incident
}

// sendOne posts a message to one recipient of a channel and returns the
// Telegram message id when the channel reports one.
func (n *Notifier) sendOne(ctx context.Context, channel Channel, message Message, recipient string) (int64, error) {
	if fan, ok := channel.(fanOut); ok {
		results := fan.SendEach(ctx, message, []string{recipient})
		if len(results) != 1 {
			return 0, fmt.Errorf("channel returned %d results for one recipient", len(results))
		}
		return results[0].messageID, results[0].err
	}
	return 0, channel.Send(ctx, message)
}

// waitPage reschedules a page until its policy allows it, at most one minute
// at a time so a deleted silence or a changed configuration is noticed.
func (n *Notifier) waitPage(ctx context.Context, page store.Page, decision Decision, now time.Time) {
	due := decision.ReadyAt
	if due.IsZero() || due.After(now.Add(pageRecheck)) {
		due = now.Add(pageRecheck)
	}
	if !due.After(now) {
		due = now.Add(pageRecheck)
	}
	changed := page.Reason != decision.Reason ||
		(page.SilenceID == nil) != (decision.SilenceID == nil) ||
		(page.SilenceID != nil && decision.SilenceID != nil && *page.SilenceID != *decision.SilenceID)
	if changed {
		status := "held"
		if decision.SilenceID != nil {
			status = "silenced"
		} else if decision.Reason == "pager cooldown" {
			status = "cooldown"
		}
		n.recordSuppressed(ctx, page, status, decision.Reason, decision.SilenceID, now)
	}
	page.DueAt = due
	page.Reason = decision.Reason
	page.SilenceID = decision.SilenceID
	if err := n.store.UpdatePage(ctx, page); err != nil {
		n.logger.Warn("dispatcher: wait page", slog.Int64("page", page.ID), slog.String("error", err.Error()))
	}
	if page.Status == store.PagePending || page.Status == store.PageResolving {
		n.logger.Info("page delayed", slog.Int64("incident", page.IncidentID),
			slog.Int64("page", page.ID), slog.String("channel", page.Channel),
			slog.String("recipient", page.Recipient), slog.String("reason", decision.Reason),
			slog.Time("at", due))
	}
}

// reschedule moves a page to a new due time without changing its policy state.
func (n *Notifier) reschedule(ctx context.Context, page store.Page, reason string, now, due time.Time, silenceID *int64) {
	page.DueAt = due
	page.Reason = reason
	page.SilenceID = silenceID
	if err := n.store.UpdatePage(ctx, page); err != nil {
		n.logger.Warn("dispatcher: reschedule page", slog.Int64("page", page.ID), slog.String("error", err.Error()))
	}
}

// cancelPage cancels a page, records the audit row, and logs the decision.
func (n *Notifier) cancelPage(ctx context.Context, page store.Page, reason string, now time.Time) {
	if page.Status == store.PagePending || page.Status == store.PageResolving {
		status := "dropped"
		if reason == "acked" {
			status = "acked"
		}
		n.recordSuppressed(ctx, page, status, reason, nil, now)
		if err := n.store.CancelPage(ctx, page.ID, reason); err != nil {
			n.logger.Warn("dispatcher: cancel page", slog.Int64("page", page.ID), slog.String("error", err.Error()))
			return
		}
	}
	n.logger.Warn("page cancelled", slog.Int64("incident", page.IncidentID), slog.Int64("page", page.ID),
		slog.String("channel", page.Channel), slog.String("recipient", page.Recipient),
		slog.String("reason", reason))
}

// recordSuppressed writes one audit row for a page that did not go out.
func (n *Notifier) recordSuppressed(ctx context.Context, page store.Page, status, reason string, silenceID *int64, now time.Time) {
	incidentID := page.IncidentID
	pageID := page.ID
	if _, err := n.store.AddDelivery(ctx, store.Delivery{
		IncidentID: &incidentID,
		PageID:     &pageID,
		Kind:       page.Kind,
		Channel:    page.Channel,
		Recipient:  page.Recipient,
		CreatedAt:  now,
		Status:     status,
		Reason:     &reason,
		SilenceID:  silenceID,
	}); err != nil {
		n.logger.Warn("dispatcher: record suppressed delivery",
			slog.Int64("incident", incidentID), slog.String("error", err.Error()))
	}
}

// pageRetryDelay returns the delay after the given number of failed attempts.
func pageRetryDelay(attempts int) time.Duration {
	delays := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute,
		15 * time.Minute, 30 * time.Minute, time.Hour}
	if attempts <= 0 {
		return delays[0]
	}
	if attempts > len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempts-1]
}

// recipientConfigured reports whether a page still targets a configured
// recipient. A channel that does not fan out has the empty recipient.
func recipientConfigured(channel Channel, recipient string) bool {
	ids := channel.ChatIDs()
	if len(ids) == 0 {
		return recipient == ""
	}
	for _, id := range ids {
		if id == recipient {
			return true
		}
	}
	return false
}

// severityLevel orders the severities for escalation checks.
func severityLevel(severity store.Severity) int {
	switch severity {
	case store.SeverityCritical:
		return 2
	case store.SeverityWarning:
		return 1
	default:
		return 0
	}
}

// alertsAllowed reports whether the app still allows messages.
func (n *Notifier) alertsAllowed(appID *int64) bool {
	if appID == nil || n.model == nil {
		return true
	}
	model := n.model.Model()
	if model == nil {
		return true
	}
	for _, app := range model.Apps {
		if app.ID == *appID {
			return app.Alerts
		}
	}
	return true
}
