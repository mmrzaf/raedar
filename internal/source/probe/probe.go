package probe

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/sourcestatus"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the checker in the source state.
const SourceName = "probe"

// Model provides the routes and the address of Traefik's HTTPS entry point.
type Model interface {
	// Model returns the current discovery model.
	Model() *discovery.Model
	// TLSCheckAddr returns the detected address where Traefik serves HTTPS,
	// empty when it could not be detected.
	TLSCheckAddr() string
}

// Store keeps the verdicts and outcomes.
type Store interface {
	ProbeStates(ctx context.Context) (map[int64]store.ProbeState, error)
	SaveProbeState(ctx context.Context, state store.ProbeState) error
	AddProbeResult(ctx context.Context, result store.ProbeMinute) error
	DeleteProbeStatesExcept(ctx context.Context, keep []int64) error
	RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error
}

// Options are the checker's settings.
type Options struct {
	// Address is the configured Traefik HTTPS address; the detected one is used
	// when empty.
	Address string
	// UserAgent identifies the checks to the operator reading access logs.
	UserAgent string
	// Roots replaces the system roots; for tests.
	Roots *x509.CertPool
}

// Checker sends one HTTP check per eligible route on every interval.
type Checker struct {
	cfg    config.ProbeConfig
	opts   Options
	model  Model
	store  Store
	logger *slog.Logger
	status *sourcestatus.Reporter
	clock  clock.Clock
	// client is reused while the Traefik address stays the same.
	client     *Client
	clientAddr string
}

// New returns a checker.
func New(cfg config.ProbeConfig, opts Options, model Model, st Store, logger *slog.Logger, c clock.Clock) *Checker {
	return &Checker{
		cfg: cfg, opts: opts, model: model, store: st, logger: logger, clock: c,
		status: sourcestatus.New(SourceName, st, c, logger),
	}
}

// Run checks every interval until ctx is done. Within an interval the checks
// are spread out, and at most Concurrency run at once.
func (c *Checker) Run(ctx context.Context) error {
	for {
		started := c.clock.Now()
		c.cycle(ctx)
		wait := c.cfg.Interval - c.clock.Now().Sub(started)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.clock.After(wait):
		}
	}
}

// job is one route to check.
type job struct {
	route  *discovery.Route
	target Target
}

// jobs lists the eligible routes in a stable order.
func (c *Checker) jobs() []job {
	model := c.model.Model()
	if model == nil {
		return nil
	}
	names := make([]string, 0, len(model.Routes))
	for name := range model.Routes {
		names = append(names, name)
	}
	sort.Strings(names)
	var jobs []job
	for _, name := range names {
		route := model.Routes[name]
		if route.ID == 0 {
			continue
		}
		if target, reason := Plan(route, c.cfg.Path); reason == "" {
			jobs = append(jobs, job{route: route, target: target})
		}
	}
	return jobs
}

// address returns where Traefik serves HTTPS.
func (c *Checker) address() string {
	if c.opts.Address != "" {
		return c.opts.Address
	}
	return c.model.TLSCheckAddr()
}

// clientFor returns the client for an address. Only the checker's own
// goroutine calls it, so it needs no lock.
func (c *Checker) clientFor(address string) *Client {
	if c.client == nil || c.clientAddr != address {
		c.client = NewClient(address, c.opts.UserAgent, c.cfg.Timeout, c.opts.Roots)
		c.clientAddr = address
	}
	return c.client
}

// cycle runs one round of checks.
func (c *Checker) cycle(ctx context.Context) {
	jobs := c.jobs()
	keep := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		keep = append(keep, j.route.ID)
	}
	if err := c.store.DeleteProbeStatesExcept(ctx, keep); err != nil {
		c.logger.Warn("prune probe states", slog.String("error", err.Error()))
	}
	if len(jobs) == 0 {
		c.status.Report(ctx, store.StatusOK, nil)
		return
	}
	address := c.address()
	if address == "" {
		c.status.Report(ctx, store.StatusUnavailable, errors.New("no Traefik HTTPS address: set RAEDAR_TLS_CHECK_ADDR"))
		return
	}
	// A failing route and an unreachable Traefik look alike from here. Without
	// this check a Raedar network problem would open an incident for every
	// route, so when Traefik itself cannot be reached no verdict is changed.
	conn, err := (&net.Dialer{Timeout: c.cfg.Timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		if ctx.Err() == nil {
			c.status.Report(ctx, store.StatusUnavailable, fmt.Errorf("cannot connect to Traefik at %s", address))
		}
		return
	}
	_ = conn.Close()
	states, err := c.store.ProbeStates(ctx)
	if err != nil {
		c.status.Report(ctx, store.StatusDegraded, err)
		return
	}
	client := c.clientFor(address)
	spacing := time.Duration(0)
	if len(jobs) > 1 {
		spacing = c.cfg.Interval * 8 / 10 / time.Duration(len(jobs))
	}
	slots := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup
	var failures sync.Map
	for i, j := range jobs {
		if i > 0 && spacing > 0 {
			select {
			case <-ctx.Done():
			case <-c.clock.After(spacing):
			}
		}
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			checkCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
			outcome := client.Check(checkCtx, j.target)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err := c.record(ctx, j, states, outcome); err != nil {
				failures.Store(j.route.ID, err)
			}
		}()
	}
	wg.Wait()
	var recordErr error
	failures.Range(func(_, value any) bool { recordErr = value.(error); return false })
	if recordErr != nil {
		c.status.Report(ctx, store.StatusDegraded, recordErr)
		return
	}
	c.status.Report(ctx, store.StatusOK, nil)
}

// record stores one outcome and the new verdict. states is read-only here:
// every route appears once per cycle.
func (c *Checker) record(ctx context.Context, j job, states map[int64]store.ProbeState, outcome Outcome) error {
	now := c.clock.Now()
	var previous *store.ProbeState
	if state, ok := states[j.route.ID]; ok {
		previous = &state
	}
	next := Advance(previous, j.route.ID, j.target.String(), outcome, now)
	result := store.ProbeMinute{RouteID: j.route.ID, TS: now, Result: outcome.Result}
	if outcome.Status != 0 {
		latency := int(outcome.Latency / time.Millisecond)
		result.LatencyMS = &latency
	}
	if err := c.store.AddProbeResult(ctx, result); err != nil {
		return err
	}
	if previous != nil && previous.State != next.State {
		c.logger.Info("probe verdict changed", slog.String("route", j.route.Name),
			slog.String("from", previous.State), slog.String("to", next.State))
	}
	return c.store.SaveProbeState(ctx, next)
}
