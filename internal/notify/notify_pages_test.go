package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/store"
)

// fakeStore is an in-memory stand-in for the notifier's store. It models the
// pages table, the deliveries audit and the incident rows closely enough to
// exercise the planner and dispatcher.
type fakeStore struct {
	mu           sync.Mutex
	deliveries   map[int64]store.Delivery
	nextDelivery int64
	events       map[store.EventKind][]store.Event
	silences     []store.Silence
	incidents    map[int64]store.Incident
	pages        map[int64]store.Page
	nextPage     int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		deliveries: map[int64]store.Delivery{},
		events:     map[store.EventKind][]store.Event{},
		incidents:  map[int64]store.Incident{},
		pages:      map[int64]store.Page{},
	}
}

func (s *fakeStore) addIncident(inc store.Incident) store.Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc.ID == 0 {
		inc.ID = int64(len(s.incidents) + 1)
	}
	if inc.Episode == 0 {
		inc.Episode = 1
	}
	s.incidents[inc.ID] = inc
	return inc
}

func (s *fakeStore) updateIncident(inc store.Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incidents[inc.ID] = inc
}

func (s *fakeStore) addSilence(silence store.Silence) *store.Silence {
	s.mu.Lock()
	defer s.mu.Unlock()
	if silence.ID == 0 {
		silence.ID = int64(len(s.silences) + 1)
	}
	s.silences = append(s.silences, silence)
	return &silence
}

func (s *fakeStore) AddDelivery(_ context.Context, delivery store.Delivery) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextDelivery++
	delivery.ID = s.nextDelivery
	s.deliveries[delivery.ID] = delivery
	return delivery.ID, nil
}

func (s *fakeStore) UpdateDelivery(_ context.Context, delivery store.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.deliveries[delivery.ID]
	if !ok {
		return errors.New("delivery not found")
	}
	current.SentAt = delivery.SentAt
	current.Attempts = delivery.Attempts
	current.Status = delivery.Status
	current.Error = delivery.Error
	current.Recipient = delivery.Recipient
	current.PageID = delivery.PageID
	current.TelegramChatID = delivery.TelegramChatID
	current.TelegramMessageID = delivery.TelegramMessageID
	s.deliveries[delivery.ID] = current
	return nil
}

func (s *fakeStore) Events(_ context.Context, kind store.EventKind, _, _ time.Time) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Event(nil), s.events[kind]...), nil
}

func (s *fakeStore) ActiveSilences(_ context.Context, now time.Time) ([]store.Silence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var active []store.Silence
	for _, silence := range s.silences {
		if !silence.StartsAt.After(now) && silence.EndsAt.After(now) {
			active = append(active, silence)
		}
	}
	return active, nil
}

func (s *fakeStore) Announcements(_ context.Context, rule, subject, channel string, since time.Time) (int, *time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	episodes := map[[2]int64]bool{}
	var last *time.Time
	for _, page := range s.pages {
		if page.Channel != channel || page.AnnouncedAt == nil || page.AnnouncedAt.Before(since) {
			continue
		}
		incident, ok := s.incidents[page.IncidentID]
		if !ok || incident.Rule != rule || incident.Subject != subject {
			continue
		}
		episodes[[2]int64{page.IncidentID, page.Episode}] = true
		if last == nil || page.AnnouncedAt.After(*last) {
			at := *page.AnnouncedAt
			last = &at
		}
	}
	return len(episodes), last, nil
}

func (s *fakeStore) IncidentByID(_ context.Context, id int64) (*store.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	incident, ok := s.incidents[id]
	if !ok {
		return nil, nil
	}
	copy := incident
	return &copy, nil
}

func (s *fakeStore) IncidentsForNotify(_ context.Context, since time.Time) ([]store.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var incidents []store.Incident
	for _, incident := range s.incidents {
		if incident.ResolvedAt == nil || !incident.ResolvedAt.Before(since) {
			incidents = append(incidents, incident)
		}
	}
	sort.Slice(incidents, func(i, j int) bool { return incidents[i].ID < incidents[j].ID })
	return incidents, nil
}

func (s *fakeStore) PagesForEpisode(_ context.Context, incidentID, episode int64) ([]store.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pages []store.Page
	for _, page := range s.pages {
		if page.IncidentID == incidentID && page.Episode == episode {
			pages = append(pages, page)
		}
	}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].Channel != pages[j].Channel {
			return pages[i].Channel < pages[j].Channel
		}
		return pages[i].Recipient < pages[j].Recipient
	})
	return pages, nil
}

func (s *fakeStore) CreatePage(_ context.Context, page store.Page) (store.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.pages {
		if existing.IncidentID == page.IncidentID && existing.Episode == page.Episode &&
			existing.Channel == page.Channel && existing.Recipient == page.Recipient {
			return existing, nil
		}
	}
	s.nextPage++
	page.ID = s.nextPage
	if page.CreatedAt.IsZero() {
		page.CreatedAt = page.DueAt
	}
	page.UpdatedAt = page.DueAt
	s.pages[page.ID] = page
	return page, nil
}

func (s *fakeStore) UpdatePage(_ context.Context, page store.Page) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pages[page.ID]; !ok {
		return errors.New("page not found")
	}
	s.pages[page.ID] = page
	return nil
}

func (s *fakeStore) CancelPage(_ context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	page, ok := s.pages[id]
	if !ok {
		return errors.New("page not found")
	}
	page.Status = store.PageCancelled
	page.Reason = reason
	s.pages[id] = page
	return nil
}

func (s *fakeStore) CancelPendingPages(_ context.Context, incidentID int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, page := range s.pages {
		if page.IncidentID != incidentID {
			continue
		}
		if page.Status == store.PagePending || page.Status == store.PageResolving {
			page.Status = store.PageCancelled
			page.Reason = reason
			s.pages[id] = page
		}
	}
	return nil
}

func (s *fakeStore) DuePages(_ context.Context, now time.Time, limit int) ([]store.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pages []store.Page
	for _, page := range s.pages {
		if (page.Status == store.PagePending || page.Status == store.PageResolving) && !page.DueAt.After(now) {
			pages = append(pages, page)
		}
	}
	sort.Slice(pages, func(i, j int) bool {
		if !pages[i].DueAt.Equal(pages[j].DueAt) {
			return pages[i].DueAt.Before(pages[j].DueAt)
		}
		return pages[i].ID < pages[j].ID
	})
	if limit > 0 && len(pages) > limit {
		pages = pages[:limit]
	}
	return pages, nil
}

func (s *fakeStore) SentOpeningAt(_ context.Context, incidentID int64, openedAt time.Time, channel, recipient string) (*time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last *time.Time
	for _, delivery := range s.deliveries {
		if delivery.IncidentID == nil || *delivery.IncidentID != incidentID {
			continue
		}
		if delivery.Kind != store.DeliveryOpened && delivery.Kind != store.DeliveryEscalated {
			continue
		}
		if delivery.Status != "sent" || delivery.CreatedAt.Before(openedAt) || delivery.Channel != channel {
			continue
		}
		match := delivery.Recipient == recipient ||
			(delivery.Recipient == "" && channel != string(KindTelegram))
		if channel == string(KindTelegram) && delivery.TelegramChatID != nil && recipient != "" {
			if chat, err := strconv.ParseInt(recipient, 10, 64); err == nil && *delivery.TelegramChatID == chat {
				match = true
			}
		}
		if !match {
			continue
		}
		at := delivery.CreatedAt
		if delivery.SentAt != nil {
			at = *delivery.SentAt
		}
		if last == nil || at.After(*last) {
			last = &at
		}
	}
	return last, nil
}

func (s *fakeStore) FailPendingDeliveries(_ context.Context, reason string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for id, delivery := range s.deliveries {
		if delivery.Status == "pending" {
			delivery.Status = "failed"
			delivery.Error = &reason
			s.deliveries[id] = delivery
			count++
		}
	}
	return count, nil
}

func (s *fakeStore) OpenIncidentCount(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, incident := range s.incidents {
		if incident.ResolvedAt == nil {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) byChannel(channel Kind) []store.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deliveries []store.Delivery
	for _, delivery := range s.deliveries {
		if delivery.Channel == string(channel) {
			deliveries = append(deliveries, delivery)
		}
	}
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].ID < deliveries[j].ID })
	return deliveries
}

func (s *fakeStore) pageFor(incidentID int64, channel Kind, recipient string) *store.Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, page := range s.pages {
		if page.IncidentID == incidentID && page.Channel == string(channel) && page.Recipient == recipient {
			copy := page
			return &copy
		}
	}
	return nil
}

func (s *fakeStore) removeSilence(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.silences[:0]
	for _, silence := range s.silences {
		if silence.ID != id {
			kept = append(kept, silence)
		}
	}
	s.silences = kept
}

// fakeChannel records the messages it is sent.
type fakeChannel struct {
	kind  Kind
	mu    sync.Mutex
	sent  []Message
	fails int
}

func (c *fakeChannel) Kind() Kind        { return c.kind }
func (c *fakeChannel) ChatIDs() []string { return nil }

func (c *fakeChannel) Send(_ context.Context, message Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fails > 0 {
		c.fails--
		return errFakeSend
	}
	c.sent = append(c.sent, message)
	return nil
}

func (c *fakeChannel) messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.sent...)
}

var errFakeSend = &fakeError{}

type fakeError struct{}

func (e *fakeError) Error() string { return "fake send failure" }

// scriptedChannel is a fan-out channel with a scripted outcome per recipient.
type scriptedChannel struct {
	kind     Kind
	chatIDs  []string
	mu       sync.Mutex
	outcomes map[string]error
	calls    map[string]int
}

func (c *scriptedChannel) Kind() Kind        { return c.kind }
func (c *scriptedChannel) ChatIDs() []string { return append([]string(nil), c.chatIDs...) }
func (c *scriptedChannel) Send(ctx context.Context, message Message) error {
	var errs []error
	for _, result := range c.SendEach(ctx, message, nil) {
		errs = append(errs, result.err)
	}
	return errors.Join(errs...)
}
func (c *scriptedChannel) SendEach(_ context.Context, _ Message, chats []string) []chatSend {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(chats) == 0 {
		chats = c.chatIDs
	}
	results := make([]chatSend, 0, len(chats))
	for _, chat := range chats {
		if c.calls == nil {
			c.calls = map[string]int{}
		}
		c.calls[chat]++
		results = append(results, chatSend{chatID: chat, messageID: int64(1000 + c.calls[chat]), err: c.outcomes[chat]})
	}
	return results
}
func (c *scriptedChannel) callsFor(chat string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[chat]
}
func (c *scriptedChannel) setOutcome(chat string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes[chat] = err
}

// fakeModel is a discovery model with configured app alert flags.
type fakeModel struct {
	model *discovery.Model
}

func (m fakeModel) Model() *discovery.Model { return m.model }

// newNotifierForStore returns a notifier around an existing store so a test
// can simulate a process restart.
func newNotifierForStore(t *testing.T, st *fakeStore, clk *testClock, channels ...Channel) *Notifier {
	t.Helper()
	cfg := testConfig(t)
	cfg.HostName = "host-1"
	cfg.PublicURL = "https://raedar.example.com"
	cfg.Notify.WarningChannels = []string{"telegram"}
	cfg.Notify.CriticalChannels = []string{"telegram"}
	cfg.Notify.DigestChannels = []string{"telegram"}
	notifier := &Notifier{
		cfg:        cfg,
		store:      st,
		logger:     discardLogger(),
		clock:      clk,
		channels:   map[Kind]Channel{},
		limiters:   map[Kind]*limiter{},
		lastPageAt: clk.Now(),
		formatter:  formatter{host: cfg.HostName, publicURL: cfg.PublicURL, location: time.UTC, now: clk.now},
	}
	for _, channel := range channels {
		notifier.channels[channel.Kind()] = channel
		notifier.limiters[channel.Kind()] = &limiter{limit: cfg.Notify.RateLimit, window: time.Hour}
	}
	notifier.gate = &Gate{Store: st, Logger: discardLogger(), PageCritical: true}
	return notifier
}

func newTestNotifier(t *testing.T, channels ...Channel) (*Notifier, *fakeStore, *testClock) {
	t.Helper()
	clk := &testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}
	st := newFakeStore()
	return newNotifierForStore(t, st, clk, channels...), st, clk
}

func testIncident(id int64, severity store.Severity) store.Incident {
	return store.Incident{
		ID: id, Episode: 1, Rule: "route.errors", Subject: "shop@docker", Severity: severity,
		OpenedAt: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC), Summary: "5xx errors",
		Details: []byte(`{"rate":0.064,"app":"shop"}`),
	}
}

func runNotifierOnce(t *testing.T, n *Notifier) {
	t.Helper()
	n.workMu.Lock()
	defer n.workMu.Unlock()
	n.reconcileAll(t.Context())
	n.deliverDue(t.Context())
}

func TestNotifierGroupsAndRoutes(t *testing.T) {
	warning := &fakeChannel{kind: KindTelegram}
	critical := &fakeChannel{kind: KindSMS}
	notifier, st, _ := newTestNotifier(t, warning, critical)
	notifier.cfg.Notify.WarningChannels = []string{"telegram"}
	notifier.cfg.Notify.CriticalChannels = []string{"telegram", "sms"}
	a := st.addIncident(testIncident(1, store.SeverityWarning))
	b := st.addIncident(testIncident(2, store.SeverityCritical))
	b.Subject = "other@docker"
	st.updateIncident(b)

	notifier.IncidentsChanged(t.Context(), []rules.Change{
		{Incident: a, Kind: rules.ChangeOpened},
		{Incident: b, Kind: rules.ChangeOpened},
	})
	if got := len(warning.messages()); got != 1 {
		t.Fatalf("telegram messages = %d, want one group", got)
	}
	if got := len(critical.messages()); got != 1 {
		t.Fatalf("SMS messages = %d, want the critical incident", got)
	}
	if got := len(st.byChannel(KindTelegram)); got != 2 {
		t.Errorf("telegram deliveries = %d, want one per incident", got)
	}
}

func TestNotifierSilenceExpiresAndPages(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	silence := st.addSilence(store.Silence{
		Kind: store.SilenceOneShotMute, Rule: gateStr("route.errors"), Subject: gateStr("shop@docker"),
		Reason: "maintenance", StartsAt: clk.Now().Add(-time.Minute), EndsAt: clk.Now().Add(5 * time.Minute),
	})
	incident := st.addIncident(testIncident(1, store.SeverityCritical))

	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want none while silenced", got)
	}
	page := st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PagePending || page.Reason == "" {
		t.Fatalf("page = %+v, want a pending page with a reason", page)
	}
	if page.SilenceID == nil || *page.SilenceID != silence.ID {
		t.Fatalf("page silence = %v, want silence %d", page.SilenceID, silence.ID)
	}
	rows := st.byChannel(KindTelegram)
	if len(rows) != 1 || rows[0].Status != "silenced" || rows[0].Reason == nil {
		t.Fatalf("deliveries = %+v, want one silenced row with a reason", rows)
	}

	clk.now = clk.now.Add(5*time.Minute + time.Second)
	notifier.workMu.Lock()
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after the silence = %d, want the page", got)
	}
	page = st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PageAnnounced || page.AnnouncedAt == nil {
		t.Fatalf("page after send = %+v, want announced", page)
	}
}

func TestNotifierAutoDeploySilenceSparesCritical(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	appID := int64(7)
	st.addSilence(store.Silence{
		Kind: store.SilenceAutoDeploy, AppID: &appID, Reason: "deploy",
		StartsAt: clk.Now().Add(-time.Minute), EndsAt: clk.Now().Add(time.Hour),
	})
	critical := testIncident(1, store.SeverityCritical)
	critical.AppID = &appID
	critical.Rule = "container.restarting"
	critical.Subject = "waiotech / worker #1"
	incident := st.addIncident(critical)

	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("critical messages = %d, want the page immediately", got)
	}
}

func TestNotifierAutoDeploySilenceHoldsWarning(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	appID := int64(7)
	st.addSilence(store.Silence{
		Kind: store.SilenceAutoDeploy, AppID: &appID, Reason: "deploy",
		StartsAt: clk.Now().Add(-time.Minute), EndsAt: clk.Now().Add(time.Hour),
	})
	warning := testIncident(1, store.SeverityWarning)
	warning.AppID = &appID
	warning.Rule = "container.crashed"
	incident := st.addIncident(warning)

	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("warning messages = %d, want none during the deploy window", got)
	}
}

func TestNotifierAckCancelsPending(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	st.addSilence(store.Silence{
		Kind: store.SilenceMaintenanceWindow, Rule: gateStr("route.errors"), Subject: gateStr("shop@docker"),
		Reason: "maintenance", StartsAt: clk.Now().Add(-time.Minute), EndsAt: clk.Now().Add(time.Hour),
	})
	incident := st.addIncident(testIncident(1, store.SeverityCritical))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})

	ackedAt := clk.Now()
	incident.AckedAt = &ackedAt
	st.updateIncident(incident)
	notifier.workMu.Lock()
	notifier.reconcileAll(t.Context())
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()

	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want none after ack", got)
	}
	page := st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PageCancelled || page.Reason != "acked" {
		t.Fatalf("page = %+v, want cancelled acked", page)
	}
}

func TestNotifierResolutionOnlyForAnnounced(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	incident := st.addIncident(testIncident(1, store.SeverityWarning))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("opening messages = %d, want 1", got)
	}

	resolvedAt := clk.Now().Add(time.Minute)
	incident.ResolvedAt = &resolvedAt
	st.updateIncident(incident)
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeResolved}})
	if got := len(channel.messages()); got != 2 {
		t.Fatalf("messages = %d, want the resolution", got)
	}
	page := st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PageResolved {
		t.Fatalf("page = %+v, want resolved", page)
	}

	// A page that never announced anything produces no resolution.
	st2 := newFakeStore()
	channel2 := &fakeChannel{kind: KindTelegram}
	clk2 := &testClock{now: time.Date(2026, 10, 18, 14, 32, 0, 0, time.UTC)}
	notifier2 := newNotifierForStore(t, st2, clk2, channel2)
	st2.addSilence(store.Silence{
		Kind: store.SilenceMaintenanceWindow, Rule: gateStr("route.errors"), Subject: gateStr("shop@docker"),
		Reason: "maintenance", StartsAt: clk2.Now().Add(-time.Minute), EndsAt: clk2.Now().Add(time.Hour),
	})
	unannounced := st2.addIncident(testIncident(1, store.SeverityWarning))
	notifier2.IncidentsChanged(t.Context(), []rules.Change{{Incident: unannounced, Kind: rules.ChangeOpened}})
	unannounced.ResolvedAt = &resolvedAt
	st2.updateIncident(unannounced)
	notifier2.IncidentsChanged(t.Context(), []rules.Change{{Incident: unannounced, Kind: rules.ChangeResolved}})
	if got := len(channel2.messages()); got != 0 {
		t.Fatalf("messages for an unannounced incident = %d, want none", got)
	}
	page = st2.pageFor(unannounced.ID, KindTelegram, "")
	if page == nil || page.Status != store.PageCancelled || page.Reason != "resolved before announced" {
		t.Fatalf("unannounced page = %+v, want cancelled", page)
	}
}

func TestNotifierRestartResumesPendingPage(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	silence := st.addSilence(store.Silence{
		Kind: store.SilenceOneShotMute, Rule: gateStr("route.errors"), Subject: gateStr("shop@docker"),
		Reason: "maintenance", StartsAt: clk.Now().Add(-time.Minute), EndsAt: clk.Now().Add(5 * time.Minute),
	})
	incident := st.addIncident(testIncident(1, store.SeverityCritical))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})

	// Simulate a restart after the silence is removed.
	st.removeSilence(silence.ID)
	clk.now = clk.now.Add(5*time.Minute + time.Second)
	restartedChannel := &fakeChannel{kind: KindTelegram}
	restarted := newNotifierForStore(t, st, clk, restartedChannel)
	runNotifierOnce(t, restarted)
	if got := len(restartedChannel.messages()); got != 1 {
		t.Fatalf("messages after restart = %d, want the pending page", got)
	}
}

func TestNotifierRetriesFailedSend(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram, fails: 1}
	notifier, st, clk := newTestNotifier(t, channel)
	incident := st.addIncident(testIncident(1, store.SeverityCritical))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want the first attempt to fail", got)
	}
	page := st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PagePending || page.Attempts != 1 {
		t.Fatalf("page = %+v, want pending after one attempt", page)
	}

	clk.now = clk.now.Add(5 * time.Second)
	notifier.workMu.Lock()
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want no retry before ten seconds", got)
	}
	clk.now = clk.now.Add(5 * time.Second)
	notifier.workMu.Lock()
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want the retry", got)
	}
	page = st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PageAnnounced || page.Attempts != 2 {
		t.Fatalf("page after retry = %+v, want announced after two attempts", page)
	}
}

func TestNotifierAppAlertsDisabled(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	appID := int64(7)
	app := &discovery.App{ID: appID, Name: "shop", Alerts: false}
	notifier.model = fakeModel{model: &discovery.Model{Apps: map[string]*discovery.App{"shop": app}}}
	incident := testIncident(1, store.SeverityCritical)
	incident.AppID = &appID
	st.addIncident(incident)
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want none for an app with alerts disabled", got)
	}
	page := st.pageFor(incident.ID, KindTelegram, "")
	if page == nil || page.Status != store.PagePending || page.Reason != "app alerts disabled" {
		t.Fatalf("page = %+v, want pending with the alerts reason", page)
	}

	app.Alerts = true
	clk.now = clk.now.Add(time.Minute)
	runNotifierOnce(t, notifier)
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after enabling alerts = %d, want the held opening page", got)
	}
}

func TestNotifierAppAlertsDisabledDropsResolution(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, _ := newTestNotifier(t, channel)
	appID := int64(7)
	app := &discovery.App{ID: appID, Name: "shop", Alerts: true}
	notifier.model = fakeModel{model: &discovery.Model{Apps: map[string]*discovery.App{"shop": app}}}
	incident := testIncident(1, store.SeverityCritical)
	incident.AppID = &appID
	incident = st.addIncident(incident)
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("opening messages = %d, want 1", got)
	}

	app.Alerts = false
	resolvedAt := incident.OpenedAt.Add(time.Hour)
	resolved := incident
	resolved.ResolvedAt = &resolvedAt
	st.updateIncident(resolved)
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: resolved, Kind: rules.ChangeResolved}})
	app.Alerts = true
	runNotifierOnce(t, notifier)
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want the stale resolution dropped", got)
	}
	if page := st.pageFor(incident.ID, KindTelegram, ""); page == nil || page.Status != store.PageCancelled {
		t.Fatalf("page = %+v, want cancelled", page)
	}
}

func TestNotifierCancelsPageRefusedByRecipient(t *testing.T) {
	channel := &scriptedChannel{
		kind: KindTelegram, chatIDs: []string{"101", "202"},
		outcomes: map[string]error{"202": &refusedError{err: errors.New("the server answered 403 Forbidden")}},
	}
	notifier, st, clk := newTestNotifier(t, channel)
	incident := st.addIncident(testIncident(1, store.SeverityCritical))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})

	page := st.pageFor(incident.ID, KindTelegram, "202")
	if page == nil || page.Status != store.PageCancelled || !strings.HasPrefix(page.Reason, "recipient refused") {
		t.Fatalf("page 202 = %+v, want cancelled as refused", page)
	}
	if page := st.pageFor(incident.ID, KindTelegram, "101"); page == nil || page.Status != store.PageAnnounced {
		t.Fatalf("page 101 = %+v, want announced", page)
	}
	clk.now = clk.now.Add(time.Hour)
	runNotifierOnce(t, notifier)
	if got := channel.callsFor("202"); got != 1 {
		t.Fatalf("chat 202 calls = %d, want no retry after a refusal", got)
	}
}

func TestNotifierEscalationSendsAgain(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	incident := st.addIncident(testIncident(1, store.SeverityWarning))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("opening messages = %d, want 1", got)
	}
	clk.now = clk.now.Add(time.Minute)
	incident.Severity = store.SeverityCritical
	st.updateIncident(incident)
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeEscalated}})
	messages := channel.messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the escalation", len(messages))
	}
	if !strings.Contains(messages[1].Text, "CRITICAL") && !strings.Contains(messages[1].Text, "🔴") {
		t.Fatalf("escalation message = %q, want critical", messages[1].Text)
	}
	rows := st.byChannel(KindTelegram)
	if len(rows) != 2 || rows[1].Kind != store.DeliveryEscalated {
		t.Fatalf("deliveries = %+v, want an escalated row", rows)
	}
}

func TestNotifierRateLimitReschedules(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	notifier.limiters[KindTelegram].limit = 1
	first := st.addIncident(testIncident(1, store.SeverityCritical))
	second := st.addIncident(testIncident(2, store.SeverityCritical))
	second.Subject = "other@docker"
	st.updateIncident(second)

	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: first, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want the first", got)
	}
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: second, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want the second rate limited", got)
	}
	clk.now = clk.now.Add(2 * time.Hour)
	notifier.workMu.Lock()
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()
	if got := len(channel.messages()); got != 2 {
		t.Fatalf("messages after the window = %d, want the second page", got)
	}
}

func TestNotifierRecipientIsolation(t *testing.T) {
	channel := &scriptedChannel{
		kind: KindTelegram, chatIDs: []string{"101", "202"},
		outcomes: map[string]error{"202": errors.New("bot was kicked")},
	}
	notifier, st, clk := newTestNotifier(t, channel)
	incident := st.addIncident(testIncident(1, store.SeverityCritical))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := channel.callsFor("101"); got != 1 {
		t.Fatalf("chat 101 calls = %d, want 1", got)
	}
	if got := channel.callsFor("202"); got != 1 {
		t.Fatalf("chat 202 calls = %d, want 1", got)
	}
	if page := st.pageFor(incident.ID, KindTelegram, "101"); page == nil || page.Status != store.PageAnnounced {
		t.Fatalf("page 101 = %+v, want announced", page)
	}
	if page := st.pageFor(incident.ID, KindTelegram, "202"); page == nil || page.Status != store.PagePending {
		t.Fatalf("page 202 = %+v, want pending", page)
	}

	channel.setOutcome("202", nil)
	clk.now = clk.now.Add(10 * time.Second)
	notifier.workMu.Lock()
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()
	if got := channel.callsFor("101"); got != 1 {
		t.Fatalf("chat 101 calls after retry = %d, want no duplicate", got)
	}
	if got := channel.callsFor("202"); got != 2 {
		t.Fatalf("chat 202 calls after retry = %d, want 2", got)
	}
}

func TestNotifierQuietHoursHoldsWarning(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	notifier.gate.QuietFrom, notifier.gate.QuietTo, notifier.gate.QuietZone = &from, &to, time.UTC
	clk.now = time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC)
	incident := st.addIncident(testIncident(1, store.SeverityWarning))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	if got := len(channel.messages()); got != 0 {
		t.Fatalf("messages = %d, want none during quiet hours", got)
	}
	clk.now = time.Date(2026, 10, 19, 7, 1, 0, 0, time.UTC)
	notifier.workMu.Lock()
	notifier.deliverDue(t.Context())
	notifier.workMu.Unlock()
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after the window = %d, want the page", got)
	}
}

func TestNotifierDigestDoesNotResetStandDown(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, st, clk := newTestNotifier(t, channel)
	notifier.cfg.Notify.DigestChannels = []string{"telegram"}
	before := clk.Now().Add(-5 * time.Hour)
	notifier.mu.Lock()
	notifier.lastPageAt = before
	notifier.mu.Unlock()
	notifier.SendDigest(t.Context(), "daily digest")
	notifier.mu.Lock()
	got := notifier.lastPageAt
	notifier.mu.Unlock()
	if !got.Equal(before) {
		t.Fatalf("lastPageAt = %v after a digest, want it unchanged (%v)", got, before)
	}
	incident := st.addIncident(testIncident(1, store.SeverityCritical))
	notifier.IncidentsChanged(t.Context(), []rules.Change{{Incident: incident, Kind: rules.ChangeOpened}})
	notifier.mu.Lock()
	got = notifier.lastPageAt
	notifier.mu.Unlock()
	if !got.Equal(clk.Now()) {
		t.Fatalf("lastPageAt = %v after a page, want %v", got, clk.Now())
	}
}

func TestMaybeStandDownSendsOnce(t *testing.T) {
	channel := &fakeChannel{kind: KindTelegram}
	notifier, _, clk := newTestNotifier(t, channel)
	notifier.cfg.Notify.Silent.StandDownAfter = time.Hour
	notifier.cfg.Notify.DigestChannels = []string{"telegram"}
	notifier.mu.Lock()
	notifier.lastPageAt = clk.Now().Add(-2 * time.Hour)
	notifier.mu.Unlock()

	notifier.maybeStandDown(t.Context())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages = %d, want one all-quiet notice", got)
	}
	notifier.maybeStandDown(t.Context())
	if got := len(channel.messages()); got != 1 {
		t.Fatalf("messages after second call = %d, want still 1", got)
	}
}

func TestQuietSpan(t *testing.T) {
	for window, want := range map[time.Duration]string{
		24 * time.Hour: "24 h", 90 * time.Minute: "90 min", 30 * time.Minute: "30 min", time.Hour: "1 h",
	} {
		if got := quietSpan(window); got != want {
			t.Errorf("quietSpan(%v) = %q, want %q", window, got, want)
		}
	}
}

func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	l := &limiter{limit: 1000, window: time.Hour}
	now := time.Now()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if l.allow(now) {
					l.undo()
				}
			}
		}()
	}
	wg.Wait()
}

func TestFailInterruptedClosesPendingDeliveries(t *testing.T) {
	notifier, st, _ := newTestNotifier(t, &fakeChannel{kind: KindTelegram})
	ctx := t.Context()
	id := int64(1)
	pending, _ := st.AddDelivery(ctx, store.Delivery{IncidentID: &id, Kind: store.DeliveryOpened, Channel: "telegram", Status: "pending"})
	sent, _ := st.AddDelivery(ctx, store.Delivery{IncidentID: &id, Kind: store.DeliveryOpened, Channel: "telegram", Status: "sent"})
	if err := notifier.FailInterrupted(ctx); err != nil {
		t.Fatalf("FailInterrupted: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if got := st.deliveries[pending]; got.Status != "failed" || got.Error == nil {
		t.Fatalf("pending delivery = %+v, want failed with a reason", got)
	}
	if got := st.deliveries[sent]; got.Status != "sent" {
		t.Fatalf("sent delivery = %+v, want it untouched", got)
	}
}
