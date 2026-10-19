package config

import "testing"

func TestUsesProxy(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		target string
		want   bool
	}{
		{name: "no proxy", config: Config{}, target: ProxyTargetTelegram},
		{
			name:   "every request",
			config: Config{Notify: NotifyConfig{OutboundProxy: "http://proxy.example.com:3128"}},
			target: ProxyTargetHeartbeat,
			want:   true,
		},
		{
			name: "selected request",
			config: Config{Notify: NotifyConfig{
				OutboundProxy: "http://proxy.example.com:3128",
				ProxyFor:      []string{ProxyTargetTelegram, ProxyTargetWebhook},
			}},
			target: ProxyTargetWebhook,
			want:   true,
		},
		{
			name: "other request",
			config: Config{Notify: NotifyConfig{
				OutboundProxy: "http://proxy.example.com:3128",
				ProxyFor:      []string{ProxyTargetTelegram},
			}},
			target: ProxyTargetSMS,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.UsesProxy(tt.target); got != tt.want {
				t.Errorf("UsesProxy(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}
