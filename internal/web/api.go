package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// sourceView is one source dot in the top bar.
type sourceView struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LastError string `json:"last_error"`
}

// incidentView is one open incident.
type incidentView struct {
	ID       int64         `json:"i_d"`
	Rule     string        `json:"rule"`
	Subject  string        `json:"subject"`
	App      string        `json:"app"`
	Severity string        `json:"severity"`
	Summary  string        `json:"summary"`
	OpenedAt time.Time     `json:"opened_at"`
	Duration time.Duration `json:"duration"`
}

// eventView is one event or incident in a list.
type eventView struct {
	TS       time.Time `json:"t_s"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Subject  string    `json:"subject"`
	App      string    `json:"app"`
}

// diskView is one filesystem.
type diskView struct {
	Name          string  `json:"name"`
	Usage         float64 `json:"usage"`
	Free          int64   `json:"free"`
	DaysUntilFull float64 `json:"days_until_full"`
	Growing       bool    `json:"growing"`
}

// dockerDiskView is Docker's disk usage.
type dockerDiskView struct {
	Images      int64 `json:"images"`
	Reclaimable int64 `json:"reclaimable"`
	BuildCache  int64 `json:"build_cache"`
	Volumes     int64 `json:"volumes"`
	Layers      int64 `json:"layers"`
}

// hostView is the host strip.
type hostView struct {
	CPU          float64         `json:"c_p_u"`
	Load1        float64         `json:"load1"`
	MemAvailable int64           `json:"mem_available"`
	MemTotal     int64           `json:"mem_total"`
	SwapUsed     int64           `json:"swap_used"`
	SwapTotal    int64           `json:"swap_total"`
	Disks        []diskView      `json:"disks"`
	Docker       *dockerDiskView `json:"docker"`
}

// appView is one row of the overview.
type appView struct {
	Name              string    `json:"name"`
	Domains           []string  `json:"domains"`
	Status            string    `json:"status"`
	RequestsPerMinute float64   `json:"requests_per_minute"`
	Sparkline         []float64 `json:"sparkline"`
	ErrorRate         float64   `json:"error_rate"`
	ErrorBaseline     float64   `json:"error_baseline"`
	P95               float64   `json:"p95"`
	P95Baseline       float64   `json:"p95_baseline"`
	CPU               float64   `json:"c_p_u"`
	Memory            int64     `json:"memory"`
	MemoryLimit       int64     `json:"memory_limit"`
	Running           int       `json:"running"`
	Total             int       `json:"total"`
	Restarts          int       `json:"restarts"`
	LastDeploy        time.Time `json:"last_deploy"`
}

// certView is one certificate check.
type certView struct {
	Domain    string     `json:"domain"`
	Status    string     `json:"status"`
	Issuer    string     `json:"issuer"`
	NotAfter  *time.Time `json:"not_after"`
	DaysLeft  float64    `json:"days_left"`
	CheckedAt time.Time  `json:"checked_at"`
	Error     string     `json:"error"`
	Attention bool       `json:"attention"`
}

// unitView is one systemd unit.
type unitView struct {
	Name        string     `json:"name"`
	ActiveState string     `json:"active_state"`
	SubState    string     `json:"sub_state"`
	Result      string     `json:"result"`
	ExitStatus  *int       `json:"exit_status"`
	Since       *time.Time `json:"since"`
}

// overviewData is everything the overview page and its API show.
type overviewData struct {
	HostName  string         `json:"host_name"`
	Now       time.Time      `json:"now"`
	Version   string         `json:"version"`
	Sources   []sourceView   `json:"sources"`
	Incidents []incidentView `json:"incidents"`
	Host      *hostView      `json:"host"`
	Apps      []appView      `json:"apps"`
	Certs     []certView     `json:"certs"`
	Units     []unitView     `json:"units"`
	Events    []eventView    `json:"events"`
}

// loadOverview gathers the overview data.
func (s *Server) loadOverview(ctx context.Context) (*overviewData, error) {
	now := s.clock.Now()
	model := s.model.Model()
	data := &overviewData{HostName: s.cfg.HostName, Now: now, Version: s.version}
	states, err := s.store.SourceStates(ctx)
	if err != nil {
		return nil, err
	}
	now2 := now
	for _, state := range states {
		view := sourceView{Name: state.Source, Status: state.Status}
		if state.LastError != nil {
			view.LastError = *state.LastError
		}
		data.Sources = append(data.Sources, view)
	}
	if model != nil {
		if model.Traefik != nil {
			data.Sources = append(data.Sources, sourceView{Name: "tls", Status: sourceStatus(states, "tlscheck")})
		}
	}
	data.Sources = append(data.Sources, sourceView{Name: "database", Status: store.StatusOK})
	open, err := s.store.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	for _, incident := range open {
		data.Incidents = append(data.Incidents, incidentView{
			ID: incident.ID, Rule: incident.Rule, Subject: incident.Subject,
			App: incidentApp(incident), Severity: incident.Severity, Summary: incident.Summary,
			OpenedAt: incident.OpenedAt, Duration: now.Sub(incident.OpenedAt),
		})
	}
	sort.Slice(data.Incidents, func(i, j int) bool {
		if data.Incidents[i].Severity != data.Incidents[j].Severity {
			return data.Incidents[i].Severity == "critical"
		}
		return data.Incidents[i].Duration > data.Incidents[j].Duration
	})
	if data.Host, err = s.hostView(ctx, now); err != nil {
		return nil, err
	}
	if data.Apps, err = s.appViews(ctx, model, now); err != nil {
		return nil, err
	}
	if data.Certs, err = s.certViews(ctx, now); err != nil {
		return nil, err
	}
	if data.Units, err = s.unitViews(ctx); err != nil {
		return nil, err
	}
	events, err := s.store.AllEvents(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	for i, event := range events {
		if i == 20 {
			break
		}
		view := eventView{TS: event.TS, Kind: event.Kind, Severity: event.Severity, Title: event.Title, Subject: event.Subject}
		if event.AppID != nil {
			view.App = s.appName(model, *event.AppID)
		}
		data.Events = append(data.Events, view)
	}
	_ = now2
	return data, nil
}

// sourceStatus returns the recorded status of a source.
func sourceStatus(states []store.SourceState, name string) string {
	for _, state := range states {
		if state.Source == name {
			return state.Status
		}
	}
	return store.StatusUnavailable
}

// appName returns the name of an app.
func (s *Server) appName(model *discovery.Model, id int64) string {
	if model == nil {
		return ""
	}
	for _, app := range model.Apps {
		if app.ID == id {
			return app.Name
		}
	}
	return ""
}

// incidentApp returns the app name of an incident.
func incidentApp(incident store.Incident) string {
	details := map[string]any{}
	if len(incident.Details) > 0 {
		_ = json.Unmarshal(incident.Details, &details)
	}
	name, _ := details["app"].(string)
	return name
}

// hostView gathers the host strip.
func (s *Server) hostView(ctx context.Context, now time.Time) (*hostView, error) {
	view := &hostView{}
	sample, err := s.store.LatestHostSample(ctx)
	if err != nil {
		return nil, err
	}
	if sample != nil {
		view.CPU = sample.CPUPercent
		view.Load1 = sample.Load1
		view.MemAvailable = sample.MemAvailable
		view.MemTotal = sample.MemTotal
		view.SwapUsed = sample.SwapUsed
		view.SwapTotal = sample.SwapTotal
	}
	disks, err := s.store.DiskSamples(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(disks))
	for name := range disks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		samples := disks[name]
		if len(samples) == 0 {
			continue
		}
		first, latest := samples[0], samples[len(samples)-1]
		total := latest.UsedBytes + latest.AvailBytes
		disk := diskView{Name: name, Free: latest.AvailBytes}
		if total > 0 {
			disk.Usage = 100 * float64(latest.UsedBytes) / float64(total)
		}
		growth := latest.UsedBytes - first.UsedBytes
		if growth > 0 {
			hours := latest.TS.Sub(first.TS).Hours()
			if hours >= 1 {
				perHour := float64(growth) / hours
				disk.Growing = true
				disk.DaysUntilFull = float64(latest.SizeBytes-latest.UsedBytes) / perHour / 24
			}
		}
		view.Disks = append(view.Disks, disk)
	}
	usage, err := s.store.DockerDiskLatest(ctx)
	if err == nil && usage != nil {
		view.Docker = &dockerDiskView{
			Images: usage.ImagesBytes, Reclaimable: usage.ImagesReclaimableBytes,
			BuildCache: usage.BuildCacheBytes, Volumes: usage.VolumesBytes, Layers: usage.ContainersBytes,
		}
	}
	return view, nil
}

// appViews gathers the overview rows.
func (s *Server) appViews(ctx context.Context, model *discovery.Model, now time.Time) ([]appView, error) {
	if model == nil {
		return nil, nil
	}
	open, err := s.store.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	baselines, err := s.store.Baselines(ctx)
	if err != nil {
		return nil, err
	}
	hour := now.In(s.cfg.Location).Hour()
	slots, err := s.store.SlotSamples(ctx, now.Add(-10*time.Minute))
	if err != nil {
		return nil, err
	}
	crashes, err := s.store.Events(ctx, "crashed", now.Add(-24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	deploys, err := s.store.Events(ctx, "deploy", now.Add(-7*24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(model.Apps))
	for name := range model.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	var views []appView
	for _, name := range names {
		app := model.Apps[name]
		if app.Hidden {
			continue
		}
		view := appView{Name: app.Name}
		routeIDs := make([]int64, 0, len(app.Routes))
		for _, route := range app.Routes {
			routeIDs = append(routeIDs, route.ID)
			view.Domains = append(view.Domains, route.Domains...)
		}
		sort.Strings(view.Domains)
		view.Domains = dedupe(view.Domains)
		// Traffic, errors and latency over the last hour.
		stats, err := s.store.AppMinuteStats(ctx, app.ID, now.Add(-time.Hour), now)
		if err != nil {
			return nil, err
		}
		view.RequestsPerMinute = float64(stats.Requests) / 60
		if stats.Requests > 0 {
			view.ErrorRate = 100 * float64(stats.S5xx) / float64(stats.Requests)
		}
		view.P95, _ = stats.Latency.Percentile(95)
		// The baseline of the app is the mean of its routes' baselines.
		var errorBaseline, p95Baseline []float64
		for _, route := range app.Routes {
			for _, baseline := range baselines {
				if baseline.RouteID != route.ID || baseline.HourOfDay != hour {
					continue
				}
				if baseline.ErrorRate != nil && baseline.DaysUsed >= store.BaselineMinDays {
					errorBaseline = append(errorBaseline, *baseline.ErrorRate*100)
				}
				if baseline.P95MS != nil && baseline.DaysUsed >= store.BaselineMinDays {
					p95Baseline = append(p95Baseline, *baseline.P95MS)
				}
			}
		}
		view.ErrorBaseline = mean(errorBaseline)
		view.P95Baseline = mean(p95Baseline)
		// A 24-hour sparkline from the hourly tables when available, from the
		// minute tables otherwise.
		spark, err := s.sparkline(ctx, routeIDs, now)
		if err != nil {
			return nil, err
		}
		view.Sparkline = spark
		// Containers.
		for _, slot := range app.Slots {
			view.Total++
			if slot.Container == nil {
				continue
			}
			if slot.Container.State == "running" {
				view.Running++
			}
			if sample, ok := slots[slot.ID]; ok {
				view.CPU += float64(sample.CPUMillicores)
				view.Memory += sample.MemBytes
				if sample.MemLimitBytes != nil {
					view.MemoryLimit += *sample.MemLimitBytes
				}
			}
		}
		for _, event := range crashes {
			if event.AppID != nil && *event.AppID == app.ID {
				view.Restarts++
			}
		}
		for i := len(deploys) - 1; i >= 0; i-- {
			event := deploys[i]
			if event.AppID != nil && *event.AppID == app.ID {
				view.LastDeploy = event.TS
				break
			}
		}
		view.Status = appStatus(open, app)
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Status != views[j].Status {
			return statusRank(views[i].Status) < statusRank(views[j].Status)
		}
		return views[i].RequestsPerMinute > views[j].RequestsPerMinute
	})
	return views, nil
}

// sparkline returns the requests of the last 24 hours, one value per hour.
func (s *Server) sparkline(ctx context.Context, routeIDs []int64, now time.Time) ([]float64, error) {
	values := make([]float64, 24)
	if len(routeIDs) == 0 {
		return values, nil
	}
	from := now.Truncate(time.Hour).Add(-23 * time.Hour)
	bins, err := s.store.RouteSeries(ctx, routeIDs, store.Range{From: from, To: now}, store.SourceHours, time.Hour)
	if err != nil {
		return nil, err
	}
	for _, bin := range bins {
		index := int(bin.TS.Sub(from) / time.Hour)
		if index >= 0 && index < len(values) {
			values[index] += float64(bin.Requests)
		}
	}
	return values, nil
}

// appStatus returns the worst status of an app.
func appStatus(open map[store.IncidentKey]store.Incident, app *discovery.App) string {
	status := "ok"
	for _, incident := range open {
		if incident.AppID == nil || *incident.AppID != app.ID {
			continue
		}
		if incident.Severity == "critical" {
			return "critical"
		}
		status = "warning"
	}
	for _, slot := range app.Slots {
		if slot.Container == nil {
			continue
		}
		if slot.Container.Health == "unhealthy" {
			if status == "ok" {
				status = "warning"
			}
		}
		if slot.Container.State != "running" && slot.Container.State != "" {
			if status == "ok" {
				status = "warning"
			}
		}
	}
	return status
}

// statusRank orders statuses with problems first.
func statusRank(status string) int {
	switch status {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

// certViews returns every certificate check.
func (s *Server) certViews(ctx context.Context, now time.Time) ([]certView, error) {
	certs, err := s.store.Certs(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]certView, 0, len(certs))
	for _, cert := range certs {
		view := certView{Domain: cert.Domain, Status: cert.Status, Issuer: cert.Issuer,
			NotAfter: cert.NotAfter, CheckedAt: cert.CheckedAt, Error: cert.Error}
		if cert.NotAfter != nil {
			view.DaysLeft = cert.NotAfter.Sub(now).Hours() / 24
		}
		view.Attention = cert.Status == "default" || cert.Status == "mismatch" || cert.Status == "error" ||
			(cert.NotAfter != nil && view.DaysLeft <= 30)
		views = append(views, view)
	}
	return views, nil
}

// unitViews returns every recorded unit.
func (s *Server) unitViews(ctx context.Context) ([]unitView, error) {
	units, err := s.store.Units(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]unitView, 0, len(units))
	for _, unit := range units {
		views = append(views, unitView{Name: unit.Name, ActiveState: unit.ActiveState, SubState: unit.SubState,
			Result: unit.Result, ExitStatus: unit.ExitStatus, Since: unit.Since})
	}
	return views, nil
}

// mean returns the average of the values, 0 when there are none.
func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

// seriesKey is one parsed series key.
type seriesKey struct {
	kind    string
	subject string
	metric  string
}

// parseSeriesKey parses kind:subject:metric.
func parseSeriesKey(key string) (seriesKey, error) {
	parts := strings.Split(key, ":")
	if len(parts) < 2 {
		return seriesKey{}, fmt.Errorf("series key %q: want kind:subject:metric", key)
	}
	parsed := seriesKey{kind: parts[0], metric: parts[len(parts)-1], subject: strings.Join(parts[1:len(parts)-1], ":")}
	switch parsed.kind {
	case "route", "app", "slot", "host", "disk":
	default:
		return seriesKey{}, fmt.Errorf("series key %q: unknown kind %q", key, parsed.kind)
	}
	return parsed, nil
}

// seriesRange parses the range parameters of a series request.
func seriesRange(values url.Values) (store.Range, error) {
	from, okFrom := parseTime(values.Get("from"))
	to, okTo := parseTime(values.Get("to"))
	if !okFrom || !okTo {
		return store.Range{}, fmt.Errorf("from and to must be Unix seconds or RFC3339 times")
	}
	if !to.After(from) {
		return store.Range{}, fmt.Errorf("to must be after from")
	}
	points := parseInt(values.Get("points"), store.DefaultPoints)
	if points < 10 || points > 5000 {
		return store.Range{}, fmt.Errorf("points must be between 10 and 5000")
	}
	return store.Range{From: from, To: to, Points: points}, nil
}

// seriesPoint is one timestamped value.
type seriesPoint [2]float64

// seriesAnswer is one series in the API response.
type seriesAnswer struct {
	Key    string        `json:"key"`
	Label  string        `json:"label"`
	Unit   string        `json:"unit"`
	Points []seriesPoint `json:"points"`
}

// seriesResponse is the body of /api/series.
type seriesResponse struct {
	From   time.Time      `json:"from"`
	To     time.Time      `json:"to"`
	Bin    int64          `json:"bin_seconds"`
	Source string         `json:"source"`
	Series []seriesAnswer `json:"series"`
}

// seriesAPI returns time series for the requested keys.
func (s *Server) seriesAPI(w http.ResponseWriter, request *http.Request) {
	raw := request.URL.Query().Get("keys")
	if raw == "" {
		badRequest(w, "keys is required")
		return
	}
	keys := strings.Split(raw, ",")
	if len(keys) > 20 {
		badRequest(w, "at most 20 series are allowed")
		return
	}
	timeRange, err := seriesRange(request.URL.Query())
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	source, bin := store.Resolution(timeRange, s.cfg.Retention.Minute, s.clock.Now())
	answer := seriesResponse{From: timeRange.From, To: timeRange.To, Bin: int64(bin / time.Second), Source: string(source)}
	parsedKeys := make([]seriesKey, 0, len(keys))
	for _, key := range keys {
		parsed, err := parseSeriesKey(key)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		parsedKeys = append(parsedKeys, parsed)
	}
	model := s.model.Model()
	for i, parsed := range parsedKeys {
		series, err := s.series(request.Context(), model, parsed, timeRange, source, bin)
		if err != nil {
			s.logger.Error("read series", "key", keys[i], "error", err)
			http.Error(w, "cannot read the series", http.StatusInternalServerError)
			return
		}
		answer.Series = append(answer.Series, series)
	}
	writeJSON(w, answer)
}

// series reads one series.
func (s *Server) series(ctx context.Context, model *discovery.Model, key seriesKey, timeRange store.Range, source store.RangeSource, bin time.Duration) (seriesAnswer, error) {
	answer := seriesAnswer{Key: key.kind + ":" + key.subject + ":" + key.metric, Label: key.subject, Unit: "count"}
	switch key.kind {
	case "route", "app":
		routeIDs, err := s.routeIDs(model, key)
		if err != nil {
			return answer, err
		}
		if key.kind == "app" {
			answer.Label = key.subject
		}
		bins, err := s.store.RouteSeries(ctx, routeIDs, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, unit := routeMetric(key.metric, entry)
			answer.Unit = unit
			answer.Points = append(answer.Points, seriesPoint{float64(entry.TS.Unix()), value})
		}
	case "slot":
		slotID, err := s.slotID(model, key.subject)
		if err != nil {
			return answer, err
		}
		bins, err := s.store.SlotSeries(ctx, []int64{slotID}, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, unit := slotMetric(key.metric, entry)
			answer.Unit = unit
			answer.Points = append(answer.Points, seriesPoint{float64(entry.TS.Unix()), value})
		}
	case "host":
		bins, err := s.store.HostSeries(ctx, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, unit := hostMetric(key.metric, entry)
			answer.Unit = unit
			answer.Points = append(answer.Points, seriesPoint{float64(entry.TS.Unix()), value})
		}
	case "disk":
		bins, err := s.store.DiskSeries(ctx, []string{key.subject}, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		answer.Label = key.subject
		for _, entry := range bins {
			value, unit := diskMetric(key.metric, entry)
			answer.Unit = unit
			answer.Points = append(answer.Points, seriesPoint{float64(entry.TS.Unix()), value})
		}
	default:
		return answer, fmt.Errorf("unknown kind %q", key.kind)
	}
	if answer.Points == nil {
		answer.Points = []seriesPoint{}
	}
	return answer, nil
}

// routeIDs resolves the routes of a series key.
func (s *Server) routeIDs(model *discovery.Model, key seriesKey) ([]int64, error) {
	if model == nil {
		return nil, fmt.Errorf("no model")
	}
	if key.kind == "route" {
		route, ok := model.Routes[key.subject]
		if !ok {
			return nil, fmt.Errorf("unknown route %q", key.subject)
		}
		return []int64{route.ID}, nil
	}
	app, ok := model.Apps[key.subject]
	if !ok {
		return nil, fmt.Errorf("unknown app %q", key.subject)
	}
	routeIDs := make([]int64, 0, len(app.Routes))
	for _, route := range app.Routes {
		routeIDs = append(routeIDs, route.ID)
	}
	sort.Slice(routeIDs, func(i, j int) bool { return routeIDs[i] < routeIDs[j] })
	return routeIDs, nil
}

// slotID resolves a slot key "app / key".
func (s *Server) slotID(model *discovery.Model, subject string) (int64, error) {
	if model == nil {
		return 0, fmt.Errorf("no model")
	}
	for _, app := range model.Apps {
		for _, slot := range app.Slots {
			if app.Name+" / "+slot.Key == subject {
				return slot.ID, nil
			}
		}
	}
	return 0, fmt.Errorf("unknown slot %q", subject)
}

// routeMetric returns one metric of a route bin.
func routeMetric(metric string, entry store.RouteBin) (float64, string) {
	switch metric {
	case "requests":
		return float64(entry.Requests), "count"
	case "2xx":
		return float64(entry.S2xx), "count"
	case "3xx":
		return float64(entry.S3xx), "count"
	case "4xx":
		return float64(entry.S4xx), "count"
	case "5xx":
		return float64(entry.S5xx), "count"
	case "error_rate":
		if entry.Requests == 0 {
			return 0, "percent"
		}
		return 100 * float64(entry.S5xx) / float64(entry.Requests), "percent"
	case "p50":
		value, _ := entry.Latency.Percentile(50)
		return value, "ms"
	case "p95":
		value, _ := entry.Latency.Percentile(95)
		return value, "ms"
	case "p99":
		value, _ := entry.Latency.Percentile(99)
		return value, "ms"
	case "bytes":
		return float64(entry.Bytes), "bytes"
	}
	return 0, "count"
}

// slotMetric returns one metric of a slot bin.
func slotMetric(metric string, entry store.SlotBin) (float64, string) {
	switch metric {
	case "cpu":
		return entry.CPUAvg, "millicores"
	case "cpu_max":
		return entry.CPUMax, "millicores"
	case "memory":
		return entry.MemAvg, "bytes"
	case "memory_max":
		return entry.MemMax, "bytes"
	case "net_rx":
		return float64(entry.NetRx), "bytes"
	case "net_tx":
		return float64(entry.NetTx), "bytes"
	}
	return 0, "count"
}

// hostMetric returns one metric of a host bin.
func hostMetric(metric string, entry store.HostBin) (float64, string) {
	switch metric {
	case "cpu":
		return entry.CPUAvg, "percent"
	case "load1":
		return entry.Load1Avg, "load"
	case "memory":
		return entry.MemAvailableAvg, "bytes"
	case "memory_min":
		return entry.MemAvailableMin, "bytes"
	}
	return 0, "count"
}

// diskMetric returns one metric of a disk bin.
func diskMetric(metric string, entry store.DiskBin) (float64, string) {
	switch metric {
	case "used":
		if entry.SizeBytes <= 0 {
			return 0, "percent"
		}
		return 100 * float64(entry.UsedBytesMax) / float64(entry.SizeBytes), "percent"
	case "used_bytes":
		return float64(entry.UsedBytesMax), "bytes"
	case "free":
		return float64(entry.AvailBytesMin), "bytes"
	case "inodes":
		return float64(entry.InodesUsedMax), "count"
	}
	return 0, "count"
}

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
		P50MS    float64 `json:"p50_ms"`
		P95MS    float64 `json:"p95_ms"`
	}
	entries := make([]entry, 0, len(stats))
	for _, stat := range stats {
		p50, _ := stat.Latency.Percentile(50)
		p95, _ := stat.Latency.Percentile(95)
		entries = append(entries, entry{Method: stat.Method, Pattern: stat.Pattern, Requests: stat.Requests,
			S4xx: stat.S4xx, S5xx: stat.S5xx, P50MS: p50, P95MS: p95})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].S5xx != entries[j].S5xx {
			return entries[i].S5xx > entries[j].S5xx
		}
		return entries[i].P95MS > entries[j].P95MS
	})
	writeJSON(w, map[string]any{"paths": entries})
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
		ID       int64     `json:"id"`
		TS       time.Time `json:"ts"`
		Kind     string    `json:"kind"`
		Severity string    `json:"severity"`
		Subject  string    `json:"subject"`
		Title    string    `json:"title"`
		App      string    `json:"app"`
	}
	var items []eventJSON
	for _, event := range events {
		if appID != 0 && (event.AppID == nil || *event.AppID != appID) {
			continue
		}
		if kind != "" && event.Kind != kind {
			continue
		}
		item := eventJSON{ID: event.ID, TS: event.TS, Kind: event.Kind, Severity: event.Severity,
			Subject: event.Subject, Title: event.Title}
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
		ID         int64      `json:"id"`
		Rule       string     `json:"rule"`
		Subject    string     `json:"subject"`
		Severity   string     `json:"severity"`
		OpenedAt   time.Time  `json:"opened_at"`
		ResolvedAt *time.Time `json:"resolved_at"`
		Summary    string     `json:"summary"`
		App        string     `json:"app"`
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
			Subject: incident.Subject, Severity: incident.Severity, OpenedAt: incident.OpenedAt,
			ResolvedAt: incident.ResolvedAt, Summary: incident.Summary, App: incidentApp(incident)})
	}
	writeJSON(w, map[string]any{"events": items, "incidents": incidentItems})
}

// patternsAPI returns the heatmap, day profile and trend of one series key.
func (s *Server) patternsAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	raw := query.Get("key")
	if raw == "" {
		badRequest(w, "key is required")
		return
	}
	key, err := parseSeriesKey(raw)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	weeks := parseInt(query.Get("weeks"), 4)
	if weeks < 1 || weeks > 52 {
		badRequest(w, "weeks must be between 1 and 52")
		return
	}
	now := s.clock.Now()
	from := now.Truncate(24*time.Hour).AddDate(0, 0, -7*weeks)
	model := s.model.Model()
	timeRange := store.Range{From: from, To: now, Points: int(now.Sub(from)/time.Hour) + 1}
	bins, err := s.patternBins(request.Context(), model, key, timeRange)
	if err != nil {
		http.NotFound(w, request)
		return
	}
	heatmap := make([][]float64, 7)
	for day := range heatmap {
		heatmap[day] = make([]float64, 24)
	}
	daySums := make([][]float64, 24)
	dayCounts := make([]int, 24)
	trend := make(map[string]float64)
	gauge := key.metric == "cpu" || key.metric == "memory" || key.metric == "load1" || key.metric == "used"
	for _, bin := range bins {
		local := bin.at.In(s.cfg.Location)
		weekday := int(local.Weekday()+6) % 7
		heatmap[weekday][local.Hour()] += bin.value
		daySums[local.Hour()] = append(daySums[local.Hour()], bin.value)
		dayCounts[local.Hour()]++
		day := local.Format("2006-01-02")
		trend[day] += bin.value
	}
	// Average the heatmap over the number of occurrences of each slot.
	counts := make([][]float64, 7)
	for day := range counts {
		counts[day] = make([]float64, 24)
	}
	for _, bin := range bins {
		local := bin.at.In(s.cfg.Location)
		counts[int(local.Weekday()+6)%7][local.Hour()]++
	}
	for day := range heatmap {
		for hour := range heatmap[day] {
			if counts[day][hour] > 0 {
				heatmap[day][hour] /= counts[day][hour]
			}
		}
	}
	profile := make([]float64, 24)
	for hour := range daySums {
		profile[hour] = mean(daySums[hour])
	}
	keys := make([]string, 0, len(trend))
	for day := range trend {
		keys = append(keys, day)
	}
	sort.Strings(keys)
	type trendPoint struct {
		Day   string  `json:"day"`
		Value float64 `json:"value"`
	}
	var trendPoints []trendPoint
	for _, day := range keys {
		value := trend[day]
		if gauge {
			// Gauges are averaged over the day.
			value /= 24
		}
		trendPoints = append(trendPoints, trendPoint{Day: day, Value: value})
	}
	writeJSON(w, map[string]any{
		"key":         key.kind + ":" + key.subject + ":" + key.metric,
		"heatmap":     heatmap,
		"day_profile": profile,
		"trend":       trendPoints,
	})
}

// patternBin is one hourly value of a series.
type patternBin struct {
	at    time.Time
	value float64
}

// patternBins reads the hourly values of one series key.
func (s *Server) patternBins(ctx context.Context, model *discovery.Model, key seriesKey, timeRange store.Range) ([]patternBin, error) {
	series, err := s.series(ctx, model, key, timeRange, store.SourceHours, time.Hour)
	if err != nil {
		return nil, err
	}
	bins := make([]patternBin, 0, len(series.Points))
	for _, point := range series.Points {
		bins = append(bins, patternBin{at: time.Unix(int64(point[0]), 0).UTC(), value: point[1]})
	}
	return bins, nil
}

// logsAPI streams container logs as server-sent events when follow is set.
func (s *Server) logsAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	container := query.Get("container")
	if container == "" {
		badRequest(w, "container is required")
		return
	}
	since, okSince := parseTime(query.Get("since"))
	until, okUntil := parseTime(query.Get("until"))
	if !okSince {
		badRequest(w, "since is required")
		return
	}
	if !okUntil {
		until = since.Add(4 * time.Minute)
	}
	follow := query.Get("follow") == "true"
	s.streamLogs(w, request, container, since, until, follow)
}

// streamLogs streams one container's logs.
func (s *Server) streamLogs(w http.ResponseWriter, request *http.Request, container string, since, until time.Time, follow bool) {
	if follow {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming is not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_ = flusher
	}
	client, err := s.connector.Client(request.Context())
	if err != nil {
		s.logger.Warn("container logs unavailable", "error", err)
		http.Error(w, "the Docker API is not reachable", http.StatusServiceUnavailable)
		return
	}
	model := s.model.Model()
	tty := false
	if model != nil {
		for _, running := range model.Containers {
			if running.ID == container || running.Name == container {
				tty = running.TTY
				container = running.ID
				break
			}
		}
	}
	logs, err := client.Logs(request.Context(), container, dockerLogOptions(since, follow, tty))
	if err != nil {
		s.logger.Warn("container logs unavailable", "error", err)
		http.Error(w, "cannot read the container logs", http.StatusBadGateway)
		return
	}
	defer logs.Close()
	if follow {
		s.streamLines(w, logs, until)
		return
	}
	body, err := ioReadAll(logs)
	if err != nil {
		http.Error(w, "cannot read the container logs", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(trimLines(body, until, s.cfg.Location))
}

// overviewAPI returns everything on the overview page.
func (s *Server) overviewAPI(w http.ResponseWriter, request *http.Request) {
	data, err := s.loadOverview(request.Context())
	if err != nil {
		s.logger.Error("overview API", "error", err)
		http.Error(w, "cannot read the overview", http.StatusInternalServerError)
		return
	}
	writeJSON(w, data)
}
