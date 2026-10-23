package config

import (
	"log/slog"
	"reflect"
	"testing"
	"time"
)

// valueCase sets one environment variable and checks the field it fills.
type valueCase struct {
	value string
	extra map[string]string
	check func(t *testing.T, cfg *Config)
}

// wantString checks a string field.
func wantString(get func(*Config) string, want string) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// wantInt checks a whole number field.
func wantInt(get func(*Config) int, want int) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	}
}

// wantFloat checks a factor field.
func wantFloat(get func(*Config) float64, want float64) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// wantDuration checks a duration field.
func wantDuration(get func(*Config) time.Duration, want time.Duration) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// wantPercent checks a percentage field.
func wantPercent(get func(*Config) Percent, want Percent) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// wantStrings checks a list field.
func wantStrings(get func(*Config) []string, want ...string) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// wantDisks checks the disk list.
func wantDisks(want ...Disk) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.Host.Disks; !reflect.DeepEqual(got, want) {
			t.Errorf("Disks = %+v, want %+v", got, want)
		}
	}
}

// wantHeaders checks a header list.
func wantHeaders(want ...Header) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.Notify.SMS.HTTP.Headers; !reflect.DeepEqual(got, want) {
			t.Errorf("Headers = %+v, want %+v", got, want)
		}
	}
}

// wantHeader checks the single heartbeat header.
func wantHeader(want Header) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.Heartbeat.Header; got == nil || *got != want {
			t.Errorf("Header = %v, want %v", got, want)
		}
	}
}

// wantTimeOfDay checks the digest time.
func wantTimeOfDay(want TimeOfDay) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.Notify.DigestAt; got == nil || *got != want {
			t.Errorf("DigestAt = %v, want %v", got, want)
		}
	}
}

// wantQuietTimeOfDay checks the quiet-hours time-of-day pointer.
func wantQuietTimeOfDay(get func(*Config) *TimeOfDay, want TimeOfDay) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		value := get(cfg)
		if value == nil || *value != want {
			t.Errorf("got %v, want %v", value, want)
		}
	}
}

// wantQuietLocation checks the quiet-hours location.
func wantQuietLocation(wantName string) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		zone := cfg.Notify.Silent.QuietZone
		if zone == nil {
			t.Errorf("QuietZone = nil, want %q", wantName)
			return
		}
		if zone.String() != wantName {
			t.Errorf("QuietZone = %q, want %q", zone.String(), wantName)
		}
	}
}

// wantBoolField checks a bool config field.
func wantBoolField(get func(*Config) bool, want bool) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := get(cfg); got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// wantCooldownByRule checks the parsed per-rule pager cooldown map.
func wantCooldownByRule(get func(*Config) map[string]time.Duration, want map[string]time.Duration) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		got := get(cfg)
		if len(got) != len(want) {
			t.Errorf("len = %d, want %d (%v)", len(got), len(want), got)
			return
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %v, want %v", k, got[k], v)
			}
		}
	}
}

// wantLocation checks the time zone by name.
func wantLocation(want string) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.Location.String(); got != want {
			t.Errorf("Location = %q, want %q", got, want)
		}
	}
}

// wantLogLevel checks the log level.
func wantLogLevel(want slog.Level) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.LogLevel; got != want {
			t.Errorf("LogLevel = %v, want %v", got, want)
		}
	}
}

// wantSystemdMode checks the systemd mode.
func wantSystemdMode(want SystemdMode) func(*testing.T, *Config) {
	return func(t *testing.T, cfg *Config) {
		t.Helper()
		if got := cfg.Host.Systemd; got != want {
			t.Errorf("Systemd = %q, want %q", got, want)
		}
	}
}

// variableCases returns one case for every supported environment variable.
func variableCases() map[string]valueCase {
	return map[string]valueCase{
		// General
		"RAEDAR_DATABASE_URL": {
			value: "postgres://raedar:pw@postgres.example.com:5432/raedar?sslmode=disable",
			check: wantString(func(c *Config) string { return c.DatabaseURL }, "postgres://raedar:pw@postgres.example.com:5432/raedar?sslmode=disable"),
		},
		"RAEDAR_LISTEN": {
			value: "127.0.0.1:9000",
			check: wantString(func(c *Config) string { return c.Listen }, "127.0.0.1:9000"),
		},
		"RAEDAR_PUBLIC_URL": {
			value: "https://raedar.example.com/",
			check: wantString(func(c *Config) string { return c.PublicURL }, "https://raedar.example.com"),
		},
		"RAEDAR_HOST_NAME": {
			value: "host-1",
			check: wantString(func(c *Config) string { return c.HostName }, "host-1"),
		},
		"RAEDAR_TZ": {
			value: "Asia/Tehran",
			check: wantLocation("Asia/Tehran"),
		},
		"RAEDAR_LOG_LEVEL": {
			value: "debug",
			check: wantLogLevel(slog.LevelDebug),
		},

		// Docker and Traefik
		"RAEDAR_DOCKER_HOST": {
			value: "tcp://socket-proxy:2375",
			check: wantString(func(c *Config) string { return c.Docker.Host }, "tcp://socket-proxy:2375"),
		},
		"RAEDAR_TRAEFIK_CONTAINER": {
			value: "traefik",
			check: wantString(func(c *Config) string { return c.Docker.TraefikContainer }, "traefik"),
		},
		"RAEDAR_ACCESSLOG_PATH": {
			value: "/traefik-logs/access.log",
			check: wantString(func(c *Config) string { return c.Docker.AccessLogPath }, "/traefik-logs/access.log"),
		},
		"RAEDAR_TLS_CHECK_ADDR": {
			value: "traefik:443",
			check: wantString(func(c *Config) string { return c.Docker.TLSCheckAddr }, "traefik:443"),
		},
		"RAEDAR_TLS_DOMAINS": {
			value: "shop.example.com, *.example.com",
			check: wantStrings(func(c *Config) []string { return c.Docker.TLSDomains }, "shop.example.com", "*.example.com"),
		},

		// Host
		"RAEDAR_HOST_ROOT": {
			value: "/",
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Host.Root != "/" {
					t.Errorf("Root = %q, want %q", cfg.Host.Root, "/")
				}
				want := []Disk{{Name: "/", Path: "/"}}
				if !reflect.DeepEqual(cfg.Host.Disks, want) {
					t.Errorf("Disks = %+v, want %+v", cfg.Host.Disks, want)
				}
			},
		},
		"RAEDAR_DISKS": {
			value: "/host/root/var/lib/docker,docker=/srv/docker",
			extra: map[string]string{"RAEDAR_HOST_ROOT": "/host/root"},
			check: wantDisks(
				Disk{Name: "/var/lib/docker", Path: "/host/root/var/lib/docker"},
				Disk{Name: "docker", Path: "/srv/docker"},
			),
		},
		"RAEDAR_SYSTEMD": {
			value: "OFF",
			check: wantSystemdMode(SystemdOff),
		},
		"RAEDAR_SYSTEMD_IGNORE": {
			value: "apt-daily*.service, man-db.service",
			check: wantStrings(func(c *Config) []string { return c.Host.SystemdIgnore }, "apt-daily*.service", "man-db.service"),
		},
		"RAEDAR_SYSTEMD_CRITICAL": {
			value: "*-backup.service",
			check: wantStrings(func(c *Config) []string { return c.Host.SystemdCritical }, "*-backup.service"),
		},

		// Rules
		"RAEDAR_ERRORS_MIN_RATE": {
			value: "5%",
			check: wantPercent(func(c *Config) Percent { return c.Rules.ErrorsMinRate }, 5),
		},
		"RAEDAR_ERRORS_FACTOR": {
			value: "1.5",
			check: wantFloat(func(c *Config) float64 { return c.Rules.ErrorsFactor }, 1.5),
		},
		"RAEDAR_ERRORS_MIN_REQUESTS": {
			value: "100",
			check: wantInt(func(c *Config) int { return c.Rules.ErrorsMinRequests }, 100),
		},
		"RAEDAR_SLOW_MIN_P95": {
			value: "800ms",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.SlowMinP95 }, 800*time.Millisecond),
		},
		"RAEDAR_SLOW_FACTOR": {
			value: "4",
			check: wantFloat(func(c *Config) float64 { return c.Rules.SlowFactor }, 4),
		},
		"RAEDAR_SLOW_MIN_REQUESTS": {
			value: "200",
			check: wantInt(func(c *Config) int { return c.Rules.SlowMinRequests }, 200),
		},
		"RAEDAR_LONG_REQUEST": {
			value: "15s",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.LongRequest }, 15*time.Second),
		},
		"RAEDAR_LONG_REQUEST_SHARE": {
			value: "10%",
			check: wantPercent(func(c *Config) Percent { return c.Rules.LongRequestShare }, 10),
		},
		"RAEDAR_ROUTE_IGNORE": {
			value: "fallback@*, (unrouted)",
			check: wantStrings(func(c *Config) []string { return c.Rules.RouteIgnore }, "fallback@*", "(unrouted)"),
		},
		"RAEDAR_MEMORY_LIMIT_WARN": {
			value: "80",
			check: wantPercent(func(c *Config) Percent { return c.Rules.MemoryLimitWarn }, 80),
		},
		"RAEDAR_DISK_WARN": {
			value: "70%",
			check: wantPercent(func(c *Config) Percent { return c.Rules.DiskWarn }, 70),
		},
		"RAEDAR_DISK_CRIT": {
			value: "90",
			check: wantPercent(func(c *Config) Percent { return c.Rules.DiskCrit }, 90),
		},
		"RAEDAR_DISK_FULL_WITHIN": {
			value: "7d",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.DiskFullWithin }, 7*24*time.Hour),
		},
		"RAEDAR_HOST_MEMORY_MIN_AVAILABLE": {
			value: "10%",
			check: wantPercent(func(c *Config) Percent { return c.Rules.HostMemoryMinAvailable }, 10),
		},
		"RAEDAR_CERT_WARN": {
			value: "30d",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.CertWarn }, 30*24*time.Hour),
		},
		"RAEDAR_CERT_CRIT": {
			value: "7d",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.CertCrit }, 7*24*time.Hour),
		},
		"RAEDAR_RESOLVE_AFTER": {
			value: "10m",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.ResolveAfter }, 10*time.Minute),
		},
		"RAEDAR_CRASH_LOOP_WINDOW": {
			value: "15m",
			check: wantDuration(func(c *Config) time.Duration { return c.Rules.CrashLoopWindow }, 15*time.Minute),
		},
		"RAEDAR_CRASH_LOOP_CRASHES": {
			value: "5",
			check: wantInt(func(c *Config) int { return c.Rules.CrashLoopCrashes }, 5),
		},

		// Notifications
		"RAEDAR_NOTIFY_WARNING": {
			value: "sms",
			check: wantStrings(func(c *Config) []string { return c.Notify.WarningChannels }, "sms"),
		},
		"RAEDAR_NOTIFY_CRITICAL": {
			value: "telegram,sms",
			check: wantStrings(func(c *Config) []string { return c.Notify.CriticalChannels }, "telegram", "sms"),
		},
		"RAEDAR_DIGEST_AT": {
			value: "21:30",
			check: wantTimeOfDay(TimeOfDay{Hour: 21, Minute: 30}),
		},
		"RAEDAR_DIGEST_CHANNELS": {
			value: "sms",
			check: wantStrings(func(c *Config) []string { return c.Notify.DigestChannels }, "sms"),
		},
		"RAEDAR_RATE_LIMIT": {
			value: "60",
			check: wantInt(func(c *Config) int { return c.Notify.RateLimit }, 60),
		},
		"RAEDAR_SMS_RATE_LIMIT": {
			value: "5",
			check: wantInt(func(c *Config) int { return c.Notify.SMSRateLimit }, 5),
		},
		"RAEDAR_OUTBOUND_PROXY": {
			value: "socks5://proxy.example.com:1080",
			check: wantString(func(c *Config) string { return c.Notify.OutboundProxy }, "socks5://proxy.example.com:1080"),
		},
		"RAEDAR_OUTBOUND_PROXY_FOR": {
			value: "telegram,webhook",
			extra: map[string]string{"RAEDAR_OUTBOUND_PROXY": "http://proxy.example.com:3128"},
			check: wantStrings(func(c *Config) []string { return c.Notify.ProxyFor }, "telegram", "webhook"),
		},
		"RAEDAR_TELEGRAM_TOKEN": {
			value: "123456:abcdef",
			extra: map[string]string{"RAEDAR_TELEGRAM_CHAT_IDS": "123456789"},
			check: wantString(func(c *Config) string { return c.Notify.Telegram.Token }, "123456:abcdef"),
		},
		"RAEDAR_TELEGRAM_CHAT_IDS": {
			value: "-1001234567890, @channelname",
			extra: map[string]string{"RAEDAR_TELEGRAM_TOKEN": "123456:abcdef"},
			check: wantStrings(func(c *Config) []string { return c.Notify.Telegram.ChatIDs }, "-1001234567890", "@channelname"),
		},
		"RAEDAR_TELEGRAM_API_URL": {
			value: "https://telegram.example.com/",
			check: wantString(func(c *Config) string { return c.Notify.Telegram.APIURL }, "https://telegram.example.com"),
		},
		"RAEDAR_SMS_TO": {
			value: "+989120000000, 00989120000001",
			extra: map[string]string{
				"RAEDAR_SMS_HTTP_URL": "https://sms.example.com/send",
			},
			check: wantStrings(func(c *Config) []string { return c.Notify.SMS.To }, "+989120000000", "00989120000001"),
		},
		"RAEDAR_SMS_FROM": {
			value: "10004346",
			extra: smsHTTPEnv(),
			check: wantString(func(c *Config) string { return c.Notify.SMS.From }, "10004346"),
		},
		"RAEDAR_SMS_HTTP_URL": {
			value: "https://sms.example.com/send?to={{to}}&text={{message}}",
			extra: smsHTTPEnv(),
			check: wantString(func(c *Config) string { return c.Notify.SMS.HTTP.URL }, "https://sms.example.com/send?to={{to}}&text={{message}}"),
		},
		"RAEDAR_SMS_HTTP_METHOD": {
			value: "get",
			extra: smsHTTPEnv(),
			check: wantString(func(c *Config) string { return c.Notify.SMS.HTTP.Method }, "GET"),
		},
		"RAEDAR_SMS_HTTP_HEADERS": {
			value: "Authorization: Bearer token\nX-Extra: 1",
			extra: smsHTTPEnv(),
			check: wantHeaders(Header{Name: "Authorization", Value: "Bearer token"}, Header{Name: "X-Extra", Value: "1"}),
		},
		"RAEDAR_SMS_HTTP_BODY": {
			value: `{"to": "{{to}}", "text": "{{message}}"}`,
			extra: smsHTTPEnv(),
			check: wantString(func(c *Config) string { return c.Notify.SMS.HTTP.Body }, `{"to": "{{to}}", "text": "{{message}}"}`),
		},
		"RAEDAR_SMS_HTTP_ENCODING": {
			value: "form",
			extra: smsHTTPEnv(),
			check: wantString(func(c *Config) string { return c.Notify.SMS.HTTP.Encoding }, "form"),
		},
		"RAEDAR_SMS_HTTP_API_KEY": {
			value: "abc123",
			extra: smsHTTPEnv(),
			check: wantString(func(c *Config) string { return c.Notify.SMS.HTTP.APIKey }, "abc123"),
		},
		"RAEDAR_WEBHOOK_URL": {
			value: "https://hooks.example.com/raedar",
			check: wantString(func(c *Config) string { return c.Notify.Webhook.URL }, "https://hooks.example.com/raedar"),
		},
		"RAEDAR_WEBHOOK_SECRET": {
			value: "s3cret",
			extra: map[string]string{"RAEDAR_WEBHOOK_URL": "https://hooks.example.com/raedar"},
			check: wantString(func(c *Config) string { return c.Notify.Webhook.Secret }, "s3cret"),
		},

		// Presentation
		"RAEDAR_SUBJECT_NAMES": {
			value: "fallback@file=Unrouted traffic, certbot.*=Certificates",
			check: func(t *testing.T, c *Config) {
				t.Helper()
				if got := c.SubjectNames.Display("fallback@file"); got != "Unrouted traffic" {
					t.Errorf("Display(fallback@file) = %q", got)
				}
				if got := c.SubjectNames.Display("certbot.service"); got != "Certificates" {
					t.Errorf("Display(certbot.service) = %q", got)
				}
				if got := c.SubjectNames.Display("host.disk"); got != "host.disk" {
					t.Errorf("Display(host.disk) = %q, want unchanged", got)
				}
			},
		},

		// Heartbeats
		"RAEDAR_HEARTBEAT_URL": {
			value: "https://hb.example.com/host-1",
			check: wantString(func(c *Config) string { return c.Heartbeat.URL }, "https://hb.example.com/host-1"),
		},
		"RAEDAR_HEARTBEAT_HEADER": {
			value: "Authorization: Bearer token",
			extra: map[string]string{"RAEDAR_HEARTBEAT_URL": "https://hb.example.com/host-1"},
			check: wantHeader(Header{Name: "Authorization", Value: "Bearer token"}),
		},
		"RAEDAR_HEARTBEAT_INTERVAL": {
			value: "2m",
			check: wantDuration(func(c *Config) time.Duration { return c.Heartbeat.Interval }, 2*time.Minute),
		},
		"RAEDAR_HEARTBEAT_PEERS": {
			value: "host-2,host-3",
			extra: map[string]string{"RAEDAR_HEARTBEAT_RECEIVE_TOKEN": "token"},
			check: wantStrings(func(c *Config) []string { return c.Heartbeat.Peers }, "host-2", "host-3"),
		},
		"RAEDAR_HEARTBEAT_RECEIVE_TOKEN": {
			value: "token",
			extra: map[string]string{"RAEDAR_HEARTBEAT_PEERS": "host-2"},
			check: wantString(func(c *Config) string { return c.Heartbeat.ReceiveToken }, "token"),
		},
		"RAEDAR_HEARTBEAT_TIMEOUT": {
			value: "5m",
			check: wantDuration(func(c *Config) time.Duration { return c.Heartbeat.Timeout }, 5*time.Minute),
		},

		// Silencing (quiet hours, pager cooldown, stand-down, deploy silence).
		"RAEDAR_QUIET_FROM": {
			value: "22:00",
			extra: map[string]string{"RAEDAR_QUIET_TO": "07:00"},
			check: wantQuietTimeOfDay(func(c *Config) *TimeOfDay { return c.Notify.Silent.QuietFrom }, TimeOfDay{Hour: 22}),
		},
		"RAEDAR_QUIET_TO": {
			value: "07:00",
			extra: map[string]string{"RAEDAR_QUIET_FROM": "22:00"},
			check: wantQuietTimeOfDay(func(c *Config) *TimeOfDay { return c.Notify.Silent.QuietTo }, TimeOfDay{Hour: 7}),
		},
		"RAEDAR_QUIET_TZ": {
			value: "Asia/Tehran",
			check: wantQuietLocation("Asia/Tehran"),
		},
		"RAEDAR_QUIET_PAGE_CRITICAL": {
			value: "false",
			check: wantBoolField(func(c *Config) bool { return c.Notify.Silent.QuietPageCritical }, false),
		},
		"RAEDAR_PAGER_COOLDOWN": {
			value: "30m",
			check: wantDuration(func(c *Config) time.Duration { return c.Notify.Silent.PagerCooldown }, 30*time.Minute),
		},
		"RAEDAR_PAGER_COOLDOWN_BY_RULE": {
			value: "route.down=15m,host.disk=1h",
			check: wantCooldownByRule(func(c *Config) map[string]time.Duration {
				return c.Notify.Silent.PagerCooldownByRule
			}, map[string]time.Duration{"route.down": 15 * time.Minute, "host.disk": time.Hour}),
		},
		"RAEDAR_FLAP_WINDOW": {
			value: "2h",
			check: wantDuration(func(c *Config) time.Duration { return c.Notify.Silent.FlapWindow }, 2*time.Hour),
		},
		"RAEDAR_FLAP_MAX_PAGES": {
			value: "2",
			check: wantInt(func(c *Config) int { return c.Notify.Silent.FlapMaxPages }, 2),
		},
		"RAEDAR_STAND_DOWN_AFTER": {
			value: "24h",
			check: wantDuration(func(c *Config) time.Duration { return c.Notify.Silent.StandDownAfter }, 24*time.Hour),
		},
		"RAEDAR_DEPLOY_SILENCE": {
			value: "5m",
			check: wantDuration(func(c *Config) time.Duration { return c.Notify.Silent.DeploySilence }, 5*time.Minute),
		},

		// Retention
		"RAEDAR_RETENTION_MINUTE": {
			value: "30d",
			check: wantDuration(func(c *Config) time.Duration { return c.Retention.Minute }, 30*24*time.Hour),
		},
		"RAEDAR_RETENTION_PATHS": {
			value: "3d",
			check: wantDuration(func(c *Config) time.Duration { return c.Retention.Paths }, 3*24*time.Hour),
		},
		"RAEDAR_RETENTION_HOUR": {
			value: "800d",
			check: wantDuration(func(c *Config) time.Duration { return c.Retention.Hour }, 800*24*time.Hour),
		},
		"RAEDAR_RETENTION_EVENTS": {
			value: "200d",
			check: wantDuration(func(c *Config) time.Duration { return c.Retention.Events }, 200*24*time.Hour),
		},
	}
}

// smsHTTPEnv returns the environment of a complete generic HTTP SMS provider.
func smsHTTPEnv() map[string]string {
	return map[string]string{
		"RAEDAR_SMS_TO":       "+989120000000",
		"RAEDAR_SMS_HTTP_URL": "https://sms.example.com/send",
	}
}

func TestLoadEveryVariable(t *testing.T) {
	cases := variableCases()
	for _, v := range variables() {
		t.Run(v.name, func(t *testing.T) {
			tt, ok := cases[v.name]
			if !ok {
				t.Fatalf("no test case for %s", v.name)
			}
			env := databaseEnv()
			for name, value := range tt.extra {
				env[name] = value
			}
			env[v.name] = tt.value
			cfg, err := Load(lookupFrom(env))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestVariableNames(t *testing.T) {
	known := make(map[string]bool)
	for _, v := range variables() {
		if known[v.name] {
			t.Errorf("variable %s appears twice", v.name)
		}
		known[v.name] = true
	}
	for name := range variableCases() {
		if !known[name] {
			t.Errorf("test case for unknown variable %s", name)
		}
	}
	documented := []string{
		"RAEDAR_DATABASE_URL",
		"RAEDAR_LISTEN",
		"RAEDAR_PUBLIC_URL",
		"RAEDAR_HOST_NAME",
		"RAEDAR_TZ",
		"RAEDAR_LOG_LEVEL",
		"RAEDAR_DOCKER_HOST",
		"RAEDAR_TRAEFIK_CONTAINER",
		"RAEDAR_ACCESSLOG_PATH",
		"RAEDAR_TLS_CHECK_ADDR",
		"RAEDAR_TLS_DOMAINS",
		"RAEDAR_HOST_ROOT",
		"RAEDAR_DISKS",
		"RAEDAR_SYSTEMD",
		"RAEDAR_SYSTEMD_IGNORE",
		"RAEDAR_SYSTEMD_CRITICAL",
		"RAEDAR_ERRORS_MIN_RATE",
		"RAEDAR_ERRORS_FACTOR",
		"RAEDAR_ERRORS_MIN_REQUESTS",
		"RAEDAR_SLOW_MIN_P95",
		"RAEDAR_SLOW_FACTOR",
		"RAEDAR_SLOW_MIN_REQUESTS",
		"RAEDAR_LONG_REQUEST",
		"RAEDAR_LONG_REQUEST_SHARE",
		"RAEDAR_ROUTE_IGNORE",
		"RAEDAR_MEMORY_LIMIT_WARN",
		"RAEDAR_DISK_WARN",
		"RAEDAR_DISK_CRIT",
		"RAEDAR_DISK_FULL_WITHIN",
		"RAEDAR_HOST_MEMORY_MIN_AVAILABLE",
		"RAEDAR_CERT_WARN",
		"RAEDAR_CERT_CRIT",
		"RAEDAR_RESOLVE_AFTER",
		"RAEDAR_CRASH_LOOP_WINDOW",
		"RAEDAR_CRASH_LOOP_CRASHES",
		"RAEDAR_NOTIFY_WARNING",
		"RAEDAR_NOTIFY_CRITICAL",
		"RAEDAR_DIGEST_AT",
		"RAEDAR_DIGEST_CHANNELS",
		"RAEDAR_RATE_LIMIT",
		"RAEDAR_SMS_RATE_LIMIT",
		"RAEDAR_OUTBOUND_PROXY",
		"RAEDAR_OUTBOUND_PROXY_FOR",
		"RAEDAR_TELEGRAM_TOKEN",
		"RAEDAR_TELEGRAM_CHAT_IDS",
		"RAEDAR_TELEGRAM_API_URL",
		"RAEDAR_SMS_TO",
		"RAEDAR_SMS_FROM",
		"RAEDAR_SMS_HTTP_URL",
		"RAEDAR_SMS_HTTP_METHOD",
		"RAEDAR_SMS_HTTP_HEADERS",
		"RAEDAR_SMS_HTTP_BODY",
		"RAEDAR_SMS_HTTP_ENCODING",
		"RAEDAR_SMS_HTTP_API_KEY",
		"RAEDAR_WEBHOOK_URL",
		"RAEDAR_WEBHOOK_SECRET",
		"RAEDAR_SUBJECT_NAMES",
		"RAEDAR_HEARTBEAT_URL",
		"RAEDAR_HEARTBEAT_HEADER",
		"RAEDAR_HEARTBEAT_INTERVAL",
		"RAEDAR_HEARTBEAT_PEERS",
		"RAEDAR_HEARTBEAT_RECEIVE_TOKEN",
		"RAEDAR_HEARTBEAT_TIMEOUT",
		"RAEDAR_QUIET_FROM",
		"RAEDAR_QUIET_TO",
		"RAEDAR_QUIET_TZ",
		"RAEDAR_QUIET_PAGE_CRITICAL",
		"RAEDAR_PAGER_COOLDOWN",
		"RAEDAR_PAGER_COOLDOWN_BY_RULE",
		"RAEDAR_FLAP_WINDOW",
		"RAEDAR_FLAP_MAX_PAGES",
		"RAEDAR_STAND_DOWN_AFTER",
		"RAEDAR_DEPLOY_SILENCE",
		"RAEDAR_RETENTION_MINUTE",
		"RAEDAR_RETENTION_PATHS",
		"RAEDAR_RETENTION_HOUR",
		"RAEDAR_RETENTION_EVENTS",
	}
	if len(documented) != len(known) {
		t.Errorf("configuration.md documents %d variables, the code supports %d", len(documented), len(known))
	}
	for _, name := range documented {
		if !known[name] {
			t.Errorf("configuration.md variable %s is not supported", name)
		}
	}
}
