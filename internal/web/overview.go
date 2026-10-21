package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
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
	ID       int64          `json:"id"`
	Rule     string         `json:"rule"`
	Subject  string         `json:"subject"`
	App      string         `json:"app"`
	Severity store.Severity `json:"severity"`
	Summary  string         `json:"summary"`
	OpenedAt time.Time      `json:"opened_at"`
	Duration time.Duration  `json:"duration"`
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
	CPU          float64 `json:"cpu"`
	Load1        float64 `json:"load1"`
	MemAvailable int64   `json:"mem_available"`
	MemTotal     int64   `json:"mem_total"`
	// MemUsage and SwapUsage are percentages of the totals.
	MemUsage  float64         `json:"mem_usage"`
	SwapUsed  int64           `json:"swap_used"`
	SwapTotal int64           `json:"swap_total"`
	SwapUsage float64         `json:"swap_usage"`
	Disks     []diskView      `json:"disks"`
	Docker    *dockerDiskView `json:"docker"`
}

// appView is one row of the overview.
type appView struct {
	Name              string    `json:"name"`
	Domains           []string  `json:"domains"`
	Status            appHealth `json:"status"`
	RequestsPerMinute float64   `json:"requests_per_minute"`
	Sparkline         []float64 `json:"sparkline"`
	ErrorRate         float64   `json:"error_rate"`
	ErrorBaseline     float64   `json:"error_baseline"`
	P95               latency   `json:"p95"`
	P95Baseline       float64   `json:"p95_baseline"`
	CPU               float64   `json:"cpu"`
	Memory            int64     `json:"memory"`
	MemoryLimit       int64     `json:"memory_limit"`
	// MemoryUsage is the percentage of the limit; 0 without a limit.
	MemoryUsage float64   `json:"memory_usage"`
	Running     int       `json:"running"`
	Total       int       `json:"total"`
	Restarts    int       `json:"restarts"`
	LastDeploy  time.Time `json:"last_deploy"`
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

// summaryView is the headline of the overview: how the host is doing at a
// glance.
type summaryView struct {
	State  appHealth `json:"state"`
	Title  string    `json:"title"`
	Detail string    `json:"detail"`
}

// overviewData is everything the overview page and its API show.
type overviewData struct {
	Summary   summaryView    `json:"summary"`
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

// overviewEvents is how many recent events the overview lists.
const overviewEvents = 20

// loadOverview gathers the overview data.
func (s *Server) loadOverview(ctx context.Context) (*overviewData, error) {
	now := s.clock.Now()
	model := s.model.Model()
	data := &overviewData{HostName: s.cfg.HostName, Now: now, Version: s.version}
	var err error
	if data.Sources, err = s.sourceList(ctx); err != nil {
		return nil, err
	}
	open, err := s.store.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	for _, incident := range open {
		data.Incidents = append(data.Incidents, incidentView{
			ID: incident.ID, Rule: incident.Rule, Subject: s.cfg.SubjectNames.Display(incident.Subject),
			App: incidentApp(incident), Severity: incident.Severity, Summary: incident.Summary,
			OpenedAt: incident.OpenedAt, Duration: now.Sub(incident.OpenedAt),
		})
	}
	sort.Slice(data.Incidents, func(i, j int) bool {
		if data.Incidents[i].Severity != data.Incidents[j].Severity {
			return data.Incidents[i].Severity == store.SeverityCritical
		}
		return data.Incidents[i].Duration > data.Incidents[j].Duration
	})
	if data.Host, err = s.hostView(ctx, now); err != nil {
		return nil, err
	}
	if data.Apps, err = s.appViews(ctx, model, open, now); err != nil {
		return nil, err
	}
	if data.Certs, err = s.certViews(ctx, now); err != nil {
		return nil, err
	}
	if data.Units, err = s.unitViews(ctx); err != nil {
		return nil, err
	}
	data.Summary = summarize(data.Incidents, data.Apps)
	events, err := s.store.LatestEvents(ctx, overviewEvents)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		view := eventView{TS: event.TS, Kind: event.Kind, Severity: event.Severity, Title: event.Title, Subject: s.cfg.SubjectNames.Display(event.Subject)}
		if event.AppID != nil {
			view.App = s.appName(model, *event.AppID)
		}
		data.Events = append(data.Events, view)
	}
	return data, nil
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
		view.MemUsage = percentOf(sample.MemTotal-sample.MemAvailable, sample.MemTotal)
		view.SwapUsage = percentOf(sample.SwapUsed, sample.SwapTotal)
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
		if len(disks[name]) == 0 {
			continue
		}
		view.Disks = append(view.Disks, diskOf(name, disks[name]))
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

// percentOf returns part as a percentage of total, 0 when total is not
// positive.
func percentOf(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

// summarize builds the headline from the open incidents and the apps.
func summarize(incidents []incidentView, apps []appView) summaryView {
	critical, warnings := 0, 0
	for _, incident := range incidents {
		if incident.Severity == store.SeverityCritical {
			critical++
		} else {
			warnings++
		}
	}
	detail := plural(len(apps), "app") + " monitored"
	switch {
	case critical > 0:
		return summaryView{State: healthCritical, Title: plural(critical, "critical incident") + " open", Detail: detail}
	case warnings > 0:
		return summaryView{State: healthWarning, Title: plural(warnings, "incident") + " open", Detail: detail}
	}
	for _, app := range apps {
		if app.Status != healthOK {
			return summaryView{State: healthWarning, Title: "An app needs attention", Detail: detail}
		}
	}
	return summaryView{State: healthOK, Title: "All systems normal", Detail: detail}
}

// diskOf summarizes one filesystem from its samples, oldest first, and
// forecasts when it fills up if usage grew over the period. Free space and the
// forecast both use the space available to unprivileged writers, so "free" and
// "full in" agree.
func diskOf(name string, samples []store.DiskMinute) diskView {
	first, latest := samples[0], samples[len(samples)-1]
	disk := diskView{Name: name, Free: latest.AvailBytes}
	if total := latest.UsedBytes + latest.AvailBytes; total > 0 {
		disk.Usage = 100 * float64(latest.UsedBytes) / float64(total)
	}
	growth := latest.UsedBytes - first.UsedBytes
	hours := latest.TS.Sub(first.TS).Hours()
	if growth > 0 && hours >= 1 {
		perHour := float64(growth) / hours
		disk.Growing = true
		disk.DaysUntilFull = float64(latest.AvailBytes) / perHour / 24
	}
	return disk
}

// appViews gathers the overview rows. open holds the open incidents.
func (s *Server) appViews(ctx context.Context, model *discovery.Model, open map[store.IncidentKey]store.Incident, now time.Time) ([]appView, error) {
	if model == nil {
		return nil, nil
	}
	baselines, err := s.store.Baselines(ctx)
	if err != nil {
		return nil, err
	}
	hour := now.In(s.cfg.Location).Hour()
	baselinesNow := make(map[int64]store.Baseline)
	for _, baseline := range baselines {
		if baseline.HourOfDay == hour && baseline.DaysUsed >= store.BaselineMinDays {
			baselinesNow[baseline.RouteID] = baseline
		}
	}
	// Traffic, errors and latency over the last hour, and the 24-hour
	// sparklines, for every app at once.
	stats, err := s.store.AppMinuteStats(ctx, now.Add(-time.Hour), now)
	if err != nil {
		return nil, err
	}
	sparkFrom := store.TruncateLocalHour(now, s.cfg.Location).Add(-23 * time.Hour)
	hourly, err := s.store.AppHourlyRequests(ctx, sparkFrom)
	if err != nil {
		return nil, err
	}
	slots, err := s.store.SlotSamples(ctx, now.Add(-10*time.Minute))
	if err != nil {
		return nil, err
	}
	crashes, err := s.store.Events(ctx, store.EventCrashed, now.Add(-24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	deploys, err := s.store.LatestEventByApp(ctx, store.EventDeploy, store.EventRecreated)
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
		view := appView{Name: app.Name}
		for _, route := range app.Routes {
			view.Domains = append(view.Domains, route.Domains...)
		}
		sort.Strings(view.Domains)
		view.Domains = dedupe(view.Domains)
		appStats := stats[app.ID]
		view.RequestsPerMinute = float64(appStats.Requests) / 60
		if appStats.Requests > 0 {
			view.ErrorRate = 100 * float64(appStats.S5xx) / float64(appStats.Requests)
		}
		view.P95 = percentileOf(&appStats.Latency, 95)
		// The baseline of the app is the mean of its routes' baselines.
		var errorBaseline, p95Baseline []float64
		for _, route := range app.Routes {
			baseline, ok := baselinesNow[route.ID]
			if !ok {
				continue
			}
			if baseline.ErrorRate != nil {
				errorBaseline = append(errorBaseline, *baseline.ErrorRate*100)
			}
			if baseline.P95MS != nil {
				p95Baseline = append(p95Baseline, *baseline.P95MS)
			}
		}
		view.ErrorBaseline = mean(errorBaseline)
		view.P95Baseline = mean(p95Baseline)
		view.Sparkline = sparkline(hourly[app.ID], sparkFrom)
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
		view.MemoryUsage = percentOf(view.Memory, view.MemoryLimit)
		view.LastDeploy = deploys[app.ID]
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

// sparkline returns the requests of 24 hours as one value per hour, the first
// starting at from, which must be a local hour start.
func sparkline(requests map[time.Time]int64, from time.Time) []float64 {
	values := make([]float64, 24)
	for ts, count := range requests {
		if index := int(ts.Sub(from) / time.Hour); index >= 0 && index < len(values) {
			values[index] += float64(count)
		}
	}
	return values
}

// appHealth is the worst state of an app, as the overview shows it.
type appHealth string

// The app health states.
const (
	healthOK       appHealth = "ok"
	healthWarning  appHealth = "warning"
	healthCritical appHealth = "critical"
)

// appStatus returns the worst health of an app.
func appStatus(open map[store.IncidentKey]store.Incident, app *discovery.App) appHealth {
	status := healthOK
	for _, incident := range open {
		if incident.AppID == nil || *incident.AppID != app.ID {
			continue
		}
		if incident.Severity == store.SeverityCritical {
			return healthCritical
		}
		status = healthWarning
	}
	for _, slot := range app.Slots {
		if slot.Container == nil {
			continue
		}
		unhealthy := slot.Container.Health == "unhealthy"
		stopped := slot.Container.State != "running" && slot.Container.State != "" && !slot.Container.Finished()
		if unhealthy || stopped {
			status = healthWarning
		}
	}
	return status
}

// statusRank orders health states with problems first.
func statusRank(status appHealth) int {
	switch status {
	case healthCritical:
		return 0
	case healthWarning:
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

// sourceList returns the state of every source, ending with the database,
// which is working whenever the page can be built at all.
func (s *Server) sourceList(ctx context.Context) ([]sourceView, error) {
	states, err := s.store.SourceStates(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]sourceView, 0, len(states)+1)
	for _, state := range states {
		view := sourceView{Name: state.Source, Status: state.Status}
		if state.LastError != nil {
			view.LastError = *state.LastError
		}
		views = append(views, view)
	}
	return append(views, sourceView{Name: "database", Status: store.StatusOK}), nil
}

// sourceViews returns the sources for the top bar. A failure leaves the bar
// empty rather than failing the page.
func (s *Server) sourceViews(request *http.Request) []sourceView {
	views, err := s.sourceList(request.Context())
	if err != nil {
		s.logger.Warn("read source states", "error", err)
		return nil
	}
	return views
}
