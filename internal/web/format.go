package web

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// maxForecastDays is the longest disk forecast worth showing; beyond it the
// linear extrapolation says nothing useful.
const maxForecastDays = 365

// latency is one percentile of request durations. Over is set when the value
// fell into the open-ended last histogram bucket, so only a lower bound is
// known; Known is false when there were no requests at all.
type latency struct {
	MS    float64 `json:"ms"`
	Over  bool    `json:"over"`
	Known bool    `json:"known"`
}

// percentileOf returns the percentile of a histogram.
func percentileOf(histogram *store.Histogram, percent float64) latency {
	if histogram.Count() == 0 {
		return latency{}
	}
	value, over := histogram.Percentile(percent)
	return latency{MS: value, Over: over, Known: true}
}

// String formats the latency for display.
func (l latency) String() string {
	switch {
	case !l.Known:
		return "—"
	case l.Over:
		return ">" + latencyText(l.MS)
	default:
		return latencyText(l.MS)
	}
}

// latencyText formats a duration given in milliseconds.
func latencyText(ms float64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%.0f ms", ms)
	case ms < 10000:
		return fmt.Sprintf("%.1f s", ms/1000)
	default:
		return fmt.Sprintf("%.0f s", ms/1000)
	}
}

// cpuText formats CPU use given in millicores as cores.
func cpuText(millicores float64) string {
	return fmt.Sprintf("%.2f cores", millicores/1000)
}

// rateText formats a request rate given per minute.
func rateText(perMinute float64) string {
	switch {
	case perMinute <= 0:
		return "0/min"
	case perMinute < 10:
		return fmt.Sprintf("%.1f/min", perMinute)
	default:
		return fmt.Sprintf("%.0f/min", perMinute)
	}
}

// forecastText formats the days left until a disk is full.
func forecastText(days float64) string {
	if days > maxForecastDays {
		return "over a year"
	}
	return plural(int(math.Round(math.Max(days, 1))), "day")
}

// plural formats a count with a noun, adding an s for anything but one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// relativeTime returns a compact relative time such as "3 h ago".
func relativeTime(now, at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	delta := now.Sub(at)
	if delta < 0 {
		delta = 0
	}
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%d min ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(delta.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(delta.Hours()/24))
	}
}

// durationText returns a compact duration.
func durationText(d time.Duration) string {
	if d <= 0 {
		return "0 min"
	}
	if d < time.Minute {
		return "less than a minute"
	}
	minutes := int(d.Round(time.Minute) / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%d min", minutes)
	}
	hours := minutes / 60
	minutes -= hours * 60
	if minutes == 0 {
		return fmt.Sprintf("%d h", hours)
	}
	return fmt.Sprintf("%d h %d min", hours, minutes)
}

// bytesText formats a byte count.
func bytesText(value int64) string {
	number := float64(value)
	if number <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	unit := 0
	for number >= 1024 && unit < len(units)-1 {
		number /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%.0f %s", number, units[unit])
	}
	return fmt.Sprintf("%.1f %s", number, units[unit])
}

// countText formats a count with a k suffix.
func countText(value int64) string {
	if value >= 1000 {
		return fmt.Sprintf("%.1fk", float64(value)/1000)
	}
	return strconv.FormatInt(value, 10)
}

// meterLevel classifies a usage percentage for a meter: ok below 75, warn
// below 90, crit from there.
func meterLevel(percent float64) string {
	switch {
	case percent >= 90:
		return "crit"
	case percent >= 75:
		return "warn"
	default:
		return "ok"
	}
}
