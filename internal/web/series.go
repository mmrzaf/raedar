package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// maxSeriesPerRequest bounds how many series one request may ask for.
const maxSeriesPerRequest = 20

// subjectKind is what a series measures.
type subjectKind string

const (
	subjectApp   subjectKind = "app"
	subjectRoute subjectKind = "route"
	subjectSlot  subjectKind = "slot"
	subjectHost  subjectKind = "host"
	subjectDisk  subjectKind = "disk"
)

// valid reports whether k is one of the known kinds.
func (k subjectKind) valid() bool {
	switch k {
	case subjectApp, subjectRoute, subjectSlot, subjectHost, subjectDisk:
		return true
	}
	return false
}

// seriesRef names one series: a metric of a subject. The subject is the app,
// route or disk name, the slot ID for a slot, and empty for the host. The
// wire form is kind/subject/metric with every part percent-escaped, so names
// may contain any character.
type seriesRef struct {
	Kind    subjectKind
	Subject string
	Metric  string
}

// String returns the wire form of the reference.
func (r seriesRef) String() string {
	return strings.Join([]string{string(r.Kind), url.PathEscape(r.Subject), url.PathEscape(r.Metric)}, "/")
}

// errBadSeries marks a malformed or unknown series reference, which is the
// caller's mistake.
var errBadSeries = errors.New("bad series reference")

// errUnknownSubject marks a reference to an app, route, slot or disk that
// does not exist.
var errUnknownSubject = errors.New("unknown subject")

// parseSeriesRef parses the wire form of a series reference and checks the
// kind and the metric against the registry.
func parseSeriesRef(raw string) (seriesRef, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 3 {
		return seriesRef{}, fmt.Errorf("%w %q: want kind/subject/metric", errBadSeries, raw)
	}
	subject, err := url.PathUnescape(parts[1])
	if err != nil {
		return seriesRef{}, fmt.Errorf("%w %q: %v", errBadSeries, raw, err)
	}
	metric, err := url.PathUnescape(parts[2])
	if err != nil {
		return seriesRef{}, fmt.Errorf("%w %q: %v", errBadSeries, raw, err)
	}
	ref := seriesRef{Kind: subjectKind(parts[0]), Subject: subject, Metric: metric}
	if !ref.Kind.valid() {
		return seriesRef{}, fmt.Errorf("%w %q: unknown kind %q", errBadSeries, raw, parts[0])
	}
	if !metricExists(ref.Kind, ref.Metric) {
		return seriesRef{}, fmt.Errorf("%w %q: %s has no metric %q", errBadSeries, raw, ref.Kind, ref.Metric)
	}
	return ref, nil
}

// aggregation is how a metric's hourly values combine into a day.
type aggregation int

const (
	// aggregateMean averages the values; right for rates, gauges and latency.
	aggregateMean aggregation = iota
	// aggregateSum adds the values; right for counts and traffic volumes.
	aggregateSum
	// aggregateMax takes the highest value; right for fill levels.
	aggregateMax
)

// metric is one measurable thing of a bin type: its name, unit, how to read it
// from a bin, how its hourly values combine into a day, and whether the
// patterns page offers it.
type metric[B any] struct {
	name    string
	unit    string
	daily   aggregation
	pattern bool
	// value returns the metric of a bin; ok is false when the bin holds no data
	// for it, for example the latency of a minute without requests.
	value func(B) (value float64, ok bool)
}

// always wraps a reading that every bin has.
func always[B any](read func(B) float64) func(B) (float64, bool) {
	return func(bin B) (float64, bool) { return read(bin), true }
}

// percentile returns the latency metric of a route bin at percent.
func percentile(percent float64) func(store.RouteBin) (float64, bool) {
	return func(bin store.RouteBin) (float64, bool) {
		latency := percentileOf(&bin.Latency, percent)
		return latency.MS, latency.Known
	}
}

// The metric registry: the single place that says what can be charted.
var (
	routeMetrics = []metric[store.RouteBin]{
		{name: "requests", unit: "count", daily: aggregateSum, pattern: true, value: always(func(b store.RouteBin) float64 { return float64(b.Requests) })},
		{name: "error_rate", unit: "percent", pattern: true, value: func(b store.RouteBin) (float64, bool) {
			if b.Requests == 0 {
				return 0, false
			}
			return 100 * float64(b.S5xx) / float64(b.Requests), true
		}},
		{name: "p95", unit: "ms", pattern: true, value: percentile(95)},
		{name: "p50", unit: "ms", pattern: true, value: percentile(50)},
		{name: "p99", unit: "ms", pattern: true, value: percentile(99)},
		{name: "2xx", unit: "count", daily: aggregateSum, value: always(func(b store.RouteBin) float64 { return float64(b.S2xx) })},
		{name: "3xx", unit: "count", daily: aggregateSum, value: always(func(b store.RouteBin) float64 { return float64(b.S3xx) })},
		{name: "4xx", unit: "count", daily: aggregateSum, value: always(func(b store.RouteBin) float64 { return float64(b.S4xx) })},
		{name: "5xx", unit: "count", daily: aggregateSum, value: always(func(b store.RouteBin) float64 { return float64(b.S5xx) })},
		{name: "bytes", unit: "bytes", daily: aggregateSum, value: always(func(b store.RouteBin) float64 { return float64(b.Bytes) })},
	}
	slotMetrics = []metric[store.SlotBin]{
		{name: "cpu", unit: "millicores", pattern: true, value: always(func(b store.SlotBin) float64 { return b.CPUAvg })},
		{name: "memory", unit: "bytes", pattern: true, value: always(func(b store.SlotBin) float64 { return b.MemAvg })},
		{name: "net_rx", unit: "bytes", daily: aggregateSum, pattern: true, value: always(func(b store.SlotBin) float64 { return float64(b.NetRx) })},
		{name: "net_tx", unit: "bytes", daily: aggregateSum, pattern: true, value: always(func(b store.SlotBin) float64 { return float64(b.NetTx) })},
		{name: "cpu_max", unit: "millicores", value: always(func(b store.SlotBin) float64 { return b.CPUMax })},
		{name: "memory_max", unit: "bytes", value: always(func(b store.SlotBin) float64 { return b.MemMax })},
	}
	hostMetrics = []metric[store.HostBin]{
		{name: "cpu", unit: "percent", pattern: true, value: always(func(b store.HostBin) float64 { return b.CPUAvg })},
		{name: "load1", unit: "load", pattern: true, value: always(func(b store.HostBin) float64 { return b.Load1Avg })},
		{name: "memory", unit: "bytes", pattern: true, value: always(func(b store.HostBin) float64 { return b.MemAvailableAvg })},
		{name: "swap", unit: "bytes", pattern: true, value: always(func(b store.HostBin) float64 { return b.SwapUsedAvg })},
		{name: "memory_min", unit: "bytes", value: always(func(b store.HostBin) float64 { return b.MemAvailableMin })},
	}
	diskMetrics = []metric[store.DiskBin]{
		{name: "used", unit: "percent", daily: aggregateMax, pattern: true, value: func(b store.DiskBin) (float64, bool) {
			if b.SizeBytes <= 0 {
				return 0, false
			}
			return 100 * float64(b.UsedBytesMax) / float64(b.SizeBytes), true
		}},
		{name: "inodes", unit: "count", daily: aggregateMax, pattern: true, value: always(func(b store.DiskBin) float64 { return float64(b.InodesUsedMax) })},
		{name: "used_bytes", unit: "bytes", daily: aggregateMax, value: always(func(b store.DiskBin) float64 { return float64(b.UsedBytesMax) })},
		{name: "free", unit: "bytes", value: always(func(b store.DiskBin) float64 { return float64(b.AvailBytesMin) })},
	}
)

// find returns the metric with this name.
func find[B any](metrics []metric[B], name string) (metric[B], bool) {
	for _, candidate := range metrics {
		if candidate.name == name {
			return candidate, true
		}
	}
	return metric[B]{}, false
}

// metricExists reports whether the kind has a metric with this name.
func metricExists(kind subjectKind, name string) bool {
	switch kind {
	case subjectApp, subjectRoute:
		_, ok := find(routeMetrics, name)
		return ok
	case subjectSlot:
		_, ok := find(slotMetrics, name)
		return ok
	case subjectHost:
		_, ok := find(hostMetrics, name)
		return ok
	case subjectDisk:
		_, ok := find(diskMetrics, name)
		return ok
	}
	return false
}

// patternMetricNames returns the metrics the patterns page offers for a kind,
// in display order.
func patternMetricNames(kind subjectKind) []string {
	var names []string
	collect := func(name string, offered bool) {
		if offered {
			names = append(names, name)
		}
	}
	switch kind {
	case subjectApp, subjectRoute:
		for _, m := range routeMetrics {
			collect(m.name, m.pattern)
		}
	case subjectSlot:
		for _, m := range slotMetrics {
			collect(m.name, m.pattern)
		}
	case subjectHost:
		for _, m := range hostMetrics {
			collect(m.name, m.pattern)
		}
	case subjectDisk:
		for _, m := range diskMetrics {
			collect(m.name, m.pattern)
		}
	}
	return names
}

// dailyAggregation returns how the hourly values of a metric combine into a day.
func dailyAggregation(ref seriesRef) aggregation {
	switch ref.Kind {
	case subjectApp, subjectRoute:
		m, _ := find(routeMetrics, ref.Metric)
		return m.daily
	case subjectSlot:
		m, _ := find(slotMetrics, ref.Metric)
		return m.daily
	case subjectHost:
		m, _ := find(hostMetrics, ref.Metric)
		return m.daily
	case subjectDisk:
		m, _ := find(diskMetrics, ref.Metric)
		return m.daily
	}
	return aggregateMean
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

// seriesPoint is one timestamped value. Value is nil when the bin has no data
// for the metric, for example the latency of a minute without requests, so
// charts show a gap instead of a misleading zero.
type seriesPoint struct {
	TS    int64
	Value *float64
}

// newSeriesPoint returns the point of a bin; valid reports whether the bin has data.
func newSeriesPoint(at time.Time, value float64, valid bool) seriesPoint {
	point := seriesPoint{TS: at.Unix()}
	if valid {
		point.Value = &value
	}
	return point
}

// MarshalJSON encodes the point as [timestamp, value] with null for a gap.
func (p seriesPoint) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{p.TS, p.Value})
}

// seriesAnswer is one series in the API response.
type seriesAnswer struct {
	// Series is the wire form of the reference that was asked for.
	Series string `json:"series"`
	// Metric is the metric name, for labeling.
	Metric string `json:"metric"`
	// Label names the subject: the slot key, app, route or disk name.
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

// seriesAPI returns the series named by the repeated series parameter.
func (s *Server) seriesAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	raw := query["series"]
	if len(raw) == 0 {
		badRequest(w, "series is required")
		return
	}
	if len(raw) > maxSeriesPerRequest {
		badRequest(w, fmt.Sprintf("at most %d series are allowed", maxSeriesPerRequest))
		return
	}
	timeRange, err := seriesRange(query)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	refs := make([]seriesRef, 0, len(raw))
	for _, item := range raw {
		ref, err := parseSeriesRef(item)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		refs = append(refs, ref)
	}
	source, bin := store.Resolution(timeRange, s.cfg.Retention.Minute, s.clock.Now())
	answer := seriesResponse{From: timeRange.From, To: timeRange.To, Bin: int64(bin / time.Second), Source: string(source)}
	model := s.model.Model()
	for _, ref := range refs {
		series, err := s.series(request.Context(), model, ref, timeRange, source, bin)
		if err != nil {
			s.seriesError(w, ref, err)
			return
		}
		answer.Series = append(answer.Series, series)
	}
	writeJSON(w, answer)
}

// seriesError answers a failed series read: 400 for a bad reference, 404 for
// an unknown subject, 500 for everything else.
func (s *Server) seriesError(w http.ResponseWriter, ref seriesRef, err error) {
	switch {
	case errors.Is(err, errBadSeries):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, errUnknownSubject):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.logger.Error("read series", "series", ref.String(), "error", err)
	http.Error(w, "cannot read the series", http.StatusInternalServerError)
}

// series reads one series.
func (s *Server) series(ctx context.Context, model *discovery.Model, ref seriesRef, timeRange store.Range, source store.RangeSource, bin time.Duration) (seriesAnswer, error) {
	answer := seriesAnswer{Series: ref.String(), Metric: ref.Metric, Label: ref.Subject, Points: []seriesPoint{}}
	switch ref.Kind {
	case subjectApp, subjectRoute:
		m, _ := find(routeMetrics, ref.Metric)
		answer.Unit = m.unit
		routeIDs, err := routeIDs(model, ref)
		if err != nil {
			return answer, err
		}
		bins, err := s.store.RouteSeries(ctx, routeIDs, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, ok := m.value(entry)
			answer.Points = append(answer.Points, newSeriesPoint(entry.TS, value, ok))
		}
	case subjectSlot:
		m, _ := find(slotMetrics, ref.Metric)
		answer.Unit = m.unit
		slot, err := resolveSlot(model, ref.Subject)
		if err != nil {
			return answer, err
		}
		answer.Label = slot.Key
		bins, err := s.store.SlotSeries(ctx, []int64{slot.ID}, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, ok := m.value(entry)
			answer.Points = append(answer.Points, newSeriesPoint(entry.TS, value, ok))
		}
	case subjectHost:
		m, _ := find(hostMetrics, ref.Metric)
		answer.Unit = m.unit
		answer.Label = s.cfg.HostName
		bins, err := s.store.HostSeries(ctx, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, ok := m.value(entry)
			answer.Points = append(answer.Points, newSeriesPoint(entry.TS, value, ok))
		}
	case subjectDisk:
		m, _ := find(diskMetrics, ref.Metric)
		answer.Unit = m.unit
		bins, err := s.store.DiskSeries(ctx, []string{ref.Subject}, timeRange, source, bin)
		if err != nil {
			return answer, err
		}
		for _, entry := range bins {
			value, ok := m.value(entry)
			answer.Points = append(answer.Points, newSeriesPoint(entry.TS, value, ok))
		}
	}
	return answer, nil
}

// routeIDs resolves the routes of an app or route reference.
func routeIDs(model *discovery.Model, ref seriesRef) ([]int64, error) {
	if model == nil {
		return nil, fmt.Errorf("no model")
	}
	if ref.Kind == subjectRoute {
		route, ok := model.Routes[ref.Subject]
		if !ok {
			return nil, fmt.Errorf("%w: route %q", errUnknownSubject, ref.Subject)
		}
		return []int64{route.ID}, nil
	}
	app, ok := model.Apps[ref.Subject]
	if !ok {
		return nil, fmt.Errorf("%w: app %q", errUnknownSubject, ref.Subject)
	}
	ids := make([]int64, 0, len(app.Routes))
	for _, route := range app.Routes {
		ids = append(ids, route.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// resolveSlot finds the slot whose ID is the subject of a slot reference.
func resolveSlot(model *discovery.Model, subject string) (*discovery.Slot, error) {
	if model == nil {
		return nil, fmt.Errorf("no model")
	}
	id, err := strconv.ParseInt(subject, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: slot %q is not a slot ID", errBadSeries, subject)
	}
	for _, app := range model.Apps {
		for _, slot := range app.Slots {
			if slot.ID == id {
				return slot, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: slot %d", errUnknownSubject, id)
}
