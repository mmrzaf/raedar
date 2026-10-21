package config

import "errors"

// validate reports every problem that involves more than one variable.
func (c *Config) validate() []error {
	var problems []error
	problems = append(problems, validateRules(c.Rules)...)
	problems = append(problems, validateHost(c.Host)...)
	problems = append(problems, validateNotify(c.Notify)...)
	problems = append(problems, validateHeartbeat(c.Heartbeat)...)
	return problems
}

// validateRules checks the thresholds against each other.
func validateRules(r RulesConfig) []error {
	var problems []error
	if r.DiskCrit < r.DiskWarn {
		problems = append(problems, errors.New("RAEDAR_DISK_CRIT must not be lower than RAEDAR_DISK_WARN"))
	}
	if r.CertCrit > r.CertWarn {
		problems = append(problems, errors.New("RAEDAR_CERT_CRIT must not be longer than RAEDAR_CERT_WARN"))
	}
	return problems
}

// validateHost checks the disk list.
func validateHost(h HostConfig) []error {
	names := make(map[string]bool, len(h.Disks))
	for _, disk := range h.Disks {
		if names[disk.Name] {
			return []error{errors.New("RAEDAR_DISKS: two entries have the same name")}
		}
		names[disk.Name] = true
	}
	return nil
}

// validateNotify checks that each configured channel is complete.
func validateNotify(n NotifyConfig) []error {
	var problems []error
	if n.Telegram.Token != "" && len(n.Telegram.ChatIDs) == 0 {
		problems = append(problems, errors.New("RAEDAR_TELEGRAM_CHAT_IDS is required when RAEDAR_TELEGRAM_TOKEN is set"))
	}
	if n.Telegram.Token == "" && len(n.Telegram.ChatIDs) > 0 {
		problems = append(problems, errors.New("RAEDAR_TELEGRAM_TOKEN is required when RAEDAR_TELEGRAM_CHAT_IDS is set"))
	}
	if len(n.SMS.To) > 0 && n.SMS.HTTP.URL == "" {
		problems = append(problems, errors.New("RAEDAR_SMS_HTTP_URL is required when RAEDAR_SMS_TO is set"))
	}
	if n.SMS.HTTP.URL != "" && len(n.SMS.To) == 0 {
		problems = append(problems, errors.New("RAEDAR_SMS_TO is required when RAEDAR_SMS_HTTP_URL is set"))
	}
	if len(n.ProxyFor) > 0 && n.OutboundProxy == "" {
		problems = append(problems, errors.New("RAEDAR_OUTBOUND_PROXY_FOR requires RAEDAR_OUTBOUND_PROXY"))
	}
	if n.Webhook.Secret != "" && n.Webhook.URL == "" {
		problems = append(problems, errors.New("RAEDAR_WEBHOOK_SECRET requires RAEDAR_WEBHOOK_URL"))
	}
	if n.DigestAt != nil && len(n.DigestChannels) == 0 {
		problems = append(problems, errors.New("RAEDAR_DIGEST_CHANNELS must not be empty when RAEDAR_DIGEST_AT is set"))
	}
	return problems
}

// validateHeartbeat checks the sender and receiver settings against each
// other.
func validateHeartbeat(h HeartbeatConfig) []error {
	var problems []error
	if h.Header != nil && h.URL == "" {
		problems = append(problems, errors.New("RAEDAR_HEARTBEAT_HEADER requires RAEDAR_HEARTBEAT_URL"))
	}
	peers := make(map[string]bool, len(h.Peers))
	for _, peer := range h.Peers {
		if peers[peer] {
			problems = append(problems, errors.New("RAEDAR_HEARTBEAT_PEERS lists a peer twice"))
			break
		}
		peers[peer] = true
	}
	if len(h.Peers) > 0 && h.ReceiveToken == "" {
		problems = append(problems, errors.New("RAEDAR_HEARTBEAT_RECEIVE_TOKEN is required when RAEDAR_HEARTBEAT_PEERS is set"))
	}
	if len(h.Peers) == 0 && h.ReceiveToken != "" {
		problems = append(problems, errors.New("RAEDAR_HEARTBEAT_PEERS is required when RAEDAR_HEARTBEAT_RECEIVE_TOKEN is set"))
	}
	return problems
}
