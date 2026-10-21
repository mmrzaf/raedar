package rules

import "time"

// The fixed parameters of the rules. They define what each rule means and are
// documented with it in docs/alerts.md; what operators may tune lives in the
// configuration.
const (
	// routeDownWindow and routeDownMinRequests: route.down looks at the last
	// two minutes and needs at least this many requests, half of them without
	// a backend.
	routeDownWindow      = 2 * time.Minute
	routeDownMinRequests = 3

	// routeErrorsWindow, routeErrorsOpenAfter and routeErrorsMinErrors:
	// route.errors looks at the last five minutes, needs this many 5xx
	// responses and a condition that holds for two minutes.
	routeErrorsWindow    = 5 * time.Minute
	routeErrorsOpenAfter = 2 * time.Minute
	routeErrorsMinErrors = 5

	// routeSlowWindow, routeSlowOpenAfter and routeSlowPathMinRequests:
	// route.slow looks at the last ten minutes, holds for five, and names the
	// slowest path among those with this many requests.
	routeSlowWindow          = 10 * time.Minute
	routeSlowOpenAfter       = 5 * time.Minute
	routeSlowPathMinRequests = 10

	// slotSampleWindow is how far back container samples are read.
	slotSampleWindow = 10 * time.Minute

	// unhealthyOpenAfter and memoryOpenAfter are how long a failing health
	// check and a container near its memory limit must persist.
	unhealthyOpenAfter = 2 * time.Minute
	memoryOpenAfter    = 10 * time.Minute

	// crashLoopResolveAfter is how long container.restarting stays open
	// after the crashes stop. The window and count thresholds are
	// configurable through RAEDAR_CRASH_LOOP_WINDOW / _CRASHES.
	crashLoopResolveAfter = 10 * time.Minute

	// crashLookback is how far back crashes count for container.stopped, which
	// opens after stoppedOpenAfter without the container running.
	crashLookback    = 30 * time.Minute
	stoppedOpenAfter = time.Minute

	// diskHistory is how much filesystem history is read. The forecast needs
	// diskForecastMinHistory of it and a filesystem at least
	// diskForecastMinUsage percent full, so that growth on a mostly empty disk
	// does not alert.
	diskHistory            = 24 * time.Hour
	diskForecastMinHistory = 12 * time.Hour
	diskForecastMinUsage   = 50

	// hostMemoryOpenAfter and sourceFailedAfter are how long low host memory
	// and a failing source must persist.
	hostMemoryOpenAfter = 5 * time.Minute
	sourceFailedAfter   = 5 * time.Minute
)
