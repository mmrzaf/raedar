package accesslog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/aggregate"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the access log in the source state.
const SourceName = "accesslog"

const (
	// pollInterval is how long file mode waits before it looks for new lines
	// again.
	pollInterval = 250 * time.Millisecond
	// retryBase and retryMax bound the delay before the source is tried again
	// after a failure.
	retryBase = time.Second
	retryMax  = 30 * time.Second
	// reconnectInterval is how long container mode waits while there is no
	// Traefik container to follow.
	reconnectInterval = 10 * time.Second
)

// Clock provides timers to the source, so that tests run without waiting.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the clock used outside tests.
func SystemClock() Clock {
	return systemClock{}
}

// systemClock is the clock of the running process.
type systemClock struct{}

// Now returns the current time in UTC.
func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

// After returns a channel that receives the current time after d.
func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// Model provides the parts of the host model the source needs.
type Model interface {
	// LookupRoute returns the route for a router name, creating it when it is
	// unknown.
	LookupRoute(ctx context.Context, routerName string) (discovery.RouteLookup, error)
	// TraefikContainer returns the detected Traefik container, nil when there
	// is none.
	TraefikContainer() *discovery.Container
}

// State stores and returns the reader position and the source state.
type State interface {
	// SourcePosition returns the saved reader position, nil when there is
	// none.
	SourcePosition(ctx context.Context, source string) ([]byte, error)
	// RecordSourceStatus records the state of one source.
	RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error
}

// Counter counts access log measurements.
type Counter interface {
	// Add counts one measurement.
	Add(measurement aggregate.Measurement)
	// SetPosition records how far the source has read; the position is
	// written with the next flush.
	SetPosition(position []byte)
}

// Options configure the source.
type Options struct {
	// Path is the access log file; when empty the log is read from the
	// Traefik container's output.
	Path string
	// DockerHost is the Docker API address used in container-output mode.
	DockerHost string
}

// Source reads the Traefik access log, in file mode or from the Traefik
// container's output, and turns every line into a measurement for the
// aggregator.
type Source struct {
	options   Options
	model     Model
	state     State
	counter   Counter
	logger    *slog.Logger
	clock     Clock
	pollEvery time.Duration

	mu        sync.Mutex
	connected *docker.Client
	status    string
	statusErr string
	lastTime  time.Time
}

// New returns an access log source.
func New(options Options, model Model, state State, counter Counter, logger *slog.Logger, clock Clock) *Source {
	return &Source{
		options:   options,
		model:     model,
		state:     state,
		counter:   counter,
		logger:    logger,
		clock:     clock,
		pollEvery: pollInterval,
		status:    store.StatusUnavailable,
	}
}

// Run reads the access log until ctx is done, retrying after failures.
func (s *Source) Run(ctx context.Context) error {
	delay := retryBase
	for ctx.Err() == nil {
		var err error
		if s.options.Path != "" {
			err = s.runFile(ctx)
		} else {
			err = s.runContainer(ctx)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.recordStatus(ctx, store.StatusUnavailable, err)
			s.logger.Warn("access log source failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.clock.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
	return nil
}

// runFile follows the access log file until ctx is done.
func (s *Source) runFile(ctx context.Context) error {
	position, err := s.loadFilePosition(ctx)
	if err != nil {
		return err
	}
	reader, err := openFollower(s.options.Path, position)
	if err != nil {
		return err
	}
	defer reader.close()
	s.recordStatus(ctx, store.StatusOK, nil)
	for ctx.Err() == nil {
		text, at, err := reader.next()
		switch {
		case errors.Is(err, errNoLine):
			if !s.wait(ctx, s.pollEvery) {
				return nil
			}
			continue
		case err != nil:
			return err
		}
		line, ok := parseLine([]byte(text))
		if !ok {
			s.logger.Debug("skipping a line that is not an access log entry")
			continue
		}
		if err := s.measure(ctx, line, marshalPosition(at)); err != nil {
			return err
		}
	}
	return nil
}

// runContainer follows the Traefik container's output until ctx is done.
func (s *Source) runContainer(ctx context.Context) error {
	if err := s.loadContainerPosition(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		traefik := s.model.TraefikContainer()
		if traefik == nil {
			s.recordStatus(ctx, store.StatusUnavailable, errors.New("no Traefik container found"))
			if !s.wait(ctx, reconnectInterval) {
				return nil
			}
			continue
		}
		client, err := s.client(ctx)
		if err != nil {
			return err
		}
		logs, err := client.Logs(ctx, traefik.ID, docker.LogOptions{
			Follow:     true,
			Timestamps: true,
			Since:      s.since(),
			TTY:        traefik.TTY,
		})
		if err != nil {
			return err
		}
		s.recordStatus(ctx, store.StatusOK, nil)
		readErr := s.readLogs(ctx, logs)
		logs.Close()
		if ctx.Err() != nil {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if !s.wait(ctx, retryBase) {
			return nil
		}
	}
	return nil
}

// readLogs reads the container's output line by line until the stream ends.
func (s *Source) readLogs(ctx context.Context, logs io.ReadCloser) error {
	reader := bufio.NewReader(logs)
	for {
		text, err := reader.ReadString('\n')
		if text != "" {
			s.handleContainerLine(ctx, strings.TrimRight(text, "\r\n"))
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read container logs: %w", err)
		}
	}
}

// handleContainerLine parses one container log line, which Docker prefixes
// with its own timestamp. Lines that are not access log entries, such as
// Traefik's own log output, are skipped.
func (s *Source) handleContainerLine(ctx context.Context, text string) {
	stamp, rest, found := strings.Cut(text, " ")
	if !found {
		return
	}
	when, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		s.logger.Debug("skipping a container log line without a timestamp")
		return
	}
	if !s.lastTime.IsZero() && !when.After(s.lastTime) {
		return // already read before the restart
	}
	s.lastTime = when
	position := marshalPosition(containerPosition{Time: when})
	line, ok := parseLine([]byte(rest))
	if !ok {
		s.counter.SetPosition(position)
		return
	}
	if err := s.measure(ctx, line, position); err != nil {
		s.logger.Warn("count access log line", slog.String("error", err.Error()))
	}
}

// measure counts one parsed line.
func (s *Source) measure(ctx context.Context, line Line, position []byte) error {
	route, err := s.model.LookupRoute(ctx, line.RouterName)
	if err != nil {
		return err
	}
	s.counter.Add(aggregate.Measurement{
		RouteID:      route.ID,
		Time:         line.Time,
		Method:       line.Method,
		Path:         line.Path,
		Status:       line.Status,
		NoBackend:    line.noBackend(),
		Bytes:        line.Bytes,
		LatencyMS:    float64(line.Duration) / float64(time.Millisecond),
		CountLatency: line.countLatency(route.LatencyIgnore),
	})
	s.counter.SetPosition(position)
	return nil
}

// client returns the connected Docker client, connecting when needed.
func (s *Source) client(ctx context.Context) (*docker.Client, error) {
	s.mu.Lock()
	client := s.connected
	s.mu.Unlock()
	if client != nil {
		return client, nil
	}
	client, err := docker.Connect(ctx, s.options.DockerHost)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.connected = client
	s.mu.Unlock()
	return client, nil
}

// since returns the time container logs are requested from: after the last
// read line, or from now when nothing was read yet.
func (s *Source) since() time.Time {
	if s.lastTime.IsZero() {
		return s.clock.Now()
	}
	return s.lastTime
}

// wait waits for d, reporting false when ctx is done.
func (s *Source) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.clock.After(d):
		return true
	}
}

// loadFilePosition returns the saved position when it is a file position.
func (s *Source) loadFilePosition(ctx context.Context) (*filePosition, error) {
	data, err := s.state.SourcePosition(ctx, SourceName)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var position filePosition
	if err := json.Unmarshal(data, &position); err != nil {
		return nil, nil
	}
	if position.Inode == 0 {
		return nil, nil
	}
	return &position, nil
}

// loadContainerPosition returns the saved position when it is a log
// timestamp.
func (s *Source) loadContainerPosition(ctx context.Context) error {
	data, err := s.state.SourcePosition(ctx, SourceName)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var position containerPosition
	if err := json.Unmarshal(data, &position); err != nil {
		return nil
	}
	s.lastTime = position.Time.UTC()
	return nil
}

// containerPosition is the reader position in container-output mode.
type containerPosition struct {
	Time time.Time `json:"time"`
}

// marshalPosition encodes a reader position.
func marshalPosition(position any) []byte {
	data, err := json.Marshal(position)
	if err != nil {
		return nil
	}
	return data
}

// recordStatus stores the state of the source when it changed.
func (s *Source) recordStatus(ctx context.Context, status string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	s.mu.Lock()
	changed := s.status != status || s.statusErr != message
	s.status = status
	s.statusErr = message
	s.mu.Unlock()
	if !changed {
		return
	}
	if err := s.state.RecordSourceStatus(ctx, SourceName, status, message, s.clock.Now()); err != nil {
		s.logger.Warn("record access log status", slog.String("error", err.Error()))
	}
}
