package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// baselineAPI returns the baseline of a route for the range's hours.
func (s *Server) baselineAPI(w http.ResponseWriter, request *http.Request) {
	routeName := request.URL.Query().Get("route")
	if routeName == "" {
		badRequest(w, "route is required")
		return
	}
	model := s.model.Model()
	route, ok := model.Routes[routeName]
	if !ok {
		http.NotFound(w, request)
		return
	}
	timeRange, err := seriesRange(request.URL.Query())
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	baselines, err := s.store.Baselines(request.Context())
	if err != nil {
		http.Error(w, "cannot read the baselines", http.StatusInternalServerError)
		return
	}
	byHour := make(map[int]store.Baseline)
	for _, baseline := range baselines {
		if baseline.RouteID == route.ID {
			byHour[baseline.HourOfDay] = baseline
		}
	}
	type entry struct {
		Hour      int      `json:"hour"`
		ErrorRate *float64 `json:"error_rate"`
		P95MS     *float64 `json:"p95_ms"`
		DaysUsed  int      `json:"days_used"`
	}
	var entries []entry
	start := timeRange.From.In(s.cfg.Location).Truncate(time.Hour)
	for at := start; at.Before(timeRange.To); at = at.Add(time.Hour) {
		hour := at.In(s.cfg.Location).Hour()
		baseline := byHour[hour]
		entries = append(entries, entry{Hour: hour, ErrorRate: baseline.ErrorRate, P95MS: baseline.P95MS, DaysUsed: baseline.DaysUsed})
	}
	writeJSON(w, map[string]any{"route": routeName, "baselines": entries})
}

// pathsAPI returns the path statistics of an app or a route.
func (s *Server) pathsAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	timeRange, err := seriesRange(query)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if query.Get("app") == "" && query.Get("route") == "" {
		badRequest(w, "app or route is required")
		return
	}
	model := s.model.Model()
	var routeIDs []int64
	if routeName := query.Get("route"); routeName != "" {
		route, ok := model.Routes[routeName]
		if !ok {
			http.NotFound(w, request)
			return
		}
		routeIDs = []int64{route.ID}
	} else {
		app, ok := model.Apps[query.Get("app")]
		if !ok {
			http.NotFound(w, request)
			return
		}
		for _, route := range app.Routes {
			routeIDs = append(routeIDs, route.ID)
		}
	}
	stats, err := s.store.RoutePathStats(request.Context(), routeIDs, timeRange.From, timeRange.To)
	if err != nil {
		http.Error(w, "cannot read the paths", http.StatusInternalServerError)
		return
	}
	type entry struct {
		Method   string  `json:"method"`
		Pattern  string  `json:"pattern"`
		Requests int64   `json:"requests"`
		S4xx     int64   `json:"s4xx"`
		S5xx     int64   `json:"s5xx"`
		P50      latency `json:"p50"`
		P95      latency `json:"p95"`
	}
	entries := make([]entry, 0, len(stats))
	for _, stat := range stats {
		entries = append(entries, entry{Method: stat.Method, Pattern: stat.Pattern, Requests: stat.Requests,
			S4xx: stat.S4xx, S5xx: stat.S5xx, P50: percentileOf(&stat.Latency, 50), P95: percentileOf(&stat.Latency, 95)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].S5xx != entries[j].S5xx {
			return entries[i].S5xx > entries[j].S5xx
		}
		return entries[i].P95.MS > entries[j].P95.MS
	})
	writeJSON(w, map[string]any{"paths": entries})
}

// pathView is one row of the paths table.
type pathView struct {
	Method   string
	Pattern  string
	Requests int64
	S4xx     float64
	S5xx     float64
	P50      latency
	P95      latency
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
		view := pathView{Method: stat.Method, Pattern: stat.Pattern, Requests: stat.Requests,
			P50: percentileOf(&stat.Latency, 50), P95: percentileOf(&stat.Latency, 95)}
		if stat.Requests > 0 {
			view.S4xx = 100 * float64(stat.S4xx) / float64(stat.Requests)
			view.S5xx = 100 * float64(stat.S5xx) / float64(stat.Requests)
		}
		views = append(views, view)
	}
	return views, nil
}
