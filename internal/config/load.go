package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// variable is one environment variable and how it changes the configuration.
type variable struct {
	name  string
	apply func(c *Config, raw *string) error
}

// Load reads the configuration from the environment, applies the documented
// defaults and returns every problem it finds joined into one error.
//
// A variable that is set but empty is treated as not set and yields its
// default, except for RAEDAR_DIGEST_AT, where an empty value disables the
// digest. Every variable can also be given as NAME_FILE pointing to a file
// that contains the value; setting both forms is an error.
func Load(lookup EnvLookup) (*Config, error) {
	cfg := defaults()
	var problems []error
	for _, v := range variables() {
		raw, err := lookupValue(lookup, v.name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if err := v.apply(cfg, raw); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", v.name, err))
		}
	}
	if err := cfg.finalize(); err != nil {
		problems = append(problems, err)
	}
	problems = append(problems, cfg.validate()...)
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return cfg, nil
}

// lookupValue returns the value of one variable, resolved through its _FILE
// variant. A nil result means the variable is not set.
func lookupValue(lookup EnvLookup, name string) (*string, error) {
	value, valueSet := lookup(name)
	file, fileSet := lookup(name + "_FILE")
	switch {
	case valueSet && fileSet:
		return nil, fmt.Errorf("%s and %s_FILE are both set", name, name)
	case fileSet:
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s_FILE: %w", name, err)
		}
		text := strings.TrimRight(string(data), "\r\n")
		return &text, nil
	case valueSet:
		return &value, nil
	default:
		return nil, nil
	}
}

// defaults returns the configuration with every documented default applied.
func defaults() *Config {
	return &Config{
		Listen:   ":8080",
		Location: time.UTC,
		LogLevel: slog.LevelInfo,
		Docker: DockerConfig{
			Host: "unix:///var/run/docker.sock",
		},
		Host: HostConfig{
			Root:    "/host/root",
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
			Telegram: TelegramConfig{
				APIURL: "https://api.telegram.org",
			},
			SMS: SMSConfig{
				HTTP: SMSHTTPConfig{
					Method:   "POST",
					Encoding: "json",
				},
			},
			Silent: SilentConfig{
				QuietPageCritical: true,
				PagerCooldown:     30 * time.Minute,
				StandDownAfter:    24 * time.Hour,
				DeploySilence:     5 * time.Minute,
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
}

// finalize fills in the values that depend on other values: the host name and
// the default disk list.
func (c *Config) finalize() error {
	if c.HostName == "" {
		name, err := detectHostName(c.Host.Root)
		if err != nil {
			return err
		}
		c.HostName = name
	}
	if len(c.Host.Disks) == 0 {
		c.Host.Disks = []Disk{{Name: "/", Path: c.Host.Root}}
	}
	if c.Notify.Silent.QuietZone == nil {
		c.Notify.Silent.QuietZone = c.Location
	}
	return nil
}

// detectHostName reads the host name from <RAEDAR_HOST_ROOT>/etc/hostname,
// falling back to the process host name.
func detectHostName(hostRoot string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(hostRoot, "etc/hostname")); err == nil {
		if name := firstLine(string(data)); name != "" {
			return name, nil
		}
	}
	name, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("RAEDAR_HOST_NAME: cannot determine the host name: %w", err)
	}
	if name == "" {
		return "", errors.New("RAEDAR_HOST_NAME: cannot determine the host name")
	}
	return name, nil
}

// firstLine returns the first line of s with surrounding whitespace removed.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
