package config

import (
	"strings"
	"testing"
	"time"
)

// checkConfig loads a configuration with the given overrides and asserts that
// it either loads or fails with an error containing wantErr.
func checkConfig(t *testing.T, extras map[string]string, wantErr string) {
	t.Helper()
	_, err := loadWithExtras(t, extras)
	if wantErr == "" {
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		return
	}
	assertErrorContains(t, err, wantErr)
}

func TestValidateRules(t *testing.T) {
	tests := []struct {
		name    string
		extras  map[string]string
		wantErr string
	}{
		{name: "default thresholds agree"},
		{
			name:    "disk critical below warning",
			extras:  map[string]string{"RAEDAR_DISK_WARN": "90", "RAEDAR_DISK_CRIT": "80"},
			wantErr: "RAEDAR_DISK_CRIT must not be lower than RAEDAR_DISK_WARN",
		},
		{
			name:   "disk critical equal to warning",
			extras: map[string]string{"RAEDAR_DISK_WARN": "90", "RAEDAR_DISK_CRIT": "90"},
		},
		{
			name:    "certificate critical longer than warning",
			extras:  map[string]string{"RAEDAR_CERT_WARN": "3d", "RAEDAR_CERT_CRIT": "14d"},
			wantErr: "RAEDAR_CERT_CRIT must not be longer than RAEDAR_CERT_WARN",
		},
		{
			name:   "certificate critical equal to warning",
			extras: map[string]string{"RAEDAR_CERT_WARN": "3d", "RAEDAR_CERT_CRIT": "3d"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkConfig(t, tt.extras, tt.wantErr)
		})
	}
}

func TestValidateNotify(t *testing.T) {
	tests := []struct {
		name    string
		extras  map[string]string
		wantErr string
	}{
		{name: "nothing configured"},
		{
			name:    "telegram token without chat IDs",
			extras:  map[string]string{"RAEDAR_TELEGRAM_TOKEN": "123456:abcdef"},
			wantErr: "RAEDAR_TELEGRAM_CHAT_IDS is required when RAEDAR_TELEGRAM_TOKEN is set",
		},
		{
			name:    "telegram chat IDs without token",
			extras:  map[string]string{"RAEDAR_TELEGRAM_CHAT_IDS": "123456789"},
			wantErr: "RAEDAR_TELEGRAM_TOKEN is required when RAEDAR_TELEGRAM_CHAT_IDS is set",
		},
		{
			name: "telegram complete",
			extras: map[string]string{
				"RAEDAR_TELEGRAM_TOKEN":    "123456:abcdef",
				"RAEDAR_TELEGRAM_CHAT_IDS": "123456789",
			},
		},
		{
			name:    "SMS recipients without URL",
			extras:  map[string]string{"RAEDAR_SMS_TO": "+989120000000"},
			wantErr: "RAEDAR_SMS_HTTP_URL is required when RAEDAR_SMS_TO is set",
		},
		{
			name:    "SMS URL without recipients",
			extras:  map[string]string{"RAEDAR_SMS_HTTP_URL": "https://sms.example.com/send"},
			wantErr: "RAEDAR_SMS_TO is required when RAEDAR_SMS_HTTP_URL is set",
		},
		{
			name:   "SMS complete",
			extras: smsHTTPEnv(),
		},
		{
			name:    "webhook secret without URL",
			extras:  map[string]string{"RAEDAR_WEBHOOK_SECRET": "s3cret"},
			wantErr: "RAEDAR_WEBHOOK_SECRET requires RAEDAR_WEBHOOK_URL",
		},
		{
			name:    "proxy scope without proxy",
			extras:  map[string]string{"RAEDAR_OUTBOUND_PROXY_FOR": "sms"},
			wantErr: "RAEDAR_OUTBOUND_PROXY_FOR requires RAEDAR_OUTBOUND_PROXY",
		},
		{
			name: "proxy scope with proxy",
			extras: map[string]string{
				"RAEDAR_OUTBOUND_PROXY":     "http://proxy.example.com:3128",
				"RAEDAR_OUTBOUND_PROXY_FOR": "telegram,sms",
			},
		},
		{
			name: "webhook complete",
			extras: map[string]string{
				"RAEDAR_WEBHOOK_URL":    "https://hooks.example.com/raedar",
				"RAEDAR_WEBHOOK_SECRET": "s3cret",
			},
		},
		{
			name:    "digest without channels",
			extras:  map[string]string{"RAEDAR_DIGEST_CHANNELS": " , "},
			wantErr: "RAEDAR_DIGEST_CHANNELS must not be empty when RAEDAR_DIGEST_AT is set",
		},
		{
			name: "disabled digest without channels",
			extras: map[string]string{
				"RAEDAR_DIGEST_AT":       "",
				"RAEDAR_DIGEST_CHANNELS": " , ",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkConfig(t, tt.extras, tt.wantErr)
		})
	}
}

func TestValidateHeartbeat(t *testing.T) {
	tests := []struct {
		name    string
		extras  map[string]string
		wantErr string
	}{
		{name: "nothing configured"},
		{
			name:    "peers without receive token",
			extras:  map[string]string{"RAEDAR_HEARTBEAT_PEERS": "host-2"},
			wantErr: "RAEDAR_HEARTBEAT_RECEIVE_TOKEN is required when RAEDAR_HEARTBEAT_PEERS is set",
		},
		{
			name:    "receive token without peers",
			extras:  map[string]string{"RAEDAR_HEARTBEAT_RECEIVE_TOKEN": "token"},
			wantErr: "RAEDAR_HEARTBEAT_PEERS is required when RAEDAR_HEARTBEAT_RECEIVE_TOKEN is set",
		},
		{
			name: "receiver complete",
			extras: map[string]string{
				"RAEDAR_HEARTBEAT_PEERS":         "host-2,host-3",
				"RAEDAR_HEARTBEAT_RECEIVE_TOKEN": "token",
			},
		},
		{
			name: "duplicate peers",
			extras: map[string]string{
				"RAEDAR_HEARTBEAT_PEERS":         "host-2, host-2",
				"RAEDAR_HEARTBEAT_RECEIVE_TOKEN": "token",
			},
			wantErr: "RAEDAR_HEARTBEAT_PEERS lists a peer twice",
		},
		{
			name:    "header without URL",
			extras:  map[string]string{"RAEDAR_HEARTBEAT_HEADER": "Authorization: Bearer token"},
			wantErr: "RAEDAR_HEARTBEAT_HEADER requires RAEDAR_HEARTBEAT_URL",
		},
		{
			name: "sender complete",
			extras: map[string]string{
				"RAEDAR_HEARTBEAT_URL":    "https://hb.example.com/host-1",
				"RAEDAR_HEARTBEAT_HEADER": "Authorization: Bearer token",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkConfig(t, tt.extras, tt.wantErr)
		})
	}
}

func TestValidateQuietWindow(t *testing.T) {
	base := databaseEnv()
	from := "22:00"
	onlyFrom := map[string]string{"RAEDAR_QUIET_FROM": from}
	if _, err := Load(lookupFrom(mergeEnv(base, onlyFrom))); err == nil ||
		!strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("one-sided quiet window: err = %v", err)
	}
	both := map[string]string{"RAEDAR_QUIET_FROM": "22:00", "RAEDAR_QUIET_TO": "07:00"}
	if _, err := Load(lookupFrom(mergeEnv(base, both))); err != nil {
		t.Fatalf("valid quiet window: %v", err)
	}
	same := map[string]string{"RAEDAR_QUIET_FROM": "08:00", "RAEDAR_QUIET_TO": "08:00"}
	if _, err := Load(lookupFrom(mergeEnv(base, same))); err == nil ||
		!strings.Contains(err.Error(), "must differ") {
		t.Fatalf("zero-length quiet window: err = %v", err)
	}
}

func mergeEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestSilentDefaultsAndZero(t *testing.T) {
	env := mergeEnv(databaseEnv(), map[string]string{
		"RAEDAR_TZ":                     "Asia/Tehran",
		"RAEDAR_PAGER_COOLDOWN":         "0",
		"RAEDAR_STAND_DOWN_AFTER":       "0",
		"RAEDAR_DEPLOY_SILENCE":         "0",
		"RAEDAR_PAGER_COOLDOWN_BY_RULE": "route.down=0,host.disk=1d",
	})
	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	silent := cfg.Notify.Silent
	if silent.QuietZone.String() != "Asia/Tehran" {
		t.Errorf("QuietZone = %q, want the RAEDAR_TZ zone", silent.QuietZone)
	}
	if silent.PagerCooldown != 0 || silent.StandDownAfter != 0 || silent.DeploySilence != 0 {
		t.Errorf("zero values not kept: %+v", silent)
	}
	if got := silent.PagerCooldownByRule; got["route.down"] != 0 || got["host.disk"] != 24*time.Hour {
		t.Errorf("PagerCooldownByRule = %v", got)
	}
}

func TestCooldownByRuleRejectsGarbage(t *testing.T) {
	env := mergeEnv(databaseEnv(), map[string]string{"RAEDAR_PAGER_COOLDOWN_BY_RULE": "route.down=soon"})
	if _, err := Load(lookupFrom(env)); err == nil || strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("err = %v, want a descriptive duration error", err)
	}
}
