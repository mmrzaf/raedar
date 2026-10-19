package rules

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// snapshot is the data one evaluation reads.
type snapshot struct {
	now   time.Time
	model *discovery.Model
	open  map[store.IncidentKey]store.Incident

	routes    map[int64]*routeInfo
	slots     map[int64]*slotInfo
	baselines map[int64]store.Baseline

	windows    map[time.Duration]map[int64]*store.RouteWindow
	pathErrors map[int64]pathTop
	pathSlow   map[int64]pathTop

	slotSamples map[int64]store.SlotSample
	crashes     map[int64][]store.Event

	host    *store.HostMinute
	disks   map[string][]store.DiskMinute
	units   []store.Unit
	certs   []store.Cert
	peers   []store.Peer
	sources []store.SourceState
}

// routeInfo is what the rules need about one route.
type routeInfo struct {
	route   *discovery.Route
	app     *discovery.App
	routeID int64
}

// slotInfo is what the rules need about one slot.
type slotInfo struct {
	app      *discovery.App
	appID    *int64
	slotID   int64
	key      string
	subject  string
	critical bool
	alerts   bool
	health   string
	state    string
	running  bool
}

// pathTop is the path that leads a rule's context.
type pathTop struct {
	pattern  string
	requests int64
	s5xx     int64
	p95      float64
}

// collect reads everything one evaluation needs.
func (e *Engine) collect(ctx context.Context, now time.Time) (*snapshot, error) {
	s := &snapshot{
		now:        now,
		model:      e.model.Model(),
		routes:     make(map[int64]*routeInfo),
		slots:      make(map[int64]*slotInfo),
		baselines:  make(map[int64]store.Baseline),
		windows:    make(map[time.Duration]map[int64]*store.RouteWindow),
		pathErrors: make(map[int64]pathTop),
		pathSlow:   make(map[int64]pathTop),
		disks:      make(map[string][]store.DiskMinute),
	}
	open, err := e.store.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	s.open = open
	s.fillModel()
	hour := now.In(e.cfg.Location).Hour()
	baselines, err := e.store.Baselines(ctx)
	if err != nil {
		return nil, err
	}
	for _, baseline := range baselines {
		if baseline.HourOfDay == hour {
			s.baselines[baseline.RouteID] = baseline
		}
	}
	for _, length := range []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute} {
		windows, err := e.window(ctx, now, length)
		if err != nil {
			return nil, err
		}
		s.windows[length] = windows
	}
	end := now.Truncate(time.Minute)
	errors, err := e.store.PathWindows(ctx, end.Add(-5*time.Minute), end)
	if err != nil {
		return nil, err
	}
	s.pathErrors = topByErrors(errors)
	slow, err := e.store.PathWindows(ctx, end.Add(-10*time.Minute), end)
	if err != nil {
		return nil, err
	}
	s.pathSlow = topByLatency(slow, 10)
	if s.slotSamples, err = e.store.SlotSamples(ctx, now.Add(-10*time.Minute)); err != nil {
		return nil, err
	}
	crashes, err := e.store.Events(ctx, "crashed", now.Add(-30*time.Minute), now.Add(time.Second))
	if err != nil {
		return nil, err
	}
	s.crashes = make(map[int64][]store.Event)
	for _, event := range crashes {
		if event.SlotID != nil {
			s.crashes[*event.SlotID] = append(s.crashes[*event.SlotID], event)
		}
	}
	if s.host, err = e.store.LatestHostSample(ctx); err != nil {
		return nil, err
	}
	if s.disks, err = e.store.DiskSamples(ctx, now.Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	if s.units, err = e.store.Units(ctx); err != nil {
		return nil, err
	}
	if s.certs, err = e.store.Certs(ctx); err != nil {
		return nil, err
	}
	if s.peers, err = e.store.Peers(ctx); err != nil {
		return nil, err
	}
	if s.sources, err = e.store.SourceStates(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// fillModel indexes the model the way the rules read it.
func (s *snapshot) fillModel() {
	if s.model == nil {
		return
	}
	for _, route := range s.model.Routes {
		if route.ID == 0 {
			continue
		}
		s.routes[route.ID] = &routeInfo{route: route, app: route.App, routeID: route.ID}
	}
	for _, app := range s.model.Apps {
		for _, slot := range app.Slots {
			if slot.ID == 0 {
				continue
			}
			info := &slotInfo{
				app:      app,
				slotID:   slot.ID,
				key:      slot.Key,
				subject:  app.Name + " / " + slot.Key,
				critical: app.Critical,
				alerts:   app.Alerts,
			}
			if app.ID > 0 {
				appID := app.ID
				info.appID = &appID
			}
			if slot.Container != nil {
				info.health = slot.Container.Health
				info.state = slot.Container.State
				info.running = slot.Container.State == "running"
			}
			s.slots[slot.ID] = info
		}
	}
}

// window returns the route data of one window, including the aggregator's
// unwritten buckets.
func (e *Engine) window(ctx context.Context, now time.Time, length time.Duration) (map[int64]*store.RouteWindow, error) {
	end := now.Truncate(time.Minute)
	start := end.Add(-length)
	rows, err := e.store.RouteWindows(ctx, start, end)
	if err != nil {
		return nil, err
	}
	merged := make(map[int64]*store.RouteWindow, len(rows))
	for _, row := range rows {
		row := row
		merged[row.RouteID] = &row
	}
	if e.deltas == nil {
		return merged, nil
	}
	for _, delta := range e.deltas.Deltas(start) {
		if delta.TS.Before(start) || !delta.TS.Before(end) {
			continue
		}
		window := merged[delta.RouteID]
		if window == nil {
			window = &store.RouteWindow{RouteID: delta.RouteID}
			merged[delta.RouteID] = window
		}
		window.Requests += delta.Requests
		window.S1xx += delta.S1xx
		window.S2xx += delta.S2xx
		window.S3xx += delta.S3xx
		window.S4xx += delta.S4xx
		window.S5xx += delta.S5xx
		window.NoBackend += delta.NoBackend
		window.Bytes += delta.Bytes
		window.Latency.Merge(&delta.Latency)
	}
	return merged, nil
}

// topByErrors returns the path with the most 5xx responses per route.
func topByErrors(windows []store.PathWindowSummary) map[int64]pathTop {
	top := make(map[int64]pathTop)
	for _, window := range windows {
		current, ok := top[window.RouteID]
		if !ok || window.S5xx > current.s5xx {
			top[window.RouteID] = pathTop{pattern: window.Pattern, requests: window.Requests, s5xx: window.S5xx}
		}
	}
	return top
}

// topByLatency returns the path with the highest p95 per route among the
// paths with at least minRequests requests.
func topByLatency(windows []store.PathWindowSummary, minRequests int64) map[int64]pathTop {
	top := make(map[int64]pathTop)
	for _, window := range windows {
		if window.Requests < minRequests {
			continue
		}
		p95, _ := window.Latency.Percentile(95)
		current, ok := top[window.RouteID]
		if !ok || p95 > current.p95 {
			top[window.RouteID] = pathTop{pattern: window.Pattern, requests: window.Requests, p95: p95}
		}
	}
	return top
}

// readings evaluates every rule against the snapshot.
func (e *Engine) readings(now time.Time, s *snapshot) []reading {
	var readings []reading
	readings = append(readings, e.routeReadings(now, s)...)
	readings = append(readings, e.containerReadings(now, s)...)
	readings = append(readings, e.hostReadings(now, s)...)
	readings = append(readings, e.unitReadings(now, s)...)
	readings = append(readings, e.certReadings(now, s)...)
	readings = append(readings, e.peerReadings(now, s)...)
	readings = append(readings, e.sourceReadings(now, s)...)
	return readings
}

// routeReadings evaluates the route rules.
func (e *Engine) routeReadings(now time.Time, s *snapshot) []reading {
	var readings []reading
	routeIDs := make([]int64, 0, len(s.routes))
	for routeID := range s.routes {
		routeIDs = append(routeIDs, routeID)
	}
	sort.Slice(routeIDs, func(i, j int) bool { return routeIDs[i] < routeIDs[j] })
	// A route that is down stays quiet for the error and latency rules.
	down := make(map[int64]bool)
	for routeID, info := range s.routes {
		window := s.windows[2*time.Minute][routeID]
		firing := window != nil && window.Requests >= 3 && window.NoBackend*2 >= window.Requests
		if firing {
			down[routeID] = true
		}
		readings = append(readings, reading{
			rule:         RuleRouteDown,
			subject:      info.route.Name,
			appID:        appIDOf(info.app),
			routeID:      idOf(routeID),
			severity:     "critical",
			firing:       firing,
			alerts:       alertsOf(info.app),
			summary:      "no backend for requests",
			details:      windowDetails(window, nil),
			resolveAfter: e.cfg.Rules.ResolveAfter,
		})
	}
	for _, routeID := range routeIDs {
		info := s.routes[routeID]
		minRate := e.cfg.Rules.ErrorsMinRate
		if info.app != nil && info.app.ErrorsMinRate != nil {
			minRate = *info.app.ErrorsMinRate
		}
		severity := "warning"
		if info.app != nil && info.app.Critical {
			severity = "critical"
		}
		readings = append(readings, e.routeErrorReading(info, s, minRate, severity, down[routeID]))
		slowMin := e.cfg.Rules.SlowMinP95
		if info.app != nil && info.app.SlowMinP95 != nil {
			slowMin = *info.app.SlowMinP95
		}
		readings = append(readings, e.routeSlowReading(info, s, slowMin, severity, down[routeID]))
	}
	return readings
}

// routeErrorReading evaluates route.errors for one route.
func (e *Engine) routeErrorReading(info *routeInfo, s *snapshot, minRate config.Percent, severity string, down bool) reading {
	reading := reading{
		rule:         RuleRouteErrors,
		subject:      info.route.Name,
		appID:        appIDOf(info.app),
		routeID:      idOf(info.routeID),
		severity:     severity,
		alerts:       alertsOf(info.app),
		openAfter:    2 * time.Minute,
		resolveAfter: e.cfg.Rules.ResolveAfter,
		summary:      "5xx errors",
	}
	window := s.windows[5*time.Minute][info.routeID]
	if window == nil || down {
		return reading
	}
	requests := window.Requests
	rate := 0.0
	if requests > 0 {
		rate = float64(window.S5xx) / float64(requests)
	}
	baseline := 0.0
	baselineOK := true
	if computed, ok := s.baselines[info.routeID]; ok && computed.ErrorRate != nil && computed.DaysUsed >= store.BaselineMinDays {
		baseline = *computed.ErrorRate
		baselineOK = rate >= e.cfg.Rules.ErrorsFactor*baseline
	}
	reading.firing = requests >= int64(e.cfg.Rules.ErrorsMinRequests) && window.S5xx >= 5 &&
		rate*100 >= float64(minRate) && baselineOK
	reading.details = map[string]any{
		"requests": requests,
		"errors":   window.S5xx,
		"rate":     rate,
		"baseline": baseline,
		"factor":   e.cfg.Rules.ErrorsFactor,
		"min_rate": float64(minRate),
	}
	if top, ok := s.pathErrors[info.routeID]; ok && top.s5xx > 0 {
		reading.details["path"] = top.pattern
		reading.details["path_errors"] = top.s5xx
		reading.details["path_requests"] = top.requests
	}
	return reading
}

// routeSlowReading evaluates route.slow for one route.
func (e *Engine) routeSlowReading(info *routeInfo, s *snapshot, minP95 time.Duration, severity string, down bool) reading {
	reading := reading{
		rule:         RuleRouteSlow,
		subject:      info.route.Name,
		appID:        appIDOf(info.app),
		routeID:      idOf(info.routeID),
		severity:     severity,
		alerts:       alertsOf(info.app),
		openAfter:    5 * time.Minute,
		resolveAfter: e.cfg.Rules.ResolveAfter,
		summary:      "slow responses",
	}
	window := s.windows[10*time.Minute][info.routeID]
	if window == nil || down {
		return reading
	}
	p95, over := window.Latency.Percentile(95)
	baseline := 0.0
	baselineOK := true
	if computed, ok := s.baselines[info.routeID]; ok && computed.P95MS != nil && computed.DaysUsed >= store.BaselineMinDays {
		baseline = *computed.P95MS
		baselineOK = p95 >= e.cfg.Rules.SlowFactor*baseline
	}
	reading.firing = window.Latency.Count() >= int64(e.cfg.Rules.SlowMinRequests) &&
		p95 >= float64(minP95)/float64(time.Millisecond) && baselineOK
	reading.details = map[string]any{
		"requests":    window.Latency.Count(),
		"p95_ms":      p95,
		"over_60s":    over,
		"baseline_ms": baseline,
		"factor":      e.cfg.Rules.SlowFactor,
		"min_p95_ms":  float64(minP95) / float64(time.Millisecond),
	}
	if top, ok := s.pathSlow[info.routeID]; ok {
		reading.details["path"] = top.pattern
		reading.details["path_p95_ms"] = top.p95
		reading.details["path_requests"] = top.requests
	}
	return reading
}

// containerReadings evaluates the container rules.
func (e *Engine) containerReadings(now time.Time, s *snapshot) []reading {
	var readings []reading
	slotIDs := make([]int64, 0, len(s.slots))
	for slotID := range s.slots {
		slotIDs = append(slotIDs, slotID)
	}
	sort.Slice(slotIDs, func(i, j int) bool { return slotIDs[i] < slotIDs[j] })
	for _, slotID := range slotIDs {
		slot := s.slots[slotID]
		severity := "warning"
		if slot.critical {
			severity = "critical"
		}
		readings = append(readings, reading{
			rule:         RuleContainerUnhealthy,
			subject:      slot.subject,
			appID:        slot.appID,
			slotID:       idOf(slot.slotID),
			severity:     severity,
			firing:       slot.health == "unhealthy",
			alerts:       slot.alerts,
			openAfter:    2 * time.Minute,
			resolveAfter: e.cfg.Rules.ResolveAfter,
			summary:      "health check failing",
			details:      map[string]any{"health": slot.health},
		})
		readings = append(readings, e.memoryReading(slot, s))
		readings = append(readings, e.restartingReading(now, slot, s))
		readings = append(readings, e.stoppedReading(now, slot, s, severity))
	}
	return readings
}

// memoryReading evaluates container.memory for one slot.
func (e *Engine) memoryReading(slot *slotInfo, s *snapshot) reading {
	reading := reading{
		rule:         RuleContainerMemory,
		subject:      slot.subject,
		appID:        slot.appID,
		slotID:       idOf(slot.slotID),
		severity:     "warning",
		alerts:       slot.alerts,
		openAfter:    10 * time.Minute,
		resolveAfter: e.cfg.Rules.ResolveAfter,
		summary:      "memory near the limit",
	}
	sample, ok := s.slotSamples[slot.slotID]
	if !ok || sample.MemLimitBytes == nil || *sample.MemLimitBytes == 0 {
		return reading
	}
	usage := float64(sample.MemBytes) / float64(*sample.MemLimitBytes) * 100
	reading.firing = usage >= float64(e.cfg.Rules.MemoryLimitWarn)
	reading.details = map[string]any{
		"used_bytes":  sample.MemBytes,
		"limit_bytes": *sample.MemLimitBytes,
		"usage":       usage,
		"threshold":   float64(e.cfg.Rules.MemoryLimitWarn),
	}
	return reading
}

// restartingReading evaluates container.restarting for one slot.
func (e *Engine) restartingReading(now time.Time, slot *slotInfo, s *snapshot) reading {
	reading := reading{
		rule:         RuleContainerRestarting,
		subject:      slot.subject,
		appID:        slot.appID,
		slotID:       idOf(slot.slotID),
		severity:     "critical",
		alerts:       slot.alerts,
		resolveAfter: 10 * time.Minute,
		summary:      "container restarting",
	}
	var exitCodes []int
	for _, event := range s.crashes[slot.slotID] {
		if event.TS.Before(now.Add(-10 * time.Minute)) {
			continue
		}
		exitCodes = append(exitCodes, crashExitCode(event))
	}
	reading.firing = len(exitCodes) >= 3
	details := map[string]any{"crashes": len(exitCodes)}
	if len(exitCodes) > 0 {
		details["last_exit_code"] = exitCodes[len(exitCodes)-1]
	}
	reading.details = details
	return reading
}

// stoppedReading evaluates container.stopped for one slot.
func (e *Engine) stoppedReading(now time.Time, slot *slotInfo, s *snapshot, severity string) reading {
	reading := reading{
		rule:         RuleContainerStopped,
		subject:      slot.subject,
		appID:        slot.appID,
		slotID:       idOf(slot.slotID),
		severity:     severity,
		alerts:       slot.alerts,
		openAfter:    time.Minute,
		resolveAfter: 0,
		summary:      "container stopped after a crash",
	}
	var last *store.Event
	for i := range s.crashes[slot.slotID] {
		event := &s.crashes[slot.slotID][i]
		if event.TS.Before(now.Add(-30 * time.Minute)) {
			continue
		}
		last = event
	}
	reading.firing = last != nil && !slot.running
	reading.details = map[string]any{"running": slot.running, "state": slot.state}
	if last != nil {
		reading.details["exit_code"] = crashExitCode(*last)
		reading.details["crashed_at"] = last.TS
	}
	return reading
}

// crashExitCode returns the exit code of a crash event.
func crashExitCode(event store.Event) int {
	payload := struct {
		ExitCode int `json:"exit_code"`
	}{}
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return 0
	}
	return payload.ExitCode
}

// windowDetails describes a route window for an incident.
func windowDetails(window *store.RouteWindow, extra map[string]any) map[string]any {
	details := map[string]any{"requests": int64(0), "no_backend": int64(0)}
	if window != nil {
		details["requests"] = window.Requests
		details["no_backend"] = window.NoBackend
	}
	for key, value := range extra {
		details[key] = value
	}
	return details
}

// appIDOf returns the app ID of an app, nil when it is unknown.
func appIDOf(app *discovery.App) *int64 {
	if app == nil || app.ID == 0 {
		return nil
	}
	id := app.ID
	return &id
}

// alertsOf reports whether messages are allowed for an app; an app that is
// not known keeps its incidents quiet.
func alertsOf(app *discovery.App) bool {
	return app != nil && app.Alerts
}

// idOf returns a pointer to an ID.
func idOf(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}
