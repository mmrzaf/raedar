package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// lookupFrom returns an EnvLookup backed by a map.
func lookupFrom(env map[string]string) EnvLookup {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

// databaseEnv returns an environment with the required database URL.
func databaseEnv() map[string]string {
	return map[string]string{
		"RAEDAR_DATABASE_URL": "postgres://raedar:pw@postgres:5432/raedar?sslmode=disable",
	}
}

// hostRootEnv returns a temporary host root holding an /etc/hostname file,
// with an environment that uses it.
func hostRootEnv(t *testing.T, name string) (string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	writeHostname(t, root, name+"\n")
	env := databaseEnv()
	env["RAEDAR_HOST_ROOT"] = root
	return root, env
}

// writeHostname writes content to <root>/etc/hostname.
func writeHostname(t *testing.T, root, content string) {
	t.Helper()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatalf("create %s: %v", etc, err)
	}
	if err := os.WriteFile(filepath.Join(etc, "hostname"), []byte(content), 0o644); err != nil {
		t.Fatalf("write hostname: %v", err)
	}
}

// writeFile writes a file for the _FILE mechanism and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// loadWithExtras loads a configuration from the required variables plus the
// given overrides.
func loadWithExtras(t *testing.T, extras map[string]string) (*Config, error) {
	t.Helper()
	env := databaseEnv()
	for name, value := range extras {
		env[name] = value
	}
	return Load(lookupFrom(env))
}

func TestLoadDefaults(t *testing.T) {
	root, env := hostRootEnv(t, "test-host")
	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := &Config{
		DatabaseURL: "postgres://raedar:pw@postgres:5432/raedar?sslmode=disable",
		Listen:      ":8080",
		HostName:    "test-host",
		Location:    time.UTC,
		LogLevel:    slog.LevelInfo,
		Docker: DockerConfig{
			Host: "unix:///var/run/docker.sock",
		},
		Host: HostConfig{
			Root:    root,
			Disks:   []Disk{{Name: "/", Path: root}},
			Systemd: SystemdAuto,
		},
		Rules: RulesConfig{
			ErrorsMinRate:          2,
			ErrorsFactor:           3,
			ErrorsMinRequests:      20,
			SlowMinP95:             300 * time.Millisecond,
			SlowFactor:             2,
			SlowMinRequests:        50,
			MemoryLimitWarn:        90,
			DiskWarn:               85,
			DiskCrit:               95,
			DiskFullWithin:         3 * 24 * time.Hour,
			HostMemoryMinAvailable: 5,
			CertWarn:               14 * 24 * time.Hour,
			CertCrit:               3 * 24 * time.Hour,
			ResolveAfter:           5 * time.Minute,
			CrashLoopWindow:        10 * time.Minute,
			CrashLoopCrashes:       3,
		},
		Notify: NotifyConfig{
			WarningChannels: []string{"telegram", "webhook"},
			DigestAt:        &TimeOfDay{Hour: 9},
			DigestChannels:  []string{"telegram", "webhook"},
			RateLimit:       30,
			SMSRateLimit:    10,
			Telegram:        TelegramConfig{APIURL: "https://api.telegram.org"},
			SMS: SMSConfig{
				HTTP: SMSHTTPConfig{Method: "POST", Encoding: "json"},
			},
		},
		Heartbeat: HeartbeatConfig{
			Interval: time.Minute,
			Timeout:  3 * time.Minute,
		},
		Retention: RetentionConfig{
			Minute: 14 * 24 * time.Hour,
			Paths:  7 * 24 * time.Hour,
			Hour:   400 * 24 * time.Hour,
			Events: 400 * 24 * time.Hour,
		},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v\nwant      %+v", cfg, want)
	}
}

func TestLoadHostName(t *testing.T) {
	t.Run("from environment", func(t *testing.T) {
		_, env := hostRootEnv(t, "file-host")
		env["RAEDAR_HOST_NAME"] = "given-host"
		cfg, err := Load(lookupFrom(env))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.HostName != "given-host" {
			t.Errorf("HostName = %q, want %q", cfg.HostName, "given-host")
		}
	})

	t.Run("from host root etc hostname", func(t *testing.T) {
		_, env := hostRootEnv(t, "file-host")
		cfg, err := Load(lookupFrom(env))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.HostName != "file-host" {
			t.Errorf("HostName = %q, want %q", cfg.HostName, "file-host")
		}
	})

	t.Run("first line only", func(t *testing.T) {
		root := t.TempDir()
		writeHostname(t, root, "first-host\nsecond-host\n")
		env := databaseEnv()
		env["RAEDAR_HOST_ROOT"] = root
		cfg, err := Load(lookupFrom(env))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.HostName != "first-host" {
			t.Errorf("HostName = %q, want %q", cfg.HostName, "first-host")
		}
	})

	t.Run("falls back to the process host name", func(t *testing.T) {
		env := databaseEnv()
		env["RAEDAR_HOST_ROOT"] = t.TempDir()
		cfg, err := Load(lookupFrom(env))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		name, err := os.Hostname()
		if err != nil {
			t.Fatalf("os.Hostname() error = %v", err)
		}
		if cfg.HostName != name {
			t.Errorf("HostName = %q, want %q", cfg.HostName, name)
		}
	})
}

func TestLoadTimezone(t *testing.T) {
	cfg, err := loadWithExtras(t, map[string]string{"RAEDAR_TZ": "Asia/Tehran"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Location.String(); got != "Asia/Tehran" {
		t.Errorf("Location = %q, want %q", got, "Asia/Tehran")
	}
}

func TestLoadEmptyValueKeepsDefault(t *testing.T) {
	_, env := hostRootEnv(t, "test-host")
	env["RAEDAR_LISTEN"] = ""
	env["RAEDAR_TZ"] = ""
	env["RAEDAR_NOTIFY_WARNING"] = ""
	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.Location != time.UTC {
		t.Errorf("Location = %v, want UTC", cfg.Location)
	}
	if !reflect.DeepEqual(cfg.Notify.WarningChannels, []string{"telegram", "webhook"}) {
		t.Errorf("WarningChannels = %v, want the default list", cfg.Notify.WarningChannels)
	}
}

func TestLoadEmptyListClearsDefault(t *testing.T) {
	cfg, err := loadWithExtras(t, map[string]string{"RAEDAR_NOTIFY_WARNING": " , "})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notify.WarningChannels != nil {
		t.Errorf("WarningChannels = %v, want nil", cfg.Notify.WarningChannels)
	}
}

func TestLoadDigestAt(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want *TimeOfDay
	}{
		{name: "default", want: &TimeOfDay{Hour: 9}},
		{name: "configured", env: map[string]string{"RAEDAR_DIGEST_AT": "21:30"}, want: &TimeOfDay{Hour: 21, Minute: 30}},
		{name: "empty disables", env: map[string]string{"RAEDAR_DIGEST_AT": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadWithExtras(t, tt.env)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !reflect.DeepEqual(cfg.Notify.DigestAt, tt.want) {
				t.Errorf("DigestAt = %v, want %v", cfg.Notify.DigestAt, tt.want)
			}
		})
	}
}

func TestLoadDisks(t *testing.T) {
	t.Run("names relative to the host root", func(t *testing.T) {
		root, env := hostRootEnv(t, "test-host")
		env["RAEDAR_DISKS"] = root + "/var/lib/docker,docker=/srv/docker,/srv/other"
		cfg, err := Load(lookupFrom(env))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		want := []Disk{
			{Name: "/var/lib/docker", Path: root + "/var/lib/docker"},
			{Name: "docker", Path: "/srv/docker"},
			{Name: "/srv/other", Path: "/srv/other"},
		}
		if !reflect.DeepEqual(cfg.Host.Disks, want) {
			t.Errorf("Disks = %+v, want %+v", cfg.Host.Disks, want)
		}
	})

	t.Run("invalid entries", func(t *testing.T) {
		tests := []struct {
			name    string
			value   string
			wantErr string
		}{
			{name: "relative path", value: "relative/dir", wantErr: "must use paths inside the container"},
			{name: "missing name", value: "=/srv/data", wantErr: "must be a filesystem path or a name=path entry"},
			{name: "missing path", value: "data=", wantErr: "must be a filesystem path or a name=path entry"},
			{name: "slash in name", value: "/srv/data=x", wantErr: "must be a filesystem path or a name=path entry"},
			{name: "duplicate names", value: "data=/srv/a,data=/srv/b", wantErr: "two entries have the same name"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := loadWithExtras(t, map[string]string{"RAEDAR_DISKS": tt.value})
				assertErrorContains(t, err, tt.wantErr)
			})
		}
	})
}

func TestLoadFileValue(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "plain", content: "123456:token", want: "123456:token"},
		{name: "trailing newline", content: "123456:token\n", want: "123456:token"},
		{name: "trailing carriage returns", content: "123456:token\r\n\n", want: "123456:token"},
		{name: "internal newline kept", content: "line1\nline2\n", want: "line1\nline2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := databaseEnv()
			env["RAEDAR_TELEGRAM_TOKEN_FILE"] = writeFile(t, tt.content)
			env["RAEDAR_TELEGRAM_CHAT_IDS"] = "123456789"
			cfg, err := Load(lookupFrom(env))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Notify.Telegram.Token != tt.want {
				t.Errorf("Token = %q, want %q", cfg.Notify.Telegram.Token, tt.want)
			}
		})
	}
}

func TestLoadFileMissing(t *testing.T) {
	env := databaseEnv()
	env["RAEDAR_TELEGRAM_TOKEN_FILE"] = filepath.Join(t.TempDir(), "missing")
	_, err := Load(lookupFrom(env))
	assertErrorContains(t, err, "RAEDAR_TELEGRAM_TOKEN_FILE")
}

func TestLoadFileEmptyValue(t *testing.T) {
	env := map[string]string{"RAEDAR_DATABASE_URL_FILE": writeFile(t, "")}
	_, err := Load(lookupFrom(env))
	assertErrorContains(t, err, "RAEDAR_DATABASE_URL: is required")
}

func TestLoadBothFormsSet(t *testing.T) {
	for _, v := range variables() {
		t.Run(v.name, func(t *testing.T) {
			env := databaseEnv()
			env[v.name] = "value"
			env[v.name+"_FILE"] = filepath.Join(t.TempDir(), "value")
			_, err := Load(lookupFrom(env))
			assertErrorContains(t, err, v.name+" and "+v.name+"_FILE are both set")
		})
	}
}

func TestLoadBothFormsSetWithEmptyValue(t *testing.T) {
	env := databaseEnv()
	env["RAEDAR_TELEGRAM_TOKEN"] = ""
	env["RAEDAR_TELEGRAM_TOKEN_FILE"] = writeFile(t, "123456:token")
	_, err := Load(lookupFrom(env))
	assertErrorContains(t, err, "RAEDAR_TELEGRAM_TOKEN and RAEDAR_TELEGRAM_TOKEN_FILE are both set")
}

func TestLoadDatabaseURLRequired(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{}))
	assertErrorContains(t, err, "RAEDAR_DATABASE_URL: is required")
}

func TestLoadReportsEveryProblem(t *testing.T) {
	env := map[string]string{
		"RAEDAR_TZ":               "Mars/Olympus",
		"RAEDAR_LISTEN":           "not-an-address",
		"RAEDAR_RESOLVE_AFTER":    "5",
		"RAEDAR_WEBHOOK_URL":      "https://hooks.example.com/raedar",
		"RAEDAR_WEBHOOK_URL_FILE": "/run/secrets/webhook_url",
	}
	_, err := Load(lookupFrom(env))
	if err == nil {
		t.Fatal("Load() error = nil, want problems")
	}
	wantContained := []string{
		"RAEDAR_DATABASE_URL: is required",
		"RAEDAR_TZ: must be an IANA time zone",
		"RAEDAR_LISTEN: must be an address in host:port form",
		"RAEDAR_RESOLVE_AFTER: must be a duration",
		"RAEDAR_WEBHOOK_URL and RAEDAR_WEBHOOK_URL_FILE are both set",
	}
	message := err.Error()
	for _, want := range wantContained {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to contain %q", message, want)
		}
	}
}

func TestLoadInvalidValueNamesVariable(t *testing.T) {
	// Free-form variables (tokens, secrets, keys, bodies, paths and names) have
	// no invalid value and are not listed here.
	tests := []struct {
		name  string
		value string
		extra map[string]string
	}{
		{name: "RAEDAR_DATABASE_URL", value: "mysql://raedar@postgres/raedar"},
		{name: "RAEDAR_LISTEN", value: "not-an-address"},
		{name: "RAEDAR_PUBLIC_URL", value: "raedar.example.com"},
		{name: "RAEDAR_HOST_NAME", value: "host 1"},
		{name: "RAEDAR_TZ", value: "Mars/Olympus"},
		{name: "RAEDAR_LOG_LEVEL", value: "loud"},
		{name: "RAEDAR_DOCKER_HOST", value: "docker.example.com"},
		{name: "RAEDAR_TRAEFIK_CONTAINER", value: "trae fik"},
		{name: "RAEDAR_TLS_CHECK_ADDR", value: "traefik"},
		{name: "RAEDAR_TLS_DOMAINS", value: "bad_domain"},
		{name: "RAEDAR_HOST_ROOT", value: "relative"},
		{name: "RAEDAR_DISKS", value: "relative"},
		{name: "RAEDAR_SYSTEMD", value: "maybe"},
		{name: "RAEDAR_SYSTEMD_IGNORE", value: "bad/unit"},
		{name: "RAEDAR_SYSTEMD_CRITICAL", value: "bad/unit"},
		{name: "RAEDAR_ERRORS_MIN_RATE", value: "101%"},
		{name: "RAEDAR_ERRORS_FACTOR", value: "0.5"},
		{name: "RAEDAR_ERRORS_MIN_REQUESTS", value: "0"},
		{name: "RAEDAR_SLOW_MIN_P95", value: "300"},
		{name: "RAEDAR_SLOW_FACTOR", value: "twice"},
		{name: "RAEDAR_SLOW_MIN_REQUESTS", value: "many"},
		{name: "RAEDAR_MEMORY_LIMIT_WARN", value: "-1"},
		{name: "RAEDAR_DISK_WARN", value: "almost full"},
		{name: "RAEDAR_DISK_CRIT", value: "150%"},
		{name: "RAEDAR_DISK_FULL_WITHIN", value: "0d"},
		{name: "RAEDAR_HOST_MEMORY_MIN_AVAILABLE", value: "low"},
		{name: "RAEDAR_CERT_WARN", value: "two weeks"},
		{name: "RAEDAR_CERT_CRIT", value: "soon"},
		{name: "RAEDAR_RESOLVE_AFTER", value: "5"},
		{name: "RAEDAR_NOTIFY_WARNING", value: "email"},
		{name: "RAEDAR_NOTIFY_CRITICAL", value: "email"},
		{name: "RAEDAR_DIGEST_AT", value: "9am"},
		{name: "RAEDAR_DIGEST_CHANNELS", value: "email"},
		{name: "RAEDAR_RATE_LIMIT", value: "0"},
		{name: "RAEDAR_SMS_RATE_LIMIT", value: "-5"},
		{name: "RAEDAR_OUTBOUND_PROXY", value: "ftp://proxy.example.com"},
		{name: "RAEDAR_TELEGRAM_CHAT_IDS", value: "abc", extra: map[string]string{"RAEDAR_TELEGRAM_TOKEN": "123456:abcdef"}},
		{name: "RAEDAR_TELEGRAM_API_URL", value: "telegram.example.com"},
		{name: "RAEDAR_SMS_TO", value: "12 34"},
		{name: "RAEDAR_SMS_HTTP_URL", value: "sms.example.com"},
		{name: "RAEDAR_SMS_HTTP_METHOD", value: "PUT"},
		{name: "RAEDAR_SMS_HTTP_HEADERS", value: "NoColon"},
		{name: "RAEDAR_SMS_HTTP_ENCODING", value: "yaml"},
		{name: "RAEDAR_WEBHOOK_URL", value: "not-a-url"},
		{name: "RAEDAR_HEARTBEAT_URL", value: "not-a-url"},
		{name: "RAEDAR_HEARTBEAT_HEADER", value: "NoColon"},
		{name: "RAEDAR_HEARTBEAT_INTERVAL", value: "1min"},
		{name: "RAEDAR_HEARTBEAT_PEERS", value: "host/2"},
		{name: "RAEDAR_HEARTBEAT_TIMEOUT", value: "3 minutes"},
		{name: "RAEDAR_RETENTION_MINUTE", value: "forever"},
		{name: "RAEDAR_RETENTION_PATHS", value: "a week"},
		{name: "RAEDAR_RETENTION_HOUR", value: "forever"},
		{name: "RAEDAR_RETENTION_EVENTS", value: "forever"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := databaseEnv()
			for name, value := range tt.extra {
				env[name] = value
			}
			env[tt.name] = tt.value
			_, err := Load(lookupFrom(env))
			if err == nil {
				t.Fatalf("Load() error = nil, want an error naming %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.name+":") {
				t.Errorf("error = %q, want it to name %s", err.Error(), tt.name)
			}
		})
	}
}
