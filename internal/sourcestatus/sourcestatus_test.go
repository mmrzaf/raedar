package sourcestatus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
)

// recorded is one stored status.
type recorded struct {
	source, status, message string
}

// fakeRecorder keeps what was stored and can fail.
type fakeRecorder struct {
	stored []recorded
	err    error
}

func (f *fakeRecorder) RecordSourceStatus(ctx context.Context, source, status, message string, at time.Time) error {
	f.stored = append(f.stored, recorded{source, status, message})
	return f.err
}

func newReporter(recorder Recorder) *Reporter {
	return New("docker", recorder, clock.System(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestReportRecordsOnlyChanges(t *testing.T) {
	recorder := &fakeRecorder{}
	reporter := newReporter(recorder)
	ctx := context.Background()
	reporter.Report(ctx, "ok", nil)
	reporter.Report(ctx, "ok", nil)
	reporter.Report(ctx, "unavailable", errors.New("connection refused"))
	reporter.Report(ctx, "unavailable", errors.New("connection refused"))
	reporter.Report(ctx, "unavailable", errors.New("timeout"))
	reporter.Report(ctx, "ok", nil)
	want := []recorded{
		{"docker", "ok", ""},
		{"docker", "unavailable", "connection refused"},
		{"docker", "unavailable", "timeout"},
		{"docker", "ok", ""},
	}
	if len(recorder.stored) != len(want) {
		t.Fatalf("stored = %v, want %v", recorder.stored, want)
	}
	for i := range want {
		if recorder.stored[i] != want[i] {
			t.Errorf("stored[%d] = %v, want %v", i, recorder.stored[i], want[i])
		}
	}
	if got := reporter.Status(); got != "ok" {
		t.Errorf("Status() = %q, want ok", got)
	}
}

func TestReportSurvivesARecorderFailure(t *testing.T) {
	recorder := &fakeRecorder{err: errors.New("database down")}
	reporter := newReporter(recorder)
	reporter.Report(context.Background(), "ok", nil)
	if got := reporter.Status(); got != "ok" {
		t.Errorf("Status() = %q, want the reported status even when it could not be stored", got)
	}
}
