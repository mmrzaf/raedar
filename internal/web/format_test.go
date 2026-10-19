package web

import (
	"testing"

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
