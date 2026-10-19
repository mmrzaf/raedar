package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// patternsAPI returns the heatmap, day profile and trend of one series.
func (s *Server) patternsAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	raw := query.Get("series")
	if raw == "" {
		badRequest(w, "series is required")
		return
	}
	ref, err := parseSeriesRef(raw)
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
	localNow := now.In(s.cfg.Location)
	from := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, s.cfg.Location).AddDate(0, 0, -7*weeks)
	model := s.model.Model()
	timeRange := store.Range{From: from, To: now, Points: int(now.Sub(from)/time.Hour) + 1}
	bins, unit, err := s.patternBins(request.Context(), model, ref, timeRange)
	if err != nil {
		s.seriesError(w, ref, err)
		return
	}
	patterns := aggregatePatterns(bins, s.cfg.Location, now, dailyAggregation(ref))
	patterns.Series = raw
	patterns.Unit = unit
	writeJSON(w, patterns)
}

// trendPoint is one day of the trend.
type trendPoint struct {
	// TS is the start of the local day.
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

// patterns is the answer of the patterns API.
type patterns struct {
	Series string `json:"series"`
	Unit   string `json:"unit"`
	// Heatmap is the average per weekday (Monday first) and hour of the local
	// day. Hours without data are nil, so the UI shows them as empty and not as
	// zero.
	Heatmap [][]*float64 `json:"heatmap"`
	// DayProfile is the average per hour of the local day over all bins, and
	// DayProfileRecent and DayProfilePrevious over the last seven days and the
	// time before them.
	DayProfile         []*float64 `json:"day_profile"`
	DayProfileRecent   []*float64 `json:"day_profile_recent"`
	DayProfilePrevious []*float64 `json:"day_profile_previous"`
	// Trend is one value per local day, combined as daily says.
	Trend []trendPoint `json:"trend"`
}

// hourAverages accumulates values per hour of the day.
type hourAverages struct {
	sum   [24]float64
	count [24]int
}

// add records a value for an hour.
func (h *hourAverages) add(hour int, value float64) {
	h.sum[hour] += value
	h.count[hour]++
}

// profile returns the average of every hour, nil for hours without values.
func (h *hourAverages) profile() []*float64 {
	profile := make([]*float64, 24)
	for hour := range profile {
		if h.count[hour] > 0 {
			value := h.sum[hour] / float64(h.count[hour])
			profile[hour] = &value
		}
	}
	return profile
}

// dayTotals accumulates the values of one local day.
type dayTotals struct {
	start    time.Time
	sum, max float64
	count    int
}

// aggregatePatterns folds hourly bins into the heatmap, the day profiles and
// the trend. Hours and days are those of location. Bins newer than a week
// before now form the recent profile.
func aggregatePatterns(bins []patternBin, location *time.Location, now time.Time, daily aggregation) patterns {
	var weekdays [7]hourAverages
	var all, recent, previous hourAverages
	days := make(map[string]*dayTotals)
	recentCutoff := now.Add(-7 * 24 * time.Hour)
	for _, bin := range bins {
		local := bin.at.In(location)
		weekday := int(local.Weekday()+6) % 7
		hour := local.Hour()
		weekdays[weekday].add(hour, bin.value)
		all.add(hour, bin.value)
		if bin.at.After(recentCutoff) {
			recent.add(hour, bin.value)
		} else {
			previous.add(hour, bin.value)
		}
		key := local.Format("2006-01-02")
		day := days[key]
		if day == nil {
			day = &dayTotals{start: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)}
			days[key] = day
		}
		day.sum += bin.value
		day.count++
		day.max = max(day.max, bin.value)
	}
	result := patterns{
		Heatmap:            make([][]*float64, 7),
		DayProfile:         all.profile(),
		DayProfileRecent:   recent.profile(),
		DayProfilePrevious: previous.profile(),
		Trend:              make([]trendPoint, 0, len(days)),
	}
	for weekday := range weekdays {
		result.Heatmap[weekday] = weekdays[weekday].profile()
	}
	keys := make([]string, 0, len(days))
	for key := range days {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		day := days[key]
		value := day.sum
		switch daily {
		case aggregateMax:
			value = day.max
		case aggregateMean:
			value = day.sum / float64(day.count)
		}
		result.Trend = append(result.Trend, trendPoint{TS: day.start.Unix(), Value: value})
	}
	return result
}

// patternBin is one hourly value of a series.
type patternBin struct {
	at    time.Time
	value float64
}

// patternBins reads the hourly values of one series and returns them with its
// unit. The recent window comes from the minute tables, so patterns
// are useful before the first hourly rollups have run; older days come from
// the hourly tables.
func (s *Server) patternBins(ctx context.Context, model *discovery.Model, ref seriesRef, timeRange store.Range) ([]patternBin, string, error) {
	boundary := timeRange.From
	if recent := timeRange.To.Add(-min(s.cfg.Retention.Minute, store.MinuteRange)); recent.After(boundary) {
		// Minute data is binned from the boundary, so it must be a local hour
		// start for the bins to be local hours.
		boundary = store.TruncateLocalHour(recent, s.cfg.Location).Add(time.Hour)
	}
	var (
		bins []patternBin
		unit = "count"
	)
	read := func(window store.Range, source store.RangeSource) error {
		answer, err := s.series(ctx, model, ref, window, source, time.Hour)
		if err != nil {
			return err
		}
		if len(answer.Points) > 0 {
			unit = answer.Unit
		}
		bins = append(bins, patternPoints(answer.Points)...)
		return nil
	}
	if timeRange.From.Before(boundary) {
		if err := read(store.Range{From: timeRange.From, To: boundary}, store.SourceHours); err != nil {
			return nil, "", err
		}
	}
	if boundary.Before(timeRange.To) {
		if err := read(store.Range{From: boundary, To: timeRange.To}, store.SourceMinutes); err != nil {
			return nil, "", err
		}
	}
	sort.Slice(bins, func(i, j int) bool { return bins[i].at.Before(bins[j].at) })
	return bins, unit, nil
}

// patternPoints converts series points into pattern bins.
func patternPoints(points []seriesPoint) []patternBin {
	bins := make([]patternBin, 0, len(points))
	for _, point := range points {
		if point.Value == nil {
			continue
		}
		bins = append(bins, patternBin{at: time.Unix(point.TS, 0).UTC(), value: *point.Value})
	}
	return bins
}

// patternsPage renders the patterns page.
func (s *Server) patternsPage(w http.ResponseWriter, request *http.Request) {
	subjects := s.patternSubjects()
	selected := subjects[0]
	for _, subject := range subjects {
		if subject.Value() == request.URL.Query().Get("subject") {
			selected = subject
		}
	}
	metrics := patternMetricNames(selected.Kind)
	metric := request.URL.Query().Get("metric")
	if !containsString(metrics, metric) {
		metric = metrics[0]
	}
	weeks := parseInt(request.URL.Query().Get("weeks"), 4)
	if weeks < 1 || weeks > 52 {
		weeks = 4
	}
	ref := seriesRef{Kind: selected.Kind, Subject: selected.Subject, Metric: metric}
	s.render(w, "patterns.html", map[string]any{
		"Page": pageData{Title: "Patterns", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version,
			Sources: s.sourceViews(request), Nav: "patterns", Location: s.cfg.Location},
		"Subjects": subjects, "Metrics": metrics, "Subject": selected.Value(), "Metric": metric,
		"Weeks": weeks, "Series": ref.String(),
	})
}

// patternSubject is one choice in the patterns picker.
type patternSubject struct {
	// Label is the shown label.
	Label string
	// Kind is what the subject is.
	Kind subjectKind
	// Subject identifies it within its kind, as in a seriesRef.
	Subject string
}

// Value is the form value of the subject, kind/subject with the subject
// escaped.
func (p patternSubject) Value() string {
	return string(p.Kind) + "/" + url.PathEscape(p.Subject)
}

// patternSubjects returns the subjects the patterns page can chart.
func (s *Server) patternSubjects() []patternSubject {
	model := s.model.Model()
	var subjects []patternSubject
	if model != nil {
		for _, name := range sortedKeys(model.Apps) {
			app := model.Apps[name]
			subjects = append(subjects, patternSubject{Label: "app: " + name, Kind: subjectApp, Subject: name})
			for _, routeName := range sortedKeys(app.Routes) {
				subjects = append(subjects, patternSubject{Label: "route: " + routeName, Kind: subjectRoute, Subject: routeName})
			}
			for _, slotKey := range sortedKeys(app.Slots) {
				slot := app.Slots[slotKey]
				if slot.ID == 0 {
					continue
				}
				subjects = append(subjects, patternSubject{
					Label:   "slot: " + name + " / " + slotKey,
					Kind:    subjectSlot,
					Subject: strconv.FormatInt(slot.ID, 10),
				})
			}
		}
	}
	subjects = append(subjects, patternSubject{Label: "host: " + s.cfg.HostName, Kind: subjectHost})
	for _, disk := range s.cfg.Host.Disks {
		subjects = append(subjects, patternSubject{Label: "disk: " + disk.Name, Kind: subjectDisk, Subject: disk.Name})
	}
	return subjects
}

// containsString reports whether value is in values.
func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
