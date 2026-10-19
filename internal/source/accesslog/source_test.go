package accesslog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceReplaysTheSampleLog(t *testing.T) {
	lines := sampleLines(t, "sample.log")
	path := writeFile(t, lines...)
	model := &testModel{routeID: 7, ignore: []string{"/ws", "/events"}}
	state := &testState{position: positionAtStart(t, path)}
	counter := &testCounter{}
	source := newSource(path, model, state, counter)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	waitFor(t, 10*time.Second, func() bool { return len(counter.counted()) >= len(lines) })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Every line of the sample is counted exactly once.
	measurements := counter.counted()
	if len(measurements) != len(lines) {
		t.Fatalf("measurements = %d, want %d", len(measurements), len(lines))
	}
	var requests, noBackend, ignoredLatency, upgrades int
	for _, measurement := range measurements {
		requests++
		if measurement.NoBackend {
			noBackend++
		}
		if !measurement.CountLatency {
			ignoredLatency++
			if measurement.Status == 101 {
				upgrades++
			}
		}
	}
	if requests != len(lines) {
		t.Errorf("requests = %d, want %d", requests, len(lines))
	}
	if noBackend != 4 {
		t.Errorf("no backend requests = %d, want 4", noBackend)
	}
	if upgrades != 1 {
		t.Errorf("protocol upgrades = %d, want 1", upgrades)
	}
	if ignoredLatency != 2 { // the /ws upgrade and /events
		t.Errorf("requests without latency = %d, want 2", ignoredLatency)
	}
	for _, measurement := range measurements {
		if measurement.RouteID != 7 {
			t.Fatalf("route = %d, want the route the model returned", measurement.RouteID)
		}
	}
	seen := model.seen()
	if len(seen) != len(lines) {
		t.Fatalf("route lookups = %d, want %d", len(seen), len(lines))
	}
	// A router name that matched no router arrives at the model empty.
	var empty int
	for _, name := range seen {
		if name == "" {
			empty++
		}
	}
	if empty != 1 {
		t.Errorf("unmatched routers = %d, want 1", empty)
	}
}

func TestSourceResumesWithoutLosingOrDoubleCounting(t *testing.T) {
	lines := sampleLines(t, "sample.log")
	first := lines[:len(lines)/2]
	path := writeFile(t, first...)
	model := &testModel{routeID: 3}
	state := &testState{position: positionAtStart(t, path)}
	counter := &testCounter{}

	firstSource := newSource(path, model, state, counter)
	runUntil(t, firstSource, counter, len(first))
	if got := len(counter.counted()); got != len(first) {
		t.Fatalf("measurements = %d, want %d", got, len(first))
	}
	saved := counter.lastPosition()
	if len(saved) == 0 {
		t.Fatal("no reader position was recorded")
	}

	// Simulate a restart: the saved position is loaded and the remaining
	// lines are appended.
	state.position = saved
	restarted := &testCounter{}
	secondSource := newSource(path, model, state, restarted)
	appendFile(t, path, lines[len(lines)/2:]...)
	runUntil(t, secondSource, restarted, len(lines)-len(first))

	measurements := restarted.counted()
	if len(measurements) != len(lines)-len(first) {
		t.Fatalf("measurements after the restart = %d, want %d", len(measurements), len(lines)-len(first))
	}
	// The first line after the restart is the first line after the saved
	// position: no line is lost and none is counted twice.
	want, ok := parseLine([]byte(lines[len(first)]))
	if !ok {
		t.Fatal("sample line does not parse")
	}
	if got := measurements[0].Time; !got.Equal(want.Time) {
		t.Errorf("first measurement after the restart = %v, want %v", got, want.Time)
	}
}

func TestSourceHandlesRenameRotation(t *testing.T) {
	lines := sampleLines(t, "sample.log")
	path := writeFile(t, lines[0])
	model := &testModel{routeID: 1}
	counter := &testCounter{}
	source := newSource(path, model, &testState{position: positionAtStart(t, path)}, counter)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	waitFor(t, 10*time.Second, func() bool { return len(counter.counted()) == 1 })
	// logrotate style: the file is renamed and a new one is created. A line
	// written to the rotated file before the new file has data is still
	// counted, before the new file's lines.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	appendFile(t, path+".1", lines[1])
	appendFile(t, path, lines[2], lines[3])
	waitFor(t, 10*time.Second, func() bool { return len(counter.counted()) == 4 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	measurements := counter.counted()
	for i, want := range []string{lines[0], lines[1], lines[2], lines[3]} {
		line, ok := parseLine([]byte(want))
		if !ok {
			t.Fatalf("sample line %d does not parse", i)
		}
		if !measurements[i].Time.Equal(line.Time) {
			t.Errorf("measurement %d time = %v, want %v", i, measurements[i].Time, line.Time)
		}
	}
}

func TestSourceHandlesTruncation(t *testing.T) {
	lines := sampleLines(t, "sample.log")
	path := writeFile(t, lines[0])
	model := &testModel{routeID: 1}
	counter := &testCounter{}
	source := newSource(path, model, &testState{position: positionAtStart(t, path)}, counter)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	waitFor(t, 10*time.Second, func() bool { return len(counter.counted()) == 1 })
	// copy-truncate style: the file shrinks and is refilled.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendFile(t, path, lines[1])
	waitFor(t, 10*time.Second, func() bool { return len(counter.counted()) == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	line, ok := parseLine([]byte(lines[1]))
	if !ok {
		t.Fatal("sample line does not parse")
	}
	if got := counter.counted()[1].Time; !got.Equal(line.Time) {
		t.Errorf("second measurement time = %v, want %v", got, line.Time)
	}
}

func TestSourceRecordsStatus(t *testing.T) {
	state := &testState{}
	source := newSource(filepath.Join(t.TempDir(), "missing.log"), &testModel{routeID: 1}, state, &testCounter{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	waitFor(t, 10*time.Second, func() bool {
		status, _ := state.statusOf()
		return status == "unavailable"
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, lastError := state.statusOf(); !strings.Contains(lastError, "open access log") {
		t.Errorf("last error = %q, want the missing file", lastError)
	}
}

// runUntil runs a source until it counted count measurements and stops it.
func runUntil(t *testing.T, source *Source, counter *testCounter, count int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	waitFor(t, 10*time.Second, func() bool { return len(counter.counted()) >= count })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not stop")
	}
}

func TestSourceReadsContainerOutput(t *testing.T) {
	lines := sampleLines(t, "sample.log")
	server := newLogsServer(t, lines)
	model := &testModel{
		routeID: 5,
		traefik: discoveryContainerWithID("traefik-container"),
	}
	state := &testState{}
	counter := &testCounter{}
	source := New(Options{DockerHost: server.host()}, model, state, counter, discardLogger(), &testClock{now: testBase})
	runUntil(t, source, counter, len(lines))
	if got := len(counter.counted()); got != len(lines) {
		t.Fatalf("measurements = %d, want %d", got, len(lines))
	}
	// The position is the Docker timestamp of the last line.
	position := counter.lastPosition()
	var saved containerPosition
	if err := json.Unmarshal(position, &saved); err != nil {
		t.Fatalf("decode position %s: %v", position, err)
	}
	if saved.Time.IsZero() {
		t.Errorf("position = %s, want the last log timestamp", position)
	}
}

func TestSourceSkipsTraefikLogLines(t *testing.T) {
	access := sampleLines(t, "sample.log")[0]
	others := []string{
		"2026-10-18T10:00:00.000000001Z 2026-10-18T10:00:00Z INF Starting provider *file.Provider",
		`2026-10-18T10:00:00.000000002Z {"level":"info","msg":"Configuration loaded"}`,
		"2026-10-18T10:00:00.000000003Z not json at all",
	}
	all := append(append([]string{}, others...), access)
	server := newLogsServer(t, all)
	model := &testModel{routeID: 1, traefik: discoveryContainerWithID("traefik-container")}
	counter := &testCounter{}
	source := New(Options{DockerHost: server.host()}, model, &testState{}, counter, discardLogger(), &testClock{now: testBase})
	runUntil(t, source, counter, 1)
	if got := len(counter.counted()); got != 1 {
		t.Fatalf("measurements = %d, want only the access log line", got)
	}
	// The position still advances over the skipped lines, so a restart does
	// not read them again.
	position := counter.lastPosition()
	var saved containerPosition
	if err := json.Unmarshal(position, &saved); err != nil {
		t.Fatalf("decode position: %v", err)
	}
	if want := testBase.Add(3 * time.Millisecond); !saved.Time.Equal(want) {
		t.Errorf("position time = %v, want the timestamp of the last line read (%v)", saved.Time, want)
	}
}

func TestSourceWaitsForTheTraefikContainer(t *testing.T) {
	state := &testState{}
	source := New(Options{DockerHost: "tcp://127.0.0.1:1"}, &testModel{routeID: 1}, state, &testCounter{}, discardLogger(), &testClock{now: testBase})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx) }()
	waitFor(t, 10*time.Second, func() bool {
		status, lastError := state.statusOf()
		return status == "unavailable" && strings.Contains(lastError, "Traefik")
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
