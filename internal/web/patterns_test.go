package web

import (
	"testing"
	"time"
)

func TestAggregatePatterns(t *testing.T) {
	tehran, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	// 2026-10-19 is a Monday. Bins start on local hours, so in Tehran they sit
	// at :30 UTC.
	monday := func(localHour int) time.Time {
		return time.Date(2026, 10, 19, localHour, 0, 0, 0, tehran).UTC()
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	bins := []patternBin{
		{at: monday(9), value: 10},
		{at: monday(9).Add(-7 * 24 * time.Hour), value: 30}, // the Monday before: older than a week
		{at: monday(23), value: 4},
		{at: monday(23).Add(time.Hour), value: 8}, // 00:00 on Tuesday, another day
	}
	got := aggregatePatterns(bins, tehran, now, aggregateMean)

	if len(got.Heatmap) != 7 || len(got.Heatmap[0]) != 24 {
		t.Fatalf("heatmap shape = %d x %d, want 7 x 24", len(got.Heatmap), len(got.Heatmap[0]))
	}
	if cell := got.Heatmap[0][9]; cell == nil || *cell != 20 {
		t.Errorf("Monday 09:00 = %v, want the average 20 of both Mondays", cell)
	}
	if cell := got.Heatmap[0][23]; cell == nil || *cell != 4 {
		t.Errorf("Monday 23:00 = %v, want 4", cell)
	}
	if cell := got.Heatmap[1][0]; cell == nil || *cell != 8 {
		t.Errorf("Tuesday 00:00 = %v, want 8: a bin after local midnight belongs to the next day", cell)
	}
	if got.Heatmap[0][10] != nil || got.Heatmap[3][9] != nil {
		t.Error("cells without data must be nil, not zero")
	}
	if p := got.DayProfileRecent[9]; p == nil || *p != 10 {
		t.Errorf("recent profile at 09 = %v, want 10 (only the latest Monday)", p)
	}
	if p := got.DayProfilePrevious[9]; p == nil || *p != 30 {
		t.Errorf("previous profile at 09 = %v, want 30", p)
	}
	if p := got.DayProfile[9]; p == nil || *p != 20 {
		t.Errorf("profile at 09 = %v, want 20", p)
	}
	if got.DayProfile[3] != nil {
		t.Error("an hour without data must be nil in the profile")
	}
	wantDays := []time.Time{
		time.Date(2026, 10, 12, 0, 0, 0, 0, tehran),
		time.Date(2026, 10, 19, 0, 0, 0, 0, tehran),
		time.Date(2026, 10, 20, 0, 0, 0, 0, tehran),
	}
	if len(got.Trend) != len(wantDays) {
		t.Fatalf("trend = %+v, want %d days", got.Trend, len(wantDays))
	}
	for i, day := range wantDays {
		if got.Trend[i].TS != day.Unix() {
			t.Errorf("trend[%d].TS = %d, want local midnight %d", i, got.Trend[i].TS, day.Unix())
		}
	}
	if got.Trend[1].Value != 7 {
		t.Errorf("mean of Monday = %v, want 7", got.Trend[1].Value)
	}
}

func TestAggregatePatternsDailyModes(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	bins := []patternBin{
		{at: time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC), value: 3},
		{at: time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC), value: 9},
	}
	tests := []struct {
		daily aggregation
		want  float64
	}{
		{aggregateMean, 6},
		{aggregateSum, 12},
		{aggregateMax, 9},
	}
	for _, tt := range tests {
		got := aggregatePatterns(bins, time.UTC, now, tt.daily)
		if len(got.Trend) != 1 || got.Trend[0].Value != tt.want {
			t.Errorf("trend for mode %d = %+v, want one day worth %v", tt.daily, got.Trend, tt.want)
		}
	}
}

func TestAggregatePatternsWithoutData(t *testing.T) {
	got := aggregatePatterns(nil, time.UTC, time.Now(), aggregateMean)
	if len(got.Trend) != 0 || len(got.Heatmap) != 7 {
		t.Errorf("aggregatePatterns(nil) = %+v, want an empty trend and a full empty heatmap", got)
	}
}

func TestDefaultPatternSubjectPrefersHost(t *testing.T) {
	subjects := []patternSubject{
		{Label: "app: shop", Kind: subjectApp, Subject: "shop"},
		{Label: "route: shop@docker", Kind: subjectRoute, Subject: "shop@docker"},
		{Label: "host: h", Kind: subjectHost},
	}
	got := defaultPatternSubject(subjects)
	if got.Kind != subjectHost {
		t.Fatalf("default subject kind = %q, want host", got.Kind)
	}
	// Without a host the first subject is still a sane fallback.
	got = defaultPatternSubject(subjects[:2])
	if got.Kind != subjectApp {
		t.Fatalf("fallback kind = %q, want app", got.Kind)
	}
}
