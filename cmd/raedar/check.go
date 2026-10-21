package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/source/systemd"
	"github.com/mmrzaf/raedar/internal/store"
)

// checkStatus is the outcome of one check.
type checkStatus string

const (
	// statusOK is a check that passed.
	statusOK checkStatus = "ok"
	// statusWarn is a check that found a degraded setup.
	statusWarn checkStatus = "warn"
	// statusFail is a check that must be fixed.
	statusFail checkStatus = "fail"
)

// checkTimeout bounds the network checks.
const checkTimeout = 5 * time.Second

// report writes the check results and remembers failures.
type report struct {
	out    io.Writer
	failed bool
}

// add writes one check line.
func (r *report) add(status checkStatus, name, detail string) {
	fmt.Fprintf(r.out, "%-4s  %-26s %s\n", status, name, detail)
	if status == statusFail {
		r.failed = true
	}
}

// runCheck validates the configuration and every dependency and prints one
// line per check. It exits non-zero when a check fails.
func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, "raedar: check takes no arguments\n")
		return exitUsage
	}
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "raedar: invalid configuration:\n%v\n", err)
		return exitError
	}
	result := &report{out: stdout}
	result.add(statusOK, "configuration", fmt.Sprintf("valid; log level %s; time zone %s",
		cfg.LogLevel, cfg.Location))
	checkDatabase(ctx, cfg, result)
	checkDocker(ctx, cfg, result)
	checkAccessLog(ctx, cfg, result)
	checkSystemd(ctx, cfg, result)
	checkTLS(cfg, result)
	checkChannels(cfg, result)
	if result.failed {
		return exitError
	}
	return exitOK
}

// checkDatabase opens the store and reads the schema version.
func checkDatabase(ctx context.Context, cfg *config.Config, result *report) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opened, err := store.Dial(ctx, cfg.DatabaseURL)
	if err != nil {
		result.add(statusFail, "postgresql", err.Error())
		return
	}
	defer opened.Close()
	version, err := opened.SchemaVersion(ctx)
	if err != nil {
		result.add(statusFail, "postgresql", err.Error())
		return
	}
	result.add(statusOK, "postgresql", fmt.Sprintf("reachable; schema version %d", version))
}

// checkDocker checks the daemon version, the permission to read its disk
// usage and that containers can be listed.
func checkDocker(ctx context.Context, cfg *config.Config, result *report) {
	client, err := docker.Connect(ctx, cfg.Docker.Host)
	if err != nil {
		result.add(statusFail, "docker", err.Error())
		return
	}
	version, err := client.Version(ctx)
	if err != nil {
		result.add(statusFail, "docker", err.Error())
		return
	}
	containers, err := client.Containers(ctx)
	if err != nil {
		result.add(statusFail, "docker", err.Error())
		return
	}
	detail := fmt.Sprintf("API %s, daemon %s, %d containers", version.APIVersion, version.Version, len(containers))
	if _, err := client.DiskUsage(ctx); err != nil {
		result.add(statusWarn, "docker", detail+"; /system/df is not permitted, Docker disk usage stays empty")
		return
	}
	result.add(statusOK, "docker", detail+"; /system/df is permitted")
}

// checkAccessLog checks that the access log is readable and reports the
// stored position.
func checkAccessLog(ctx context.Context, cfg *config.Config, result *report) {
	position := ""
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opened, err := store.Dial(openCtx, cfg.DatabaseURL)
	if err == nil {
		if stored, err := opened.SourcePosition(ctx, "accesslog"); err == nil {
			position = string(stored)
		}
		opened.Close()
	}
	suffix := ""
	if position != "" {
		suffix = "; position " + position
	}
	path := cfg.Docker.AccessLogPath
	if path == "" {
		result.add(statusOK, "access log", "read from the Traefik container output"+suffix)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		result.add(statusFail, "access log", err.Error())
		return
	}
	if info.IsDir() {
		result.add(statusOK, "access log", fmt.Sprintf("directory %s%s", path, suffix))
		return
	}
	result.add(statusOK, "access log", fmt.Sprintf("file %s%s", path, suffix))
}

// checkSystemd checks the D-Bus socket and the reader.
func checkSystemd(ctx context.Context, cfg *config.Config, result *report) {
	enabled, err := systemd.Enabled(cfg.Host.Systemd, systemd.BusAvailable())
	if err != nil {
		result.add(statusFail, "systemd", err.Error())
		return
	}
	if !enabled {
		result.add(statusOK, "systemd", "disabled")
		return
	}
	bus, err := systemd.Connect(systemd.BusSocket)
	if err != nil {
		result.add(statusFail, "systemd", err.Error())
		return
	}
	bus.Close()
	result.add(statusOK, "systemd", "D-Bus socket reachable")
}

// checkTLS checks that the certificate endpoint accepts connections.
func checkTLS(cfg *config.Config, result *report) {
	if cfg.Docker.TLSCheckAddr == "" {
		result.add(statusWarn, "tls", "RAEDAR_TLS_CHECK_ADDR is not set; the address is detected from Traefik at runtime")
		return
	}
	connection, err := net.DialTimeout("tcp", cfg.Docker.TLSCheckAddr, checkTimeout)
	if err != nil {
		result.add(statusFail, "tls", err.Error())
		return
	}
	connection.Close()
	result.add(statusOK, "tls", cfg.Docker.TLSCheckAddr+" accepts connections")
}

// checkChannels lists the configured notification channels.
func checkChannels(cfg *config.Config, result *report) {
	configured := 0
	route := func(target string) string {
		if cfg.UsesProxy(target) {
			return "through the outbound proxy"
		}
		return "direct"
	}
	if cfg.Notify.Telegram.Token != "" && len(cfg.Notify.Telegram.ChatIDs) > 0 {
		configured++
		result.add(statusOK, "telegram", "configured, "+route(config.ProxyTargetTelegram))
	}
	if cfg.Notify.SMS.HTTP.URL != "" && len(cfg.Notify.SMS.To) > 0 {
		configured++
		result.add(statusOK, "sms", "configured (generic HTTP), "+route(config.ProxyTargetSMS))
	}
	if cfg.Notify.Webhook.URL != "" {
		configured++
		result.add(statusOK, "webhook", "configured, signed: "+boolText(cfg.Notify.Webhook.Secret != "")+", "+route(config.ProxyTargetWebhook))
	}
	if configured == 0 {
		result.add(statusWarn, "notifications", "no channel is configured")
		return
	}
	result.add(statusOK, "notifications", fmt.Sprintf("%d channels configured; send a message with raedar notify-test", configured))
}

// boolText formats a bool as yes or no.
func boolText(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
