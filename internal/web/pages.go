package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// pageData is the common data of every page.
type pageData struct {
	Title    string
	HostName string
	Now      time.Time
	Version  string
	Sources  []sourceView
	Nav      string
	Location *time.Location
}

// appPageData is the data of the app page.
type appPageData struct {
	pageData
	App        *appDetail
	Range      rangeView
	Paths      []pathView
	Events     []eventView
	Containers []containerView
}

// appDetail is the app header.
type appDetail struct {
	Name       string
	Status     string
	Domains    []domainStatus
	Containers []containerView
}

// domainStatus is one domain with its certificate status.
type domainStatus struct {
	Name   string
	Status string
}

// containerView is one container row.
type containerView struct {
	Slot     string
	Name     string
	Image    string
	State    string
	Health   string
	Uptime   time.Duration
	Restarts int
	CPU      float64
	Memory   int64
	ID       string
}

// pathView is one row of the paths table.
type pathView struct {
	Method   string
	Pattern  string
	Requests int64
	S4xx     float64
	S5xx     float64
	P50      float64
	P95      float64
}

// rangeView is the selected time range.
type rangeView struct {
	From   time.Time
	To     time.Time
	Preset string
}

// incidentPageData is the data of the incident page.
type incidentPageData struct {
	pageData
	Incident   *store.Incident
	App        string
	Duration   time.Duration
	Details    map[string]any
	Deliveries []store.Delivery
	ChartKey   string
	ChartFrom  time.Time
	ChartTo    time.Time
	Link       string
}

// aboutPageData is the data of the about page.
type aboutPageData struct {
	pageData
	Uptime   time.Duration
	Settings []setting
	Notify   notifyView
	Sources  []sourceStateView
	Tables   []store.TableSize
}

// notifyView is the read-only notification configuration.
type notifyView struct {
	// Channels are the channel states in a fixed order.
	Channels []channelView
	// Warning and Critical are the routing lists.
	Warning, Critical string
	// Digest describes the daily digest.
	Digest string
	// Proxy describes the outbound proxy and its scope.
	Proxy string
}

// channelView is one notification channel on the About page.
type channelView struct {
	// Name is the channel name.
	Name string
	// Configured reports whether the channel has its settings.
	Configured bool
	// Route is proxy or direct when it is configured.
	Route string
}

// setting is one configuration value with secrets masked.
type setting struct {
	Name  string
	Value string
}

// sourceStateView is one source with its position.
type sourceStateView struct {
	Name      string
	Status    string
	LastError string
	Position  string
	UpdatedAt time.Time
}

// overviewPage renders the overview.
func (s *Server) overviewPage(w http.ResponseWriter, request *http.Request) {
	data, err := s.loadOverview(request.Context())
	if err != nil {
		s.logger.Error("overview page", "error", err)
		http.Error(w, "cannot read the overview", http.StatusInternalServerError)
		return
	}
	s.render(w, "overview.html", map[string]any{
		"Page": pageData{Title: "Overview", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: data.Sources, Nav: "overview", Location: s.cfg.Location},
		"Overview": data,
	})
}

// appPage renders one app.
func (s *Server) appPage(w http.ResponseWriter, request *http.Request) {
	name := request.PathValue("app")
	model := s.model.Model()
	app, ok := model.Apps[name]
	if !ok {
		http.NotFound(w, request)
		return
	}
	timeRange := s.rangeFromQuery(request)
	now := s.clock.Now()
	certs, err := s.certViews(request.Context(), now)
	if err != nil {
		http.Error(w, "cannot read the certificates", http.StatusInternalServerError)
		return
	}
	certStatus := make(map[string]string, len(certs))
	for _, cert := range certs {
		certStatus[cert.Domain] = cert.Status
	}
	detail := &appDetail{Name: app.Name, Status: appStatus(mustOpenMap(request, s), app)}
	for _, route := range app.Routes {
		for _, domain := range route.Domains {
			detail.Domains = append(detail.Domains, domainStatus{Name: domain, Status: certStatus[domain]})
		}
	}
	slotSamples, err := s.store.SlotSamples(request.Context(), now.Add(-10*time.Minute))
	if err != nil {
		http.Error(w, "cannot read the container samples", http.StatusInternalServerError)
		return
	}
	crashes, err := s.store.Events(request.Context(), "crashed", now.Add(-24*time.Hour), now)
	if err != nil {
		http.Error(w, "cannot read the crash events", http.StatusInternalServerError)
		return
	}
	for _, slot := range app.Slots {
		view := containerView{Slot: slot.Key}
		if slot.Container != nil {
			view.ID = slot.Container.ID
			view.Name = slot.Container.Name
			view.Image = slot.Container.ImageRef
			view.State = slot.Container.State
			view.Health = slot.Container.Health
			if !slot.Container.StartedAt.IsZero() {
				view.Uptime = now.Sub(slot.Container.StartedAt)
			}
		}
		for _, event := range crashes {
			if event.SlotID != nil && *event.SlotID == slot.ID {
				view.Restarts++
			}
		}
		if sample, ok := slotSamples[slot.ID]; ok {
			view.CPU = float64(sample.CPUMillicores)
			view.Memory = sample.MemBytes
		}
		detail.Containers = append(detail.Containers, view)
	}
	paths, err := s.pathsFor(request, app)
	if err != nil {
		http.Error(w, "cannot read the paths", http.StatusInternalServerError)
		return
	}
	events, err := s.eventsForApp(request, app.ID, timeRange)
	if err != nil {
		http.Error(w, "cannot read the events", http.StatusInternalServerError)
		return
	}
	s.render(w, "app.html", map[string]any{
		"Page": pageData{Title: app.Name, HostName: s.cfg.HostName, Now: now, Version: s.version,
			Sources: s.sourceViews(request), Nav: "overview", Location: s.cfg.Location},
		"App":          detail,
		"Data":         appPageData{App: detail, Range: timeRange, Paths: paths, Events: events, Containers: detail.Containers},
		"RangePresets": rangePresets(),
	})
}

// mustOpenMap returns the open incidents, or an empty map.
func mustOpenMap(request *http.Request, s *Server) map[store.IncidentKey]store.Incident {
	open, err := s.store.OpenIncidents(request.Context())
	if err != nil {
		return map[store.IncidentKey]store.Incident{}
	}
	return open
}

// pathsFor returns the path rows of an app.
func (s *Server) pathsFor(request *http.Request, app *discovery.App) ([]pathView, error) {
	timeRange := s.rangeFromQuery(request)
	var routeIDs []int64
	for _, route := range app.Routes {
		routeIDs = append(routeIDs, route.ID)
	}
	stats, err := s.store.RoutePathStats(request.Context(), routeIDs, timeRange.From, timeRange.To)
	if err != nil {
		return nil, err
	}
	var views []pathView
	for _, stat := range stats {
		p50, _ := stat.Latency.Percentile(50)
		p95, _ := stat.Latency.Percentile(95)
		view := pathView{Method: stat.Method, Pattern: stat.Pattern, Requests: stat.Requests, P50: p50, P95: p95}
		if stat.Requests > 0 {
			view.S4xx = 100 * float64(stat.S4xx) / float64(stat.Requests)
			view.S5xx = 100 * float64(stat.S5xx) / float64(stat.Requests)
		}
		views = append(views, view)
	}
	return views, nil
}

// hostPage renders the host page.
func (s *Server) hostPage(w http.ResponseWriter, request *http.Request) {
	now := s.clock.Now()
	host, err := s.hostView(request.Context(), now)
	if err != nil {
		http.Error(w, "cannot read the host", http.StatusInternalServerError)
		return
	}
	units, err := s.unitViews(request.Context())
	if err != nil {
		http.Error(w, "cannot read the units", http.StatusInternalServerError)
		return
	}
	certs, err := s.certViews(request.Context(), now)
	if err != nil {
		http.Error(w, "cannot read the certificates", http.StatusInternalServerError)
		return
	}
	s.render(w, "host.html", map[string]any{
		"Page": pageData{Title: "Host", HostName: s.cfg.HostName, Now: now, Version: s.version,
			Sources: s.sourceViews(request), Nav: "host", Location: s.cfg.Location},
		"Host": host, "Units": units, "Certs": certs, "Range": s.rangeFromQuery(request),
		"RangePresets": rangePresets(),
	})
}

// patternsPage renders the patterns page.
func (s *Server) patternsPage(w http.ResponseWriter, request *http.Request) {
	model := s.model.Model()
	var subjects []string
	for _, name := range sortedKeys(model.Apps) {
		subjects = append(subjects, "app:"+name)
		for _, route := range model.Apps[name].Routes {
			subjects = append(subjects, "route:"+route.Name)
		}
		for _, slot := range model.Apps[name].Slots {
			subjects = append(subjects, "slot:"+name+" / "+slot.Key)
		}
	}
	subjects = append(subjects, "host:load1")
	weeks := parseInt(request.URL.Query().Get("weeks"), 4)
	s.render(w, "patterns.html", map[string]any{
		"Page": pageData{Title: "Patterns", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: s.sourceViews(request), Nav: "patterns", Location: s.cfg.Location},
		"Subjects": subjects, "Weeks": weeks, "Key": request.URL.Query().Get("key"),
	})
}

// eventsPage renders the events page.
func (s *Server) eventsPage(w http.ResponseWriter, request *http.Request) {
	timeRange := s.rangeFromQuery(request)
	query := request.URL.Query()
	events, err := s.eventsForApp(request, 0, timeRange)
	if err != nil {
		http.Error(w, "cannot read the events", http.StatusInternalServerError)
		return
	}
	filters := map[string]string{"app": query.Get("app"), "kind": query.Get("kind"), "severity": query.Get("severity")}
	var filtered []eventView
	for _, event := range events {
		if filters["app"] != "" && event.App != filters["app"] {
			continue
		}
		if filters["kind"] != "" && event.Kind != filters["kind"] {
			continue
		}
		if filters["severity"] != "" && event.Severity != filters["severity"] {
			continue
		}
		filtered = append(filtered, event)
	}
	model := s.model.Model()
	apps := append([]string(nil), sortedKeys(model.Apps)...)
	kinds := []string{"deploy", "crashed", "started", "stopped", "health"}
	s.render(w, "events.html", map[string]any{
		"Page": pageData{Title: "Events", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: s.sourceViews(request), Nav: "events", Location: s.cfg.Location},
		"Range": timeRange, "Events": filtered, "Filters": filters, "Apps": apps, "Kinds": kinds,
		"RangePresets": rangePresets(),
	})
}

// incidentPage renders one incident.
func (s *Server) incidentPage(w http.ResponseWriter, request *http.Request) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "the incident ID must be a number")
		return
	}
	incident, err := s.store.IncidentByID(request.Context(), id)
	if err != nil {
		http.Error(w, "cannot read the incident", http.StatusInternalServerError)
		return
	}
	if incident == nil {
		http.NotFound(w, request)
		return
	}
	deliveries, err := s.store.Deliveries(request.Context(), id)
	if err != nil {
		http.Error(w, "cannot read the deliveries", http.StatusInternalServerError)
		return
	}
	duration := s.clock.Now().Sub(incident.OpenedAt)
	if incident.ResolvedAt != nil {
		duration = incident.ResolvedAt.Sub(incident.OpenedAt)
	}
	details := map[string]any{}
	if len(incident.Details) > 0 {
		_ = decodeJSON(incident.Details, &details)
	}
	from := incident.OpenedAt.Add(-30 * time.Minute)
	to := incident.OpenedAt.Add(30 * time.Minute)
	key := ""
	link := "/"
	if incident.RouteID != nil && s.model.Model() != nil {
		for _, route := range s.model.Model().Routes {
			if route.ID == *incident.RouteID {
				key = "route:" + route.Name + ":p95"
				if route.App != nil {
					link = "/apps/" + route.App.Name
				}
			}
		}
	}
	s.render(w, "incident.html", map[string]any{
		"Page": pageData{Title: "Incident", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: s.sourceViews(request), Nav: "", Location: s.cfg.Location},
		"Data": incidentPageData{Incident: incident, App: incidentApp(*incident), Duration: duration,
			Details: details, Deliveries: deliveries, ChartKey: key, ChartFrom: from, ChartTo: to, Link: link},
	})
}

// aboutPage renders the about page.
func (s *Server) aboutPage(w http.ResponseWriter, request *http.Request) {
	sizes, err := s.store.TableSizes(request.Context())
	if err != nil {
		http.Error(w, "cannot read the table sizes", http.StatusInternalServerError)
		return
	}
	states, err := s.store.SourceStates(request.Context())
	if err != nil {
		http.Error(w, "cannot read the source states", http.StatusInternalServerError)
		return
	}
	var sources []sourceStateView
	for _, state := range states {
		view := sourceStateView{Name: state.Source, Status: state.Status, UpdatedAt: state.UpdatedAt}
		if state.LastError != nil {
			view.LastError = *state.LastError
		}
		if len(state.Position) > 0 {
			view.Position = string(state.Position)
		}
		sources = append(sources, view)
	}
	s.render(w, "about.html", map[string]any{
		"Page": pageData{Title: "About", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: s.sourceViews(request), Nav: "", Location: s.cfg.Location},
		"Data": aboutPageData{Uptime: s.clock.Now().Sub(s.startedAt), Settings: s.settings(),
			Notify: s.notificationView(), Sources: sources, Tables: sizes},
	})
}

// notificationView describes the notification configuration without its
// secrets.
func (s *Server) notificationView() notifyView {
	view := notifyView{
		Warning:  strings.Join(s.cfg.Notify.WarningChannels, ", "),
		Critical: "all configured",
		Digest:   "disabled",
	}
	if s.cfg.Notify.CriticalChannels != nil {
		view.Critical = strings.Join(s.cfg.Notify.CriticalChannels, ", ")
	}
	if s.cfg.Notify.DigestAt != nil {
		view.Digest = fmt.Sprintf("%02d:%02d to %s", s.cfg.Notify.DigestAt.Hour,
			s.cfg.Notify.DigestAt.Minute, strings.Join(s.cfg.Notify.DigestChannels, ", "))
	}
	switch {
	case s.cfg.Notify.OutboundProxy == "":
		view.Proxy = "none"
	case len(s.cfg.Notify.ProxyFor) == 0:
		view.Proxy = maskURL(s.cfg.Notify.OutboundProxy) + " (all requests)"
	default:
		view.Proxy = maskURL(s.cfg.Notify.OutboundProxy) + " (" + strings.Join(s.cfg.Notify.ProxyFor, ", ") + ")"
	}
	channels := []struct {
		name       string
		target     string
		configured bool
	}{
		{"telegram", config.ProxyTargetTelegram, s.cfg.Notify.Telegram.Token != "" && len(s.cfg.Notify.Telegram.ChatIDs) > 0},
		{"sms", config.ProxyTargetSMS, s.cfg.Notify.SMS.Provider != "" && len(s.cfg.Notify.SMS.To) > 0},
		{"webhook", config.ProxyTargetWebhook, s.cfg.Notify.Webhook.URL != ""},
	}
	for _, channel := range channels {
		entry := channelView{Name: channel.name, Configured: channel.configured, Route: "direct"}
		if s.cfg.UsesProxy(channel.target) {
			entry.Route = "proxy"
		}
		view.Channels = append(view.Channels, entry)
	}
	return view
}

// rangePresets returns the documented time range presets.
func rangePresets() []struct{ Name, Value string } {
	return []struct{ Name, Value string }{
		{"1 h", "1h"}, {"6 h", "6h"}, {"24 h", "24h"}, {"7 d", "7d"}, {"14 d", "14d"}, {"90 d", "90d"},
	}
}

// rangeFromQuery returns the selected range, defaulting to the last six hours.
func (s *Server) rangeFromQuery(request *http.Request) rangeView {
	now := s.clock.Now()
	query := request.URL.Query()
	from, okFrom := parseTime(query.Get("from"))
	to, okTo := parseTime(query.Get("to"))
	if okFrom && okTo && to.After(from) {
		return rangeView{From: from, To: to, Preset: "custom"}
	}
	preset := query.Get("range")
	length := 6 * time.Hour
	switch preset {
	case "1h":
		length = time.Hour
	case "6h", "":
		length = 6 * time.Hour
		preset = "6h"
	case "24h":
		length = 24 * time.Hour
	case "7d":
		length = 7 * 24 * time.Hour
	case "14d":
		length = 14 * 24 * time.Hour
	case "90d":
		length = 90 * 24 * time.Hour
	}
	return rangeView{From: now.Add(-length), To: now, Preset: preset}
}

// sourceViews returns the source dots.
func (s *Server) sourceViews(request *http.Request) []sourceView {
	states, err := s.store.SourceStates(request.Context())
	if err != nil {
		return nil
	}
	var views []sourceView
	for _, state := range states {
		view := sourceView{Name: state.Source, Status: state.Status}
		if state.LastError != nil {
			view.LastError = *state.LastError
		}
		views = append(views, view)
	}
	views = append(views, sourceView{Name: "database", Status: store.StatusOK})
	return views
}

// eventsForApp returns the events of an app, or of every app.
func (s *Server) eventsForApp(request *http.Request, appID int64, timeRange rangeView) ([]eventView, error) {
	events, err := s.store.AllEvents(request.Context(), timeRange.From, timeRange.To)
	if err != nil {
		return nil, err
	}
	model := s.model.Model()
	var views []eventView
	for _, event := range events {
		if appID != 0 && (event.AppID == nil || *event.AppID != appID) {
			continue
		}
		view := eventView{TS: event.TS, Kind: event.Kind, Severity: event.Severity, Title: event.Title, Subject: event.Subject}
		if event.AppID != nil {
			view.App = s.appName(model, *event.AppID)
		}
		views = append(views, view)
	}
	return views, nil
}

// sortedKeys returns the keys of an app map in order.
func sortedKeys(values map[string]*discovery.App) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// decodeJSON decodes a JSON value.
func decodeJSON(data []byte, value any) error {
	return json.Unmarshal(data, value)
}

// render writes one page. A template error becomes a 500 instead of a
// half-written page.
func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	var page bytes.Buffer
	if err := s.templates.ExecuteTemplate(&page, name, data); err != nil {
		s.logger.Error("render page", "page", name, "error", err)
		http.Error(w, "cannot render the page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page.Bytes())
}

// settings returns the effective configuration with secrets masked.
func (s *Server) settings() []setting {
	cfg := s.cfg
	mask := func(value string) string {
		if value == "" {
			return ""
		}
		return "••••••"
	}
	settings := []setting{
		{"RAEDAR_DATABASE_URL", maskURL(cfg.DatabaseURL)},
		{"RAEDAR_LISTEN", cfg.Listen},
		{"RAEDAR_PUBLIC_URL", cfg.PublicURL},
		{"RAEDAR_HOST_NAME", cfg.HostName},
		{"RAEDAR_TZ", cfg.Location.String()},
		{"RAEDAR_LOG_LEVEL", cfg.LogLevel.String()},
		{"RAEDAR_DOCKER_HOST", cfg.Docker.Host},
		{"RAEDAR_ACCESSLOG_PATH", cfg.Docker.AccessLogPath},
		{"RAEDAR_HOST_ROOT", cfg.Host.Root},
		{"RAEDAR_SYSTEMD", string(cfg.Host.Systemd)},
		{"RAEDAR_TELEGRAM_TOKEN", mask(cfg.Notify.Telegram.Token)},
		{"RAEDAR_KAVENEGAR_API_KEY", mask(cfg.Notify.SMS.KavenegarAPIKey)},
		{"RAEDAR_WEBHOOK_SECRET", mask(cfg.Notify.Webhook.Secret)},
		{"RAEDAR_HEARTBEAT_RECEIVE_TOKEN", mask(cfg.Heartbeat.ReceiveToken)},
		{"RAEDAR_RETENTION_MINUTE", cfg.Retention.Minute.String()},
		{"RAEDAR_RETENTION_HOUR", cfg.Retention.Hour.String()},
	}
	return settings
}

// maskURL hides the password of a URL.
func maskURL(value string) string {
	if value == "" {
		return ""
	}
	if index := strings.Index(value, "://"); index >= 0 {
		rest := value[index+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			credentials := rest[:at]
			if colon := strings.Index(credentials, ":"); colon >= 0 {
				return value[:index+3] + credentials[:colon+1] + "••••••" + rest[at:]
			}
		}
	}
	return value
}
