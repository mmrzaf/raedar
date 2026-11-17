package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// defaultCompared is how many apps the compare page selects when the request
// names none: the busiest ones.
const defaultCompared = 4

// requestTotals are the request counters of one subject over a range.
// Everything here is a counter or a histogram, so totals of several subjects
// are plain sums and merges, never averages.
type requestTotals struct {
	Requests int64
	S4xx     int64
	S5xx     int64
	Bytes    int64
	Latency  store.Histogram
}

// add folds other into t.
func (t *requestTotals) add(other requestTotals) {
	t.Requests += other.Requests
	t.S4xx += other.S4xx
	t.S5xx += other.S5xx
	t.Bytes += other.Bytes
	t.Latency.Merge(&other.Latency)
}

// errorRate is the share of requests answered with 5xx, in percent. It is
// derived from the totals, which is the only correct way to combine rates;
// known is false without requests.
func (t requestTotals) errorRate() (rate float64, known bool) {
	if t.Requests == 0 {
		return 0, false
	}
	return 100 * float64(t.S5xx) / float64(t.Requests), true
}

// compareRow is one app of the comparison table, or the combined row.
type compareRow struct {
	Name string
	// Requests and the following are the totals over the range.
	Requests int64
	S5xx     int64
	Bytes    int64
	// ErrorRate is the 5xx percentage; Known tells whether there were requests.
	ErrorRate float64
	RateKnown bool
	P95       latency
	// RequestShare and ErrorShare are the percentage of the combined requests
	// and 5xx responses; zero when the combined total is zero.
	RequestShare float64
	ErrorShare   float64
	// CPU and Memory are the average combined use of the app's containers;
	// ResourcesKnown is false when no container sample exists in the range.
	CPU            float64
	Memory         int64
	ResourcesKnown bool
}

// compareTotalsRow turns totals into a row.
func compareTotalsRow(name string, t requestTotals) compareRow {
	rate, known := t.errorRate()
	return compareRow{Name: name, Requests: t.Requests, S5xx: t.S5xx, Bytes: t.Bytes,
		ErrorRate: rate, RateKnown: known, P95: percentileOf(&t.Latency, 95)}
}

// withShares sets each row's share of the combined requests and errors.
func withShares(rows []compareRow, combined compareRow) {
	for i := range rows {
		if combined.Requests > 0 {
			rows[i].RequestShare = 100 * float64(rows[i].Requests) / float64(combined.Requests)
		}
		if combined.S5xx > 0 {
			rows[i].ErrorShare = 100 * float64(rows[i].S5xx) / float64(combined.S5xx)
		}
	}
}

// compareApp is one choice in the app picker.
type compareApp struct {
	Name     string
	Group    string
	Selected bool
}

// compareGroup is one logical group, a link that selects all its apps.
type compareGroup struct {
	Name string
	Apps string
}

// comparePageData is the data of the compare page.
type comparePageData struct {
	Apps     []compareApp
	Groups   []compareGroup
	Selected []string
	// Joined is the comma-separated selection, as the combined series subject.
	Joined   string
	Range    rangeView
	Rows     []compareRow
	Combined *compareRow
	Events   []eventView
}

// selectedApps returns the apps the request asks for: the apps parameter
// (comma-separated or repeated) and every app of the group parameter. Unknown
// names are dropped; without a valid selection the busiest apps are used.
func (s *Server) selectedApps(request *http.Request, model *discovery.Model) []string {
	query := request.URL.Query()
	chosen := make(map[string]bool)
	for _, value := range query["apps"] {
		for _, name := range splitApps(value) {
			if _, ok := model.Apps[name]; ok {
				chosen[name] = true
			}
		}
	}
	if group := query.Get("group"); group != "" {
		for name, app := range model.Apps {
			if app.Group == group {
				chosen[name] = true
			}
		}
	}
	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxAppsPerSeries {
		names = names[:maxAppsPerSeries]
	}
	if len(names) > 0 || query.Has("apps") || query.Has("group") {
		return names
	}
	return s.busiestApps(request.Context(), model)
}

// busiestApps returns up to defaultCompared apps with the most requests in the
// last hour, falling back to the first apps by name when there is no traffic.
func (s *Server) busiestApps(ctx context.Context, model *discovery.Model) []string {
	views, err := s.appViews(ctx, model, map[store.IncidentKey]store.Incident{}, s.clock.Now())
	if err != nil {
		views = nil
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].RequestsPerMinute > views[j].RequestsPerMinute })
	var names []string
	for _, view := range views {
		if len(names) == defaultCompared {
			break
		}
		if _, ok := model.Apps[view.Name]; ok && view.RequestsPerMinute > 0 {
			names = append(names, view.Name)
		}
	}
	if len(names) == 0 {
		for _, name := range sortedKeys(model.Apps) {
			if len(names) == defaultCompared {
				break
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// comparePage renders the comparison of several apps: a table of totals over
// the range and aligned charts of the combined and individual series.
func (s *Server) comparePage(w http.ResponseWriter, request *http.Request) {
	model := s.model.Model()
	now := s.clock.Now()
	page := pageData{Title: "Compare", HostName: s.cfg.HostName, Now: now, Version: s.version,
		Sources: s.sourceViews(request), Nav: "compare", Location: s.cfg.Location}
	if model == nil {
		s.render(w, "compare.html", map[string]any{"Page": page, "Data": comparePageData{}, "RangePresets": rangePresets()})
		return
	}
	selected := s.selectedApps(request, model)
	timeRange := s.rangeFromQuery(request)
	data := comparePageData{Selected: selected, Joined: strings.Join(selected, ","), Range: timeRange}
	picked := make(map[string]bool, len(selected))
	for _, name := range selected {
		picked[name] = true
	}
	groups := make(map[string][]string)
	for _, name := range sortedKeys(model.Apps) {
		app := model.Apps[name]
		data.Apps = append(data.Apps, compareApp{Name: name, Group: app.Group, Selected: picked[name]})
		if app.Group != "" {
			groups[app.Group] = append(groups[app.Group], name)
		}
	}
	for _, name := range sortedKeys(groups) {
		data.Groups = append(data.Groups, compareGroup{Name: name, Apps: strings.Join(groups[name], ",")})
	}
	if len(selected) > 0 {
		rows, combined, err := s.compareRows(request.Context(), model, selected, timeRange)
		if err != nil {
			s.logger.Error("compare totals", "error", err)
			http.Error(w, "cannot read the comparison", http.StatusInternalServerError)
			return
		}
		data.Rows, data.Combined = rows, combined
		for _, name := range selected {
			events, err := s.eventsForApp(request, model.Apps[name].ID, timeRange)
			if err != nil {
				http.Error(w, "cannot read the events", http.StatusInternalServerError)
				return
			}
			data.Events = append(data.Events, events...)
		}
		sort.Slice(data.Events, func(i, j int) bool { return data.Events[i].TS.After(data.Events[j].TS) })
	}
	s.render(w, "compare.html", map[string]any{"Page": page, "Data": data, "RangePresets": rangePresets()})
}

// compareRows reads the totals of every selected app and of all of them
// together, over one range. Each read is a single bin, so counters are exact
// sums and latency comes from the merged histogram.
func (s *Server) compareRows(ctx context.Context, model *discovery.Model, names []string, r rangeView) ([]compareRow, *compareRow, error) {
	window := store.Range{From: r.From, To: r.To, Points: 1}
	source, bin := store.Resolution(window, s.cfg.Retention.Minute, s.clock.Now())
	var (
		rows     []compareRow
		combined requestTotals
		cpu      float64
		mem      int64
		anyRes   bool
	)
	for _, name := range names {
		app := model.Apps[name]
		var routeIDs, slotIDs []int64
		for _, route := range app.Routes {
			if route.ID != 0 {
				routeIDs = append(routeIDs, route.ID)
			}
		}
		for _, slot := range app.Slots {
			if slot.ID != 0 {
				slotIDs = append(slotIDs, slot.ID)
			}
		}
		totals, err := s.requestTotals(ctx, routeIDs, window, source, bin)
		if err != nil {
			return nil, nil, err
		}
		row := compareTotalsRow(name, totals)
		resources, err := s.store.ResourceSeries(ctx, slotIDs, window, source, bin)
		if err != nil {
			return nil, nil, err
		}
		row.CPU, row.Memory, row.ResourcesKnown = averageResources(resources)
		if row.ResourcesKnown {
			cpu += row.CPU
			mem += row.Memory
			anyRes = true
		}
		combined.add(totals)
		rows = append(rows, row)
	}
	total := compareTotalsRow("Combined", combined)
	total.CPU, total.Memory, total.ResourcesKnown = cpu, mem, anyRes
	total.RequestShare, total.ErrorShare = 100, 100
	withShares(rows, total)
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].S5xx > rows[j].S5xx || rows[i].S5xx == rows[j].S5xx && rows[i].Requests > rows[j].Requests
	})
	return rows, &total, nil
}

// requestTotals sums the request counters of routes over a range.
func (s *Server) requestTotals(ctx context.Context, routeIDs []int64, window store.Range, source store.RangeSource, bin time.Duration) (requestTotals, error) {
	var totals requestTotals
	bins, err := s.store.RouteSeries(ctx, routeIDs, window, source, bin)
	if err != nil {
		return totals, err
	}
	for _, entry := range bins {
		totals.add(requestTotals{Requests: entry.Requests, S4xx: entry.S4xx, S5xx: entry.S5xx, Bytes: entry.Bytes, Latency: entry.Latency})
	}
	return totals, nil
}

// averageResources averages the combined resource bins over a range; known is
// false when there are none.
func averageResources(bins []store.ResourceBin) (cpu float64, memory int64, known bool) {
	if len(bins) == 0 {
		return 0, 0, false
	}
	var memorySum float64
	for _, entry := range bins {
		cpu += entry.CPU
		memorySum += entry.Memory
	}
	return cpu / float64(len(bins)), int64(memorySum / float64(len(bins))), true
}
