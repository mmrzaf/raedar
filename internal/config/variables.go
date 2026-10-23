package config

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// variables returns every supported environment variable with its parser,
// grouped by area: general, Docker and Traefik, host, rules, notifications,
// heartbeats and retention. RAEDAR_HOST_ROOT is applied before RAEDAR_DISKS,
// whose default is the host root.
func variables() []variable {
	return []variable{
		// General
		{name: "RAEDAR_DATABASE_URL", apply: applyDatabaseURL},
		{name: "RAEDAR_LISTEN", apply: parseVar(func(c *Config) *string { return &c.Listen }, checkedString(checkListenAddr))},
		{name: "RAEDAR_PUBLIC_URL", apply: parseVar(func(c *Config) *string { return &c.PublicURL }, parseHTTPURL)},
		{name: "RAEDAR_HOST_NAME", apply: parseVar(func(c *Config) *string { return &c.HostName }, checkedString(checkHostName))},
		{name: "RAEDAR_TZ", apply: parseVar(func(c *Config) **time.Location { return &c.Location }, parseLocation)},
		{name: "RAEDAR_LOG_LEVEL", apply: parseVar(func(c *Config) *slog.Level { return &c.LogLevel }, parseLogLevel)},

		// Docker and Traefik
		{name: "RAEDAR_DOCKER_HOST", apply: parseVar(func(c *Config) *string { return &c.Docker.Host }, checkedString(checkDockerHost))},
		{name: "RAEDAR_TRAEFIK_CONTAINER", apply: parseVar(func(c *Config) *string { return &c.Docker.TraefikContainer }, checkedString(checkContainerName))},
		{name: "RAEDAR_ACCESSLOG_PATH", apply: parseVar(func(c *Config) *string { return &c.Docker.AccessLogPath }, parseString)},
		{name: "RAEDAR_TLS_CHECK_ADDR", apply: parseVar(func(c *Config) *string { return &c.Docker.TLSCheckAddr }, checkedString(checkTLSCheckAddr))},
		{name: "RAEDAR_TLS_DOMAINS", apply: parseVar(func(c *Config) *[]string { return &c.Docker.TLSDomains }, parseListChecked(checkDomain))},

		// Host
		{name: "RAEDAR_HOST_ROOT", apply: parseVar(func(c *Config) *string { return &c.Host.Root }, checkedString(checkAbsolutePath))},
		{name: "RAEDAR_DISKS", apply: applyDisks},
		{name: "RAEDAR_SYSTEMD", apply: parseVar(func(c *Config) *SystemdMode { return &c.Host.Systemd }, parseSystemdMode)},
		{name: "RAEDAR_SYSTEMD_IGNORE", apply: parseVar(func(c *Config) *[]string { return &c.Host.SystemdIgnore }, parseListChecked(checkUnitPattern))},
		{name: "RAEDAR_SYSTEMD_CRITICAL", apply: parseVar(func(c *Config) *[]string { return &c.Host.SystemdCritical }, parseListChecked(checkUnitPattern))},

		// Rules
		{name: "RAEDAR_ERRORS_MIN_RATE", apply: parseVar(func(c *Config) *Percent { return &c.Rules.ErrorsMinRate }, parsePercent)},
		{name: "RAEDAR_ERRORS_FACTOR", apply: parseVar(func(c *Config) *float64 { return &c.Rules.ErrorsFactor }, parseFactor)},
		{name: "RAEDAR_ERRORS_MIN_REQUESTS", apply: parseVar(func(c *Config) *int { return &c.Rules.ErrorsMinRequests }, parseIntAtLeast(1))},
		{name: "RAEDAR_SLOW_MIN_P95", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.SlowMinP95 }, parseDuration)},
		{name: "RAEDAR_SLOW_FACTOR", apply: parseVar(func(c *Config) *float64 { return &c.Rules.SlowFactor }, parseFactor)},
		{name: "RAEDAR_SLOW_MIN_REQUESTS", apply: parseVar(func(c *Config) *int { return &c.Rules.SlowMinRequests }, parseIntAtLeast(1))},
		{name: "RAEDAR_LONG_REQUEST", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.LongRequest }, parseDuration)},
		{name: "RAEDAR_LONG_REQUEST_SHARE", apply: parseVar(func(c *Config) *Percent { return &c.Rules.LongRequestShare }, parsePercent)},
		{name: "RAEDAR_ROUTE_IGNORE", apply: parseVar(func(c *Config) *[]string { return &c.Rules.RouteIgnore }, parseListChecked(checkRoutePattern))},
		{name: "RAEDAR_MEMORY_LIMIT_WARN", apply: parseVar(func(c *Config) *Percent { return &c.Rules.MemoryLimitWarn }, parsePercent)},
		{name: "RAEDAR_DISK_WARN", apply: parseVar(func(c *Config) *Percent { return &c.Rules.DiskWarn }, parsePercent)},
		{name: "RAEDAR_DISK_CRIT", apply: parseVar(func(c *Config) *Percent { return &c.Rules.DiskCrit }, parsePercent)},
		{name: "RAEDAR_DISK_FULL_WITHIN", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.DiskFullWithin }, parseDuration)},
		{name: "RAEDAR_HOST_MEMORY_MIN_AVAILABLE", apply: parseVar(func(c *Config) *Percent { return &c.Rules.HostMemoryMinAvailable }, parsePercent)},
		{name: "RAEDAR_CERT_WARN", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.CertWarn }, parseDuration)},
		{name: "RAEDAR_CERT_CRIT", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.CertCrit }, parseDuration)},
		{name: "RAEDAR_RESOLVE_AFTER", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.ResolveAfter }, parseDuration)},
		{name: "RAEDAR_CRASH_LOOP_WINDOW", apply: parseVar(func(c *Config) *time.Duration { return &c.Rules.CrashLoopWindow }, parseDuration)},
		{name: "RAEDAR_CRASH_LOOP_CRASHES", apply: parseVar(func(c *Config) *int { return &c.Rules.CrashLoopCrashes }, parseIntAtLeast(2))},

		// Notifications
		{name: "RAEDAR_NOTIFY_WARNING", apply: parseVar(func(c *Config) *[]string { return &c.Notify.WarningChannels }, parseChannels)},
		{name: "RAEDAR_NOTIFY_CRITICAL", apply: parseVar(func(c *Config) *[]string { return &c.Notify.CriticalChannels }, parseChannels)},
		{name: "RAEDAR_DIGEST_AT", apply: applyDigestAt},
		{name: "RAEDAR_DIGEST_CHANNELS", apply: parseVar(func(c *Config) *[]string { return &c.Notify.DigestChannels }, parseChannels)},
		{name: "RAEDAR_RATE_LIMIT", apply: parseVar(func(c *Config) *int { return &c.Notify.RateLimit }, parseIntAtLeast(1))},
		{name: "RAEDAR_SMS_RATE_LIMIT", apply: parseVar(func(c *Config) *int { return &c.Notify.SMSRateLimit }, parseIntAtLeast(1))},
		{name: "RAEDAR_OUTBOUND_PROXY", apply: parseVar(func(c *Config) *string { return &c.Notify.OutboundProxy }, checkedString(checkProxyURL))},
		{name: "RAEDAR_OUTBOUND_PROXY_FOR", apply: parseVar(func(c *Config) *[]string { return &c.Notify.ProxyFor }, parseProxyFor)},
		{name: "RAEDAR_TELEGRAM_TOKEN", apply: parseVar(func(c *Config) *string { return &c.Notify.Telegram.Token }, parseString)},
		{name: "RAEDAR_TELEGRAM_CHAT_IDS", apply: parseVar(func(c *Config) *[]string { return &c.Notify.Telegram.ChatIDs }, parseListChecked(checkChatID))},
		{name: "RAEDAR_TELEGRAM_API_URL", apply: parseVar(func(c *Config) *string { return &c.Notify.Telegram.APIURL }, parseHTTPURL)},
		{name: "RAEDAR_SMS_TO", apply: parseVar(func(c *Config) *[]string { return &c.Notify.SMS.To }, parseListChecked(checkPhoneNumber))},
		{name: "RAEDAR_SMS_FROM", apply: parseVar(func(c *Config) *string { return &c.Notify.SMS.From }, parseString)},
		{name: "RAEDAR_SMS_HTTP_URL", apply: parseVar(func(c *Config) *string { return &c.Notify.SMS.HTTP.URL }, checkedString(checkSMSURL))},
		{name: "RAEDAR_SMS_HTTP_METHOD", apply: parseVar(func(c *Config) *string { return &c.Notify.SMS.HTTP.Method }, oneOfFold("GET", "POST"))},
		{name: "RAEDAR_SMS_HTTP_HEADERS", apply: parseVar(func(c *Config) *[]Header { return &c.Notify.SMS.HTTP.Headers }, parseHeaders)},
		{name: "RAEDAR_SMS_HTTP_BODY", apply: parseVar(func(c *Config) *string { return &c.Notify.SMS.HTTP.Body }, parseString)},
		{name: "RAEDAR_SMS_HTTP_ENCODING", apply: parseVar(func(c *Config) *string { return &c.Notify.SMS.HTTP.Encoding }, oneOfFold("json", "form", "raw"))},
		{name: "RAEDAR_SMS_HTTP_API_KEY", apply: parseVar(func(c *Config) *string { return &c.Notify.SMS.HTTP.APIKey }, parseString)},
		{name: "RAEDAR_WEBHOOK_URL", apply: parseVar(func(c *Config) *string { return &c.Notify.Webhook.URL }, parseHTTPURL)},
		{name: "RAEDAR_WEBHOOK_SECRET", apply: parseVar(func(c *Config) *string { return &c.Notify.Webhook.Secret }, parseString)},

		// Silencing (quiet hours, pager cooldown, stand-down, deploy silence,
		// per-rule cooldown overrides, subject naming).
		{name: "RAEDAR_QUIET_FROM", apply: parseVar(func(c *Config) **TimeOfDay { return &c.Notify.Silent.QuietFrom }, parseOptionalTimeOfDay)},
		{name: "RAEDAR_QUIET_TO", apply: parseVar(func(c *Config) **TimeOfDay { return &c.Notify.Silent.QuietTo }, parseOptionalTimeOfDay)},
		{name: "RAEDAR_QUIET_TZ", apply: parseVar(func(c *Config) **time.Location { return &c.Notify.Silent.QuietZone }, parseLocation)},
		{name: "RAEDAR_QUIET_PAGE_CRITICAL", apply: parseVar(func(c *Config) *bool { return &c.Notify.Silent.QuietPageCritical }, parseBool)},
		{name: "RAEDAR_PAGER_COOLDOWN", apply: parseVar(func(c *Config) *time.Duration { return &c.Notify.Silent.PagerCooldown }, parseDurationOrZero)},
		{name: "RAEDAR_PAGER_COOLDOWN_BY_RULE", apply: parseVar(func(c *Config) *map[string]time.Duration { return &c.Notify.Silent.PagerCooldownByRule }, parseCooldownByRule)},
		{name: "RAEDAR_FLAP_WINDOW", apply: parseVar(func(c *Config) *time.Duration { return &c.Notify.Silent.FlapWindow }, parseDurationOrZero)},
		{name: "RAEDAR_FLAP_MAX_PAGES", apply: parseVar(func(c *Config) *int { return &c.Notify.Silent.FlapMaxPages }, parseIntAtLeast(1))},
		{name: "RAEDAR_STAND_DOWN_AFTER", apply: parseVar(func(c *Config) *time.Duration { return &c.Notify.Silent.StandDownAfter }, parseDurationOrZero)},
		{name: "RAEDAR_DEPLOY_SILENCE", apply: parseVar(func(c *Config) *time.Duration { return &c.Notify.Silent.DeploySilence }, parseDurationOrZero)},

		// Presentation
		{name: "RAEDAR_SUBJECT_NAMES", apply: parseVar(func(c *Config) *SubjectNames { return &c.SubjectNames }, parseSubjectNames)},

		// Heartbeats
		{name: "RAEDAR_HEARTBEAT_URL", apply: parseVar(func(c *Config) *string { return &c.Heartbeat.URL }, parseHTTPURL)},
		{name: "RAEDAR_HEARTBEAT_HEADER", apply: parseVar(func(c *Config) **Header { return &c.Heartbeat.Header }, parseOneHeader)},
		{name: "RAEDAR_HEARTBEAT_INTERVAL", apply: parseVar(func(c *Config) *time.Duration { return &c.Heartbeat.Interval }, parseDuration)},
		{name: "RAEDAR_HEARTBEAT_PEERS", apply: parseVar(func(c *Config) *[]string { return &c.Heartbeat.Peers }, parseListChecked(checkPeerName))},
		{name: "RAEDAR_HEARTBEAT_RECEIVE_TOKEN", apply: parseVar(func(c *Config) *string { return &c.Heartbeat.ReceiveToken }, parseString)},
		{name: "RAEDAR_HEARTBEAT_TIMEOUT", apply: parseVar(func(c *Config) *time.Duration { return &c.Heartbeat.Timeout }, parseDuration)},

		// Retention
		{name: "RAEDAR_RETENTION_MINUTE", apply: parseVar(func(c *Config) *time.Duration { return &c.Retention.Minute }, parseDuration)},
		{name: "RAEDAR_RETENTION_PATHS", apply: parseVar(func(c *Config) *time.Duration { return &c.Retention.Paths }, parseDuration)},
		{name: "RAEDAR_RETENTION_HOUR", apply: parseVar(func(c *Config) *time.Duration { return &c.Retention.Hour }, parseDuration)},
		{name: "RAEDAR_RETENTION_EVENTS", apply: parseVar(func(c *Config) *time.Duration { return &c.Retention.Events }, parseDuration)},
	}
}

// parseVar returns an apply function that parses one value and assigns it. An
// absent or empty value keeps the default.
func parseVar[T any](field func(*Config) *T, parse func(string) (T, error)) func(*Config, *string) error {
	return func(c *Config, raw *string) error {
		if raw == nil || *raw == "" {
			return nil
		}
		value, err := parse(*raw)
		if err != nil {
			return err
		}
		*field(c) = value
		return nil
	}
}

// applyDatabaseURL assigns the required PostgreSQL connection URL.
func applyDatabaseURL(c *Config, raw *string) error {
	if raw == nil || *raw == "" {
		return errors.New("is required")
	}
	value, err := parseDatabaseURL(*raw)
	if err != nil {
		return err
	}
	c.DatabaseURL = value
	return nil
}

// applyDigestAt assigns the digest time; an empty value disables the digest.
func applyDigestAt(c *Config, raw *string) error {
	switch {
	case raw == nil:
		return nil
	case *raw == "":
		c.Notify.DigestAt = nil
		return nil
	}
	at, err := parseTimeOfDay(*raw)
	if err != nil {
		return err
	}
	c.Notify.DigestAt = &at
	return nil
}

// applyDisks assigns the filesystems to watch, each a path or a name=path
// entry. Without a display name, a disk is named by its path relative to the
// host root.
func applyDisks(c *Config, raw *string) error {
	if raw == nil || *raw == "" {
		return nil
	}
	items := parseList(*raw)
	if len(items) == 0 {
		return nil
	}
	disks := make([]Disk, 0, len(items))
	for _, item := range items {
		name, path, named := strings.Cut(item, "=")
		if !named {
			path = item
			name = ""
		} else if name == "" || path == "" || strings.ContainsAny(name, "/ \t") {
			return errors.New("must be a filesystem path or a name=path entry, for example /var/lib/docker or data=/srv/data")
		}
		if !filepath.IsAbs(path) {
			return errors.New("must use paths inside the container, for example /var/lib/docker")
		}
		if name == "" {
			name = diskName(c.Host.Root, path)
		}
		disks = append(disks, Disk{Name: name, Path: path})
	}
	c.Host.Disks = disks
	return nil
}

// diskName is the display name of a filesystem: its path relative to the host
// root, or the path itself when it is outside the host root.
func diskName(hostRoot, path string) string {
	rel, err := filepath.Rel(hostRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return path
	}
	if rel == "." {
		return "/"
	}
	return "/" + rel
}
