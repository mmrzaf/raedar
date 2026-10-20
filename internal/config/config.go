// Package config reads Raedar's configuration from the environment, applies
// the documented defaults and reports every problem at once.
package config

import (
	"log/slog"
	"slices"
	"time"
)

// Config is the effective configuration of one Raedar instance.
type Config struct {
	// DatabaseURL is the PostgreSQL connection URL.
	DatabaseURL string
	// Listen is the address the web server listens on.
	Listen string
	// PublicURL is the public URL of the UI, used for links in messages. It is
	// empty when not configured, and messages then contain no links.
	PublicURL string
	// HostName is the name of this host in the UI and in messages.
	HostName string
	// Location is the time zone used for display, baselines, heatmaps and the
	// digest.
	Location *time.Location
	// LogLevel is the level of the JSON logs written to stdout.
	LogLevel slog.Level

	// Docker holds the Docker and Traefik settings.
	Docker DockerConfig
	// Host holds the host filesystem and systemd settings.
	Host HostConfig
	// Rules holds the thresholds of the alerting rules.
	Rules RulesConfig
	// Notify holds the notification and digest settings.
	Notify NotifyConfig
	// Heartbeat holds the heartbeat sender and receiver settings.
	Heartbeat HeartbeatConfig
	// Retention holds how long history is kept.
	Retention RetentionConfig
}

// DockerConfig holds the Docker and Traefik settings.
type DockerConfig struct {
	// Host is the Docker API address, either unix:// or tcp://.
	Host string
	// TraefikContainer is the name of the Traefik container, empty when it is
	// detected.
	TraefikContainer string
	// AccessLogPath is the path of the Traefik access log file inside the
	// container, empty when the log is read from the Traefik container's
	// output.
	AccessLogPath string
	// TLSCheckAddr is the host:port where Traefik serves HTTPS, empty when the
	// address is detected.
	TLSCheckAddr string
	// TLSDomains are extra domains to check, for routes that are defined
	// outside Docker labels.
	TLSDomains []string
}

// HostConfig holds the host filesystem and systemd settings.
type HostConfig struct {
	// Root is where the host's root filesystem is mounted.
	Root string
	// Disks are the filesystems watched by the host collector.
	Disks []Disk
	// Systemd selects whether systemd is read and what a missing D-Bus socket
	// means.
	Systemd SystemdMode
	// SystemdIgnore are unit name patterns that are skipped.
	SystemdIgnore []string
	// SystemdCritical are unit name patterns whose failure is critical.
	SystemdCritical []string
}

// RulesConfig holds the thresholds of the alerting rules.
type RulesConfig struct {
	// ErrorsMinRate is the minimum 5xx rate for route.errors.
	ErrorsMinRate Percent
	// ErrorsFactor is the multiple of the baseline 5xx rate for route.errors.
	ErrorsFactor float64
	// ErrorsMinRequests is the minimum number of requests for route.errors and
	// for baselines.
	ErrorsMinRequests int
	// SlowMinP95 is the minimum p95 latency for route.slow.
	SlowMinP95 time.Duration
	// SlowFactor is the multiple of the baseline p95 for route.slow.
	SlowFactor float64
	// SlowMinRequests is the minimum number of latency-counted requests for
	// route.slow.
	SlowMinRequests int
	// MemoryLimitWarn is the share of the memory limit for container.memory.
	MemoryLimitWarn Percent
	// DiskWarn is the usage where host.disk and host.inodes warn.
	DiskWarn Percent
	// DiskCrit is the usage where host.disk and host.inodes become critical.
	DiskCrit Percent
	// DiskFullWithin is the projection horizon for host.disk_forecast.
	DiskFullWithin time.Duration
	// HostMemoryMinAvailable is the available memory share below which
	// host.memory warns.
	HostMemoryMinAvailable Percent
	// CertWarn is the remaining certificate validity that warns.
	CertWarn time.Duration
	// CertCrit is the remaining certificate validity that is critical.
	CertCrit time.Duration
	// ResolveAfter is the time a condition must be false before an incident
	// resolves, for rules that do not define their own resolution.
	ResolveAfter time.Duration
}

// NotifyConfig holds the notification and digest settings.
type NotifyConfig struct {
	// WarningChannels are the channels that receive warning incidents.
	WarningChannels []string
	// CriticalChannels are the channels that receive critical incidents, nil
	// when every configured channel receives them.
	CriticalChannels []string
	// DigestAt is the local time of the daily digest, nil when the digest is
	// disabled.
	DigestAt *TimeOfDay
	// DigestChannels are the channels that receive the digest.
	DigestChannels []string
	// RateLimit is the maximum number of messages per hour per channel.
	RateLimit int
	// SMSRateLimit is the maximum number of SMS per hour.
	SMSRateLimit int
	// OutboundProxy is the proxy used for outbound requests, empty when no
	// proxy is used.
	OutboundProxy string
	// ProxyFor are the outbound requests that use the proxy: telegram, sms,
	// webhook and heartbeat. An empty list means every request uses it.
	ProxyFor []string
	// Telegram holds the Telegram channel settings.
	Telegram TelegramConfig
	// SMS holds the SMS channel settings.
	SMS SMSConfig
	// Webhook holds the webhook channel settings.
	Webhook WebhookConfig
	// Silent holds the silencing settings: quiet hours, ack persistence,
	// pager cooldown and stand-down.
	Silent SilentConfig
}

// TelegramConfig holds the Telegram channel settings.
type TelegramConfig struct {
	// Token is the bot token, empty when Telegram is not configured.
	Token string
	// ChatIDs are the chats the bot posts to.
	ChatIDs []string
	// APIURL is the Bot API base URL.
	APIURL string
}

// SMSConfig holds the SMS channel settings.
type SMSConfig struct {
	// Provider is "kavenegar" or "http", empty when SMS is not configured.
	Provider string
	// To are the recipient numbers.
	To []string
	// KavenegarAPIKey is the Kavenegar API key.
	KavenegarAPIKey string
	// KavenegarSender is the Kavenegar sender line, empty for Kavenegar's
	// default line.
	KavenegarSender string
	// HTTP holds the generic HTTP provider settings.
	HTTP SMSHTTPConfig
}

// SMSHTTPConfig holds the generic HTTP SMS provider settings.
type SMSHTTPConfig struct {
	// URL is the request URL; it may contain {{to}} and {{message}}.
	URL string
	// Method is "GET" or "POST".
	Method string
	// Headers are extra request headers.
	Headers []Header
	// Body is the request body template.
	Body string
	// Encoding is how placeholder values are escaped in the body: "json",
	// "form" or "raw".
	Encoding string
}

// WebhookConfig holds the webhook channel settings.
type WebhookConfig struct {
	// URL is the URL that receives incident and digest requests, empty when
	// the webhook is not configured.
	URL string
	// Secret signs the requests, empty when they are not signed.
	Secret string
}

// HeartbeatConfig holds the heartbeat sender and receiver settings.
type HeartbeatConfig struct {
	// URL is requested while Raedar is healthy, empty when the sender is
	// disabled.
	URL string
	// Header is the single header sent with the heartbeat request, nil when no
	// header is sent.
	Header *Header
	// Interval is how often the heartbeat URL is requested.
	Interval time.Duration
	// Peers are the names of the peers whose heartbeats this instance expects.
	Peers []string
	// ReceiveToken is the bearer token peers must send, empty when the
	// receiver is disabled.
	ReceiveToken string
	// Timeout is the silence after which a peer is reported.
	Timeout time.Duration
}

// SilentConfig holds the silencing settings shared by the gate and the UI.
//
// QuietFrom and QuietTo form an optional [from, to) window in QuietZone.
// Overnight windows (QuietFrom > QuietTo) span midnight. A nil side
// disables that edge.
//
// PagerCooldown is the minimum time between two pages for the same
// (rule, subject). Per-rule overrides are set by PagerCooldownByRule.
//
// StandDownAfter is the idle duration after which Raedar emits a single
// 'all quiet for N h' message; the disabled default is 0 (never).
type SilentConfig struct {
	QuietFrom           *TimeOfDay
	QuietTo             *TimeOfDay
	QuietZone           *time.Location
	QuietPageCritical   bool
	PagerCooldown       time.Duration
	PagerCooldownByRule map[string]time.Duration
	StandDownAfter      time.Duration
	DeploySilence       time.Duration
}

// RetentionConfig holds how long history is kept.
type RetentionConfig struct {
	// Minute is the retention of the per-minute tables.
	Minute time.Duration
	// Paths is the retention of the per-path windows.
	Paths time.Duration
	// Hour is the retention of the hourly tables and docker_disk.
	Hour time.Duration
	// Events is the retention of events, incidents and deliveries.
	Events time.Duration
}

// The outbound request kinds the proxy can cover.
const (
	// ProxyTargetTelegram is the Telegram Bot API.
	ProxyTargetTelegram = "telegram"
	// ProxyTargetSMS is the SMS provider.
	ProxyTargetSMS = "sms"
	// ProxyTargetWebhook is the webhook target.
	ProxyTargetWebhook = "webhook"
	// ProxyTargetHeartbeat is the outgoing heartbeat URL.
	ProxyTargetHeartbeat = "heartbeat"
)

// UsesProxy reports whether outbound requests of a kind go through the
// outbound proxy. Without a proxy, nothing does; without a selection, every
// request does.
func (c Config) UsesProxy(target string) bool {
	if c.Notify.OutboundProxy == "" {
		return false
	}
	if len(c.Notify.ProxyFor) == 0 {
		return true
	}
	return slices.Contains(c.Notify.ProxyFor, target)
}
