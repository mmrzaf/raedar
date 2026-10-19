// Package sourcestatus records how a data source is doing: working, working
// with errors, or unavailable. Every source reports through a Reporter, so the
// state shown in the interface and checked by the rules comes from one place.
package sourcestatus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
)

// Recorder stores the state of a source.
type Recorder interface {
	// RecordSourceStatus stores the status of a source and its last error.
	RecordSourceStatus(ctx context.Context, source, status, message string, at time.Time) error
}

// Reporter records the status of one source whenever it changes.
type Reporter struct {
	source   string
	recorder Recorder
	clock    clock.Clock
	logger   *slog.Logger

	mu      sync.Mutex
	status  string
	message string
}

// New returns a reporter for a source.
func New(source string, recorder Recorder, c clock.Clock, logger *slog.Logger) *Reporter {
	return &Reporter{source: source, recorder: recorder, clock: c, logger: logger}
}

// Report records status and the error behind it, when either differs from the
// last report. cause is nil for a source that works.
func (r *Reporter) Report(ctx context.Context, status string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	r.mu.Lock()
	changed := r.status != status || r.message != message
	r.status = status
	r.message = message
	r.mu.Unlock()
	if !changed {
		return
	}
	if err := r.recorder.RecordSourceStatus(ctx, r.source, status, message, r.clock.Now()); err != nil {
		r.logger.Warn("record source status", slog.String("source", r.source), slog.String("error", err.Error()))
	}
}

// Status returns the last reported status, empty before the first report.
func (r *Reporter) Status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}
