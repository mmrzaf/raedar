package web

import (
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

func TestLatencyText(t *testing.T) {
	var slow, empty store.Histogram
	slow.Add(5 * store.HistogramBound)
	tests := []struct {
		name      string
		histogram *store.Histogram
		want      string
	}{
		{name: "overflow bucket", histogram: &slow, want: ">60 s"},
		{name: "no requests", histogram: &empty, want: "—"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentileOf(tt.histogram, 95).String(); got != tt.want {
				t.Errorf("percentileOf().String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLatencyKnown(t *testing.T) {
	if got := (latency{MS: 63, Known: true}).String(); got != "63 ms" {
		t.Errorf("String() = %q, want 63 ms", got)
	}
}

func TestLatencyScaling(t *testing.T) {
	tests := map[float64]string{0: "0 ms", 63: "63 ms", 999: "999 ms", 1500: "1.5 s", 12000: "12 s", 60000: "60 s"}
	for ms, want := range tests {
		if got := latencyText(ms); got != want {
			t.Errorf("latencyText(%v) = %q, want %q", ms, got, want)
		}
	}
}

func TestCPUAndRateText(t *testing.T) {
	if got := cpuText(486); got != "0.49 cores" {
		t.Errorf("cpuText(486) = %q, want 0.49 cores", got)
	}
	if got := cpuText(2500); got != "2.50 cores" {
		t.Errorf("cpuText(2500) = %q, want 2.50 cores", got)
	}
	tests := map[float64]string{0: "0/min", 0.3: "0.3/min", 9.94: "9.9/min", 42: "42/min"}
	for rate, want := range tests {
		if got := rateText(rate); got != want {
			t.Errorf("rateText(%v) = %q, want %q", rate, got, want)
		}
	}
}

func TestDurationText(t *testing.T) {
	tests := map[time.Duration]string{
		30 * time.Second:               "less than a minute",
		5 * time.Minute:                "5 min",
		2*time.Hour + 5*time.Minute:    "2 h 5 min",
		3 * time.Hour:                  "3 h",
		26 * time.Hour:                 "1 d 2 h",
		48 * time.Hour:                 "2 d",
		27*24*time.Hour + 19*time.Hour: "27 d 19 h",
	}
	for duration, want := range tests {
		if got := durationText(duration); got != want {
			t.Errorf("durationText(%s) = %q, want %q", duration, got, want)
		}
	}
}

func TestSilenceKindLabel(t *testing.T) {
	tests := map[store.SilenceKind]string{
		store.SilenceMaintenanceWindow: "maintenance window",
		store.SilenceOneShotMute:       "one-shot mute",
		store.SilenceAutoDeploy:        "auto deploy",
		"custom_kind":                  "custom kind",
	}
	for kind, want := range tests {
		if got := silenceKindLabel(kind); got != want {
			t.Errorf("silenceKindLabel(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestForecastAndPlural(t *testing.T) {
	tests := map[float64]string{0.2: "1 day", 1: "1 day", 1.4: "1 day", 2: "2 days", 364: "364 days", 5000: "over a year"}
	for days, want := range tests {
		if got := forecastText(days); got != want {
			t.Errorf("forecastText(%v) = %q, want %q", days, got, want)
		}
	}
	if got := plural(0, "restart"); got != "0 restarts" {
		t.Errorf("plural(0) = %q", got)
	}
	if got := plural(1, "restart"); got != "1 restart" {
		t.Errorf("plural(1) = %q", got)
	}
}

func TestMeterLevel(t *testing.T) {
	tests := map[float64]string{0: "ok", 74.9: "ok", 75: "warn", 89.9: "warn", 90: "crit", 100: "crit"}
	for percent, want := range tests {
		if got := meterLevel(percent); got != want {
			t.Errorf("meterLevel(%v) = %q, want %q", percent, got, want)
		}
	}
}

func TestPercentOf(t *testing.T) {
	if got := percentOf(25, 200); got != 12.5 {
		t.Errorf("percentOf(25, 200) = %v, want 12.5", got)
	}
	if got := percentOf(5, 0); got != 0 {
		t.Errorf("percentOf(5, 0) = %v, want 0 for an unknown total", got)
	}
}

func TestSummarize(t *testing.T) {
	ok := appView{Status: healthOK}
	tests := []struct {
		name      string
		incidents []incidentView
		apps      []appView
		want      appHealth
		title     string
	}{
		{name: "quiet", apps: []appView{ok, ok}, want: healthOK, title: "All systems normal"},
		{name: "critical wins", incidents: []incidentView{{Severity: store.SeverityWarning}, {Severity: store.SeverityCritical}},
			apps: []appView{ok}, want: healthCritical, title: "1 critical incident open"},
		{name: "warnings", incidents: []incidentView{{Severity: store.SeverityWarning}, {Severity: store.SeverityWarning}},
			apps: []appView{ok}, want: healthWarning, title: "2 incidents open"},
		{name: "an app with a problem and no incident", apps: []appView{ok, {Status: healthWarning}}, want: healthWarning, title: "An app needs attention"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarize(tt.incidents, tt.apps)
			if got.State != tt.want || got.Title != tt.title {
				t.Errorf("summarize() = %+v, want %q %q", got, tt.want, tt.title)
			}
		})
	}
}
