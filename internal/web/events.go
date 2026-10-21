package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// eventView is one event or incident in a list.
type eventView struct {
	TS       time.Time       `json:"ts"`
	Kind     store.EventKind `json:"kind"`
	Severity store.Severity  `json:"severity"`
	Title    string          `json:"title"`
	Subject  string          `json:"subject"`
	App      string          `json:"app"`
}

// eventsAPI returns the events and incidents of a range.
func (s *Server) eventsAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	timeRange, err := seriesRange(query)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	model := s.model.Model()
	var appID int64
	if name := query.Get("app"); name != "" {
		app, ok := model.Apps[name]
		if !ok {
			http.NotFound(w, request)
			return
		}
		appID = app.ID
	}
	kind := query.Get("kind")
	events, err := s.store.AllEvents(request.Context(), timeRange.From, timeRange.To)
	if err != nil {
		http.Error(w, "cannot read the events", http.StatusInternalServerError)
		return
	}
	type eventJSON struct {
		ID       int64           `json:"id"`
		TS       time.Time       `json:"ts"`
		Kind     store.EventKind `json:"kind"`
		Severity store.Severity  `json:"severity"`
		Subject  string          `json:"subject"`
		Title    string          `json:"title"`
		App      string          `json:"app"`
	}
	var items []eventJSON
	for _, event := range events {
		if appID != 0 && (event.AppID == nil || *event.AppID != appID) {
			continue
		}
		if kind != "" && string(event.Kind) != kind {
			continue
		}
		item := eventJSON{ID: event.ID, TS: event.TS, Kind: event.Kind, Severity: event.Severity,
			Subject: s.cfg.SubjectNames.Display(event.Subject), Title: event.Title}
		if event.AppID != nil {
			item.App = s.appName(model, *event.AppID)
		}
		items = append(items, item)
	}
	incidents, err := s.store.Incidents(request.Context(), timeRange.From, timeRange.To)
	if err != nil {
		http.Error(w, "cannot read the incidents", http.StatusInternalServerError)
		return
	}
	type incidentJSON struct {
		ID         int64          `json:"id"`
		Rule       string         `json:"rule"`
		Subject    string         `json:"subject"`
		Severity   store.Severity `json:"severity"`
		OpenedAt   time.Time      `json:"opened_at"`
		ResolvedAt *time.Time     `json:"resolved_at"`
		Summary    string         `json:"summary"`
		App        string         `json:"app"`
	}
	var incidentItems []incidentJSON
	for _, incident := range incidents {
		if appID != 0 && (incident.AppID == nil || *incident.AppID != appID) {
			continue
		}
		if kind != "" && kind != "incident" && kind != incident.Rule {
			continue
		}
		incidentItems = append(incidentItems, incidentJSON{ID: incident.ID, Rule: incident.Rule,
			Subject: s.cfg.SubjectNames.Display(incident.Subject), Severity: incident.Severity, OpenedAt: incident.OpenedAt,
			ResolvedAt: incident.ResolvedAt, Summary: incident.Summary, App: incidentApp(incident)})
	}
	writeJSON(w, map[string]any{"events": items, "incidents": incidentItems})
}

// eventKinds returns the kinds present in events, sorted, so the filter offers
// exactly what the range contains.
func eventKinds(events []eventView) []string {
	seen := make(map[string]bool)
	var kinds []string
	for _, event := range events {
		kind := string(event.Kind)
		if !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return kinds
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
		if filters["kind"] != "" && string(event.Kind) != filters["kind"] {
			continue
		}
		if filters["severity"] != "" && string(event.Severity) != filters["severity"] {
			continue
		}
		filtered = append(filtered, event)
	}
	model := s.model.Model()
	apps := append([]string(nil), sortedKeys(model.Apps)...)
	kinds := eventKinds(events)
	s.render(w, "events.html", map[string]any{
		"Page": pageData{Title: "Events", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: s.sourceViews(request), Nav: "events", Location: s.cfg.Location},
		"Range": timeRange, "Events": filtered, "Filters": filters, "Apps": apps, "Kinds": kinds,
		"RangePresets": rangePresets(),
	})
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
		view := eventView{TS: event.TS, Kind: event.Kind, Severity: event.Severity, Title: event.Title, Subject: s.cfg.SubjectNames.Display(event.Subject)}
		if event.AppID != nil {
			view.App = s.appName(model, *event.AppID)
		}
		views = append(views, view)
	}
	return views, nil
}
