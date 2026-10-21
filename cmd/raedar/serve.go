package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mmrzaf/raedar/internal/aggregate"
	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/digest"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/heartbeat"
	"github.com/mmrzaf/raedar/internal/inbox"
	"github.com/mmrzaf/raedar/internal/memlimit"
	"github.com/mmrzaf/raedar/internal/notify"
	"github.com/mmrzaf/raedar/internal/rules"
	"github.com/mmrzaf/raedar/internal/source/accesslog"
	"github.com/mmrzaf/raedar/internal/source/containers"
	"github.com/mmrzaf/raedar/internal/source/host"
	"github.com/mmrzaf/raedar/internal/source/systemd"
	"github.com/mmrzaf/raedar/internal/source/tlscheck"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/web"
)

// cgroupRoot is where the cgroup filesystem is mounted inside the container.
const cgroupRoot = "/sys/fs/cgroup"

// procRoot is where the host's /proc is visible inside the container.
const procRoot = "/proc"

// lockCheckInterval is how often the instance lock is verified while running.
const lockCheckInterval = 30 * time.Second

// lockCheckTimeout bounds one verification of the instance lock.
const lockCheckTimeout = 10 * time.Second

// stopFlushTimeout bounds the flush of the open buckets on shutdown.
const stopFlushTimeout = 10 * time.Second

// runServe loads the configuration and runs the service until SIGTERM or
// SIGINT.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, "raedar: serve takes no arguments\n")
		return exitUsage
	}
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "raedar: invalid configuration:\n%v\n", err)
		return exitError
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := serve(ctx, cfg, logger); err != nil {
		logger.Error("stopped with error", slog.String("error", err.Error()))
		return exitError
	}
	return exitOK
}

// serve runs the startup sequence, starts every component and blocks until
// ctx is done or a component fails.
func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("starting", slog.String("version", version), slog.String("host", cfg.HostName))
	if limit, err := memlimit.Apply(cgroupRoot); err != nil {
		logger.Debug("container memory limit not detected", slog.String("reason", err.Error()))
	} else if limit > 0 {
		logger.Info("memory limit set", slog.Int64("bytes", limit))
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if err := st.Lock(ctx); err != nil {
		return err
	}
	if hours, err := st.RealignRollups(ctx, cfg.Location, cfg.Retention.Minute, time.Now().UTC()); err != nil {
		return err
	} else if hours > 0 {
		logger.Info("hourly rows rebuilt on local hour boundaries", slog.Int("hours", hours), slog.String("zone", cfg.Location.String()))
	}
	logger.Info("running")
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	components := &sync.WaitGroup{}
	failures := make(chan error, 1)
	start := func(run func(context.Context) error) {
		components.Add(1)
		go func() {
			defer components.Done()
			if err := run(runCtx); err != nil {
				select {
				case failures <- err:
				default:
				}
			}
		}()
	}
	self, err := os.Hostname()
	if err != nil {
		logger.Debug("cannot determine the container host name", slog.String("error", err.Error()))
	}
	connector := docker.NewConnector(cfg.Docker.Host)
	discoverer := discovery.New(cfg.Docker.Host, cfg.Docker.TraefikContainer, self, st, logger, clock.System())
	if err := discoverer.Load(ctx); err != nil {
		return err
	}
	aggregator := aggregate.New(st, accesslog.SourceName, logger, clock.System())
	accessLog := accesslog.New(accesslog.Options{
		Path:       cfg.Docker.AccessLogPath,
		DockerHost: cfg.Docker.Host,
	}, discoverer, st, aggregator, logger, clock.System())
	containerStats := containers.New(discoverer, connector, st, logger, clock.System())
	hostStats := host.New(cfg.Host.Disks, procRoot, connector, st, logger, clock.System())
	tlsChecks := tlscheck.New(cfg.Docker.TLSCheckAddr, discoverer, cfg.Docker.TLSDomains, st, logger,
		clock.System(), cfg.Rules.CertWarn)
	if enabled, err := systemd.Enabled(cfg.Host.Systemd, systemd.BusAvailable()); err != nil {
		return err
	} else if enabled {
		bus, err := systemd.Connect(systemd.BusSocket)
		if err != nil {
			if cfg.Host.Systemd == config.SystemdOn {
				return fmt.Errorf("connect to systemd: %w", err)
			}
			logger.Warn("systemd is unavailable", slog.String("error", err.Error()))
			recordStatus(ctx, st, systemd.SourceName, store.StatusUnavailable, err, logger)
		} else {
			defer bus.Close()
			start(systemd.New(bus, st, logger, clock.System(), cfg.Host.SystemdIgnore, cfg.Host.SystemdCritical).Run)
		}
	}
	notification, err := notify.New(cfg, st, discoverer, logger, clock.System())
	if err != nil {
		return err
	}
	engine := rules.New(st, discoverer, aggregator, cfg, logger, clock.System(), notification)
	webOptions := web.Options{Version: version, DevStatic: os.Getenv("RAEDAR_DEV_STATIC")}
	if len(cfg.Heartbeat.Peers) > 0 {
		webOptions.Heartbeat = heartbeat.NewReceiver(cfg.Heartbeat.Peers, cfg.Heartbeat.ReceiveToken,
			st, logger, clock.System())
	}
	webServer, err := web.New(st, cfg, connector, discoverer, logger, clock.System(), webOptions)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           webServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case failures <- fmt.Errorf("serve web: %w", err):
			default:
			}
		}
	}()
	start(func(ctx context.Context) error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	})
	if cfg.Notify.Telegram.Token != "" && len(cfg.Notify.Telegram.ChatIDs) > 0 {
		dispatcher := &inbox.Dispatcher{
			Store:    st,
			Notifier: notification,
			Logger:   logger,
			Host:     cfg.HostName,
		}
		source := inbox.NewSource(cfg, logger, clock.System(),
			func(ctx context.Context, updateID int64) (bool, error) {
				seen, err := st.HasProcessedUpdate(ctx, updateID)
				if err != nil {
					return false, err
				}
				if seen {
					return true, nil
				}
				if _, err := st.RecordProcessedUpdate(ctx, updateID); err != nil {
					return false, err
				}
				return false, nil
			})
		source.Dispatcher = dispatcher
		start(source.Run)
		// Prune inbox rows once an hour so the table stays tiny. Skip the
		// retention bookkeeping here: the inbox deliberately re-processes
		// recent updates after a restart, so we keep only what we already saw.
		start(func(ctx context.Context) error {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					if _, err := st.PruneProcessedUpdates(ctx, int64(time.Hour.Seconds()*24*2)); err != nil {
						logger.Warn("prune inbox", slog.String("error", err.Error()))
					}
				}
			}
		})
	}
	start(discoverer.Run)
	start(aggregator.Run)
	start(notification.Run)
	start(engine.Run)
	start(func(ctx context.Context) error { return maintenance(ctx, st, cfg, logger, notification) })
	if cfg.Heartbeat.URL != "" {
		client, err := notify.OutboundClient(cfg, cfg.UsesProxy(config.ProxyTargetHeartbeat))
		if err != nil {
			return err
		}
		sender := heartbeat.NewSender(cfg, client, logger, clock.System(), func(ctx context.Context) bool {
			return discoverer.Healthy() && databaseWritable(ctx, st, time.Now().UTC())
		})
		start(sender.Run)
	}
	start(accessLog.Run)
	start(containerStats.Run)
	start(hostStats.Run)
	start(tlsChecks.Run)
	start(func(ctx context.Context) error { return keepLock(ctx, st, logger) })
	var failure error
	select {
	case <-runCtx.Done():
		logger.Info("stopping")
	case failure = <-failures:
		logger.Error("a component failed, stopping", slog.String("error", failure.Error()))
	}
	stop()
	components.Wait()
	if failure != nil {
		// A failed component may mean that the instance lock was lost, and
		// another instance then owns the access log position; writing the
		// buffered buckets could count lines twice.
		return failure
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), stopFlushTimeout)
	defer cancel()
	if err := aggregator.Flush(flushCtx); err != nil {
		logger.Warn("flush access log buckets", slog.String("error", err.Error()))
	}
	return nil
}

// maxSampleAge is how old the newest host sample may be while the database
// still counts as accepting writes.
const maxSampleAge = 3 * time.Minute

// sampleReader reads the newest host sample.
type sampleReader interface {
	LatestHostSample(ctx context.Context) (*store.HostMinute, error)
}

// databaseWritable reports whether the database accepted writes recently. The
// host collector writes one sample every minute whatever the traffic, so a
// recent one proves both the connection and the write path, which the access
// log buckets cannot while a server is idle.
func databaseWritable(ctx context.Context, samples sampleReader, now time.Time) bool {
	sample, err := samples.LatestHostSample(ctx)
	return err == nil && sample != nil && now.Sub(sample.TS) <= maxSampleAge
}

// maintenanceHour is the local hour of the daily retention.
const maintenanceHour = 3

// maintenance runs the hourly rollup with baselines, the daily retention and
// the daily digest until ctx is done.
func maintenance(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger, notification *notify.Notifier) error {
	composer := digest.New(st, cfg, logger, clock.System())
	now := time.Now().UTC()
	nextRollup := nextRollupTime(now, cfg.Location)
	nextRetention := nextTimeOfDay(now, cfg.Location, maintenanceHour, 0)
	var nextDigest time.Time
	if cfg.Notify.DigestAt != nil {
		nextDigest = nextTimeOfDay(now, cfg.Location, cfg.Notify.DigestAt.Hour, cfg.Notify.DigestAt.Minute)
	}
	for {
		wait := nextRollup.Sub(now)
		if nextRetention.Before(now.Add(wait)) {
			wait = nextRetention.Sub(now)
		}
		if !nextDigest.IsZero() && nextDigest.Before(now.Add(wait)) {
			wait = nextDigest.Sub(now)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		now = time.Now().UTC()
		if !now.Before(nextRollup) {
			if err := st.Rollup(ctx, now, cfg.Location); err != nil {
				logger.Error("hourly rollup failed", slog.String("error", err.Error()))
			} else if err := st.ComputeBaselines(ctx, cfg.Location, cfg.Rules.ErrorsMinRequests, now); err != nil {
				logger.Error("baseline computation failed", slog.String("error", err.Error()))
			} else {
				logger.Debug("hourly rollup done")
			}
			nextRollup = nextRollupTime(now, cfg.Location)
		}
		if !now.Before(nextRetention) {
			policy := store.Retention{
				Minute: cfg.Retention.Minute,
				Paths:  cfg.Retention.Paths,
				Hour:   cfg.Retention.Hour,
				Events: cfg.Retention.Events,
			}
			if err := st.Retention(ctx, policy, now); err != nil {
				logger.Error("retention failed", slog.String("error", err.Error()))
			} else {
				logger.Info("retention done")
			}
			nextRetention = nextTimeOfDay(now, cfg.Location, maintenanceHour, 0)
		}
		if !nextDigest.IsZero() && !now.Before(nextDigest) {
			text, err := composer.Text(ctx)
			if err != nil {
				logger.Error("compose digest", slog.String("error", err.Error()))
			} else {
				notification.SendDigest(ctx, text)
			}
			nextDigest = nextTimeOfDay(now, cfg.Location, cfg.Notify.DigestAt.Hour, cfg.Notify.DigestAt.Minute)
		}
	}
}

// nextRollupTime returns the next hourly rollup: five minutes after the next
// local hour boundary.
func nextRollupTime(now time.Time, location *time.Location) time.Time {
	next := store.TruncateLocalHour(now, location).Add(5 * time.Minute)
	if !next.After(now) {
		next = next.Add(time.Hour)
	}
	return next
}

// nextTimeOfDay returns the next occurrence of a time of day in a location.
func nextTimeOfDay(now time.Time, location *time.Location, hour, minute int) time.Time {
	local := now.In(location)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, location)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// recordStatus records an unavailable source at startup.
func recordStatus(ctx context.Context, st *store.Store, source, status string, cause error, logger *slog.Logger) {
	if err := st.RecordSourceStatus(ctx, source, status, cause.Error(), clock.System().Now()); err != nil {
		logger.Warn("record source status", slog.String("source", source), slog.String("error", err.Error()))
	}
}

// keepLock verifies the instance lock every lockCheckInterval until ctx is
// done or the lock is lost.
func keepLock(ctx context.Context, st *store.Store, logger *slog.Logger) error {
	ticker := time.NewTicker(lockCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, lockCheckTimeout)
			err := st.KeepLock(checkCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil // stopping
				}
				return err
			}
		}
	}
}
