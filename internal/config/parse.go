package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// parseString keeps a string value as it is.
func parseString(s string) (string, error) {
	return s, nil
}

// checkedString returns a parser that validates a string without changing it.
func checkedString(check func(string) error) func(string) (string, error) {
	return func(s string) (string, error) {
		return s, check(s)
	}
}

// oneOfFold returns a parser that accepts one of the allowed values, ignoring
// case, and returns the allowed spelling.
func oneOfFold(allowed ...string) func(string) (string, error) {
	return func(s string) (string, error) {
		for _, option := range allowed {
			if strings.EqualFold(s, option) {
				return option, nil
			}
		}
		return "", fmt.Errorf("must be one of %s", strings.Join(allowed, ", "))
	}
}

// parseIntAtLeast returns a parser for a whole number of at least min.
func parseIntAtLeast(min int) func(string) (int, error) {
	return func(s string) (int, error) {
		number, err := strconv.Atoi(s)
		if err != nil {
			return 0, errors.New("must be a whole number")
		}
		if number < min {
			return 0, fmt.Errorf("must be at least %d", min)
		}
		return number, nil
	}
}

// parseFactor parses a multiplication factor of at least 1.
func parseFactor(s string) (float64, error) {
	factor, rest, err := parseDecimalPrefix(s)
	if err != nil || rest != "" || factor < 1 {
		return 0, errors.New("must be a number of at least 1, for example 3 or 2.5")
	}
	return factor, nil
}

// parseDuration parses a duration such as 300ms, 90s, 5m, 6h, 14d or 1h30m.
// The unit is required and the duration must be greater than zero.
func parseDuration(s string) (time.Duration, error) {
	syntax := errors.New("must be a duration, for example 300ms, 90s, 5m, 6h or 14d")
	if s == "" {
		return 0, syntax
	}
	rest := s
	var total time.Duration
	for rest != "" {
		number, remainder, err := parseDecimalPrefix(rest)
		if err != nil {
			return 0, syntax
		}
		unit, remainder, ok := durationUnit(remainder)
		if !ok {
			return 0, syntax
		}
		part := math.Round(number * float64(unit))
		if part <= 0 {
			return 0, errors.New("must be greater than zero")
		}
		if part > float64(math.MaxInt64) || total > time.Duration(math.MaxInt64)-time.Duration(part) {
			return 0, errors.New("is too large")
		}
		total += time.Duration(part)
		rest = remainder
	}
	return total, nil
}

// parseDecimalPrefix consumes a decimal number at the start of s and returns
// it together with the rest of the string.
func parseDecimalPrefix(s string) (float64, string, error) {
	end := 0
	for end < len(s) && isDigit(s[end]) {
		end++
	}
	if end < len(s) && s[end] == '.' {
		fraction := end + 1
		for fraction < len(s) && isDigit(s[fraction]) {
			fraction++
		}
		if fraction == end+1 {
			return 0, s, errors.New("invalid number")
		}
		end = fraction
	}
	if end == 0 {
		return 0, s, errors.New("invalid number")
	}
	value, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, s, err
	}
	return value, s[end:], nil
}

// durationUnit matches a duration unit at the start of s. Milliseconds are
// matched before minutes.
func durationUnit(s string) (time.Duration, string, bool) {
	switch {
	case strings.HasPrefix(s, "ms"):
		return time.Millisecond, s[2:], true
	case strings.HasPrefix(s, "s"):
		return time.Second, s[1:], true
	case strings.HasPrefix(s, "m"):
		return time.Minute, s[1:], true
	case strings.HasPrefix(s, "h"):
		return time.Hour, s[1:], true
	case strings.HasPrefix(s, "d"):
		return 24 * time.Hour, s[1:], true
	}
	return 0, s, false
}

// parsePercent parses a percentage with or without a percent sign, for
// example 85, 85% or 2.5%. The value must be between 0 and 100.
func parsePercent(s string) (Percent, error) {
	syntax := errors.New("must be a percentage between 0 and 100, for example 85, 85% or 2.5%")
	value, rest, err := parseDecimalPrefix(strings.TrimSuffix(s, "%"))
	if err != nil || rest != "" || value > 100 {
		return 0, syntax
	}
	return Percent(value), nil
}

// parseTimeOfDay parses a time of day in HH:MM form, between 00:00 and 23:59.
func parseTimeOfDay(s string) (TimeOfDay, error) {
	syntax := errors.New("must be a time of day in HH:MM form, for example 09:00")
	if len(s) != 5 || s[2] != ':' {
		return TimeOfDay{}, syntax
	}
	for i := 0; i < len(s); i++ {
		if i != 2 && !isDigit(s[i]) {
			return TimeOfDay{}, syntax
		}
	}
	hour := int(s[0]-'0')*10 + int(s[1]-'0')
	minute := int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || minute > 59 {
		return TimeOfDay{}, syntax
	}
	return TimeOfDay{Hour: hour, Minute: minute}, nil
}

// parseList splits a comma-separated list, trimming spaces around items and
// dropping empty items.
func parseList(s string) []string {
	var items []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// parseListChecked returns a parser for a comma-separated list whose items are
// checked.
func parseListChecked(check func(string) error) func(string) ([]string, error) {
	return func(s string) ([]string, error) {
		items := parseList(s)
		for _, item := range items {
			if err := check(item); err != nil {
				return nil, err
			}
		}
		return items, nil
	}
}

// parseProxyFor parses the outbound requests that use the proxy: all, or a
// comma-separated subset of telegram, sms, webhook and heartbeat.
func parseProxyFor(s string) ([]string, error) {
	items := parseList(s)
	if len(items) == 1 && strings.EqualFold(items[0], "all") {
		return nil, nil
	}
	var targets []string
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if strings.EqualFold(item, "all") {
			return nil, errors.New("must be all alone or a comma-separated list of telegram, sms, webhook and heartbeat")
		}
		target, err := oneOfFold(ProxyTargetTelegram, ProxyTargetSMS, ProxyTargetWebhook, ProxyTargetHeartbeat)(item)
		if err != nil {
			return nil, err
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, target)
	}
	return targets, nil
}

// parseChannels parses a list of notification channels: telegram, sms or
// webhook.
func parseChannels(s string) ([]string, error) {
	items := parseList(s)
	for i, item := range items {
		channel, err := oneOfFold("telegram", "sms", "webhook")(item)
		if err != nil {
			return nil, err
		}
		items[i] = channel
	}
	return items, nil
}

// parseHeaders parses one Name: value header per line.
func parseHeaders(s string) ([]Header, error) {
	var headers []Header
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, errors.New("must have one Name: value header per line")
		}
		headers = append(headers, Header{Name: name, Value: strings.TrimSpace(value)})
	}
	return headers, nil
}

// parseOneHeader parses a single Name: value header.
func parseOneHeader(s string) (*Header, error) {
	headers, err := parseHeaders(s)
	if err != nil {
		return nil, err
	}
	if len(headers) != 1 {
		return nil, errors.New("must be one Name: value header")
	}
	return &headers[0], nil
}

// parseHTTPURL checks that s is an absolute http or https URL and returns it
// without trailing slashes.
func parseHTTPURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return "", errors.New("must be an absolute http:// or https:// URL")
	}
	return strings.TrimRight(s, "/"), nil
}

// checkSMSURL checks a generic SMS request URL, which may contain the {{to}}
// and {{message}} placeholders.
func checkSMSURL(s string) error {
	probe := strings.NewReplacer("{{to}}", "1", "{{message}}", "text").Replace(s)
	_, err := parseHTTPURL(probe)
	return err
}

// checkProxyURL checks an outbound proxy URL: http:// or socks5://.
func checkProxyURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "socks5")) {
		return errors.New("must be an http:// or socks5:// URL")
	}
	return nil
}

// checkDockerHost checks a Docker API address: unix:///var/run/docker.sock or
// tcp://host:2375.
func checkDockerHost(s string) error {
	syntax := errors.New("must be a unix:// or tcp:// address, for example unix:///var/run/docker.sock")
	u, err := url.Parse(s)
	if err != nil {
		return syntax
	}
	switch {
	case strings.EqualFold(u.Scheme, "unix") && u.Path != "":
		return nil
	case strings.EqualFold(u.Scheme, "tcp") && u.Host != "":
		return nil
	}
	return syntax
}

// checkListenAddr checks an address the web server can listen on, for example
// :8080 or 127.0.0.1:8080.
func checkListenAddr(s string) error {
	return checkHostPort(s, false)
}

// checkTLSCheckAddr checks a host:port address, for example traefik:443.
func checkTLSCheckAddr(s string) error {
	return checkHostPort(s, true)
}

// checkHostPort checks a host:port address with a numeric port.
func checkHostPort(s string, requireHost bool) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return errors.New("must be an address in host:port form, for example :8080")
	}
	if requireHost && host == "" {
		return errors.New("must include a host")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("must use a port between 1 and 65535")
	}
	return nil
}

// checkAbsolutePath checks that a path starts at the filesystem root.
func checkAbsolutePath(s string) error {
	if !filepath.IsAbs(s) {
		return errors.New("must be an absolute path")
	}
	return nil
}

// checkContainerName checks a Docker container name.
func checkContainerName(s string) error {
	if strings.ContainsAny(s, " \t/") {
		return errors.New("must be a container name without spaces or slashes")
	}
	return nil
}

// checkHostName checks the host name that appears in the UI and in messages.
func checkHostName(s string) error {
	if strings.ContainsAny(s, " \t") {
		return errors.New("must not contain spaces")
	}
	return nil
}

// checkDomain checks a domain name such as example.com or *.example.com.
func checkDomain(s string) error {
	name := strings.TrimPrefix(s, "*.")
	for _, label := range strings.Split(name, ".") {
		if !validLabel(label) {
			return errors.New("must be a domain name, for example example.com")
		}
	}
	return nil
}

// validLabel reports whether one domain label carries letters, digits or
// hyphens only, and does not start or end with a hyphen.
func validLabel(label string) bool {
	if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for i := 0; i < len(label); i++ {
		b := label[i]
		if !isDigit(b) && !isLetter(b) && b != '-' {
			return false
		}
	}
	return true
}

// checkPeerName checks the name of a heartbeat peer.
func checkPeerName(s string) error {
	if strings.ContainsAny(s, " \t/:") {
		return errors.New("must be a peer name without spaces, slashes or colons, for example host-2")
	}
	return nil
}

// checkUnitPattern checks a systemd unit name or wildcard pattern.
func checkUnitPattern(s string) error {
	if strings.ContainsAny(s, " \t/") {
		return errors.New("must be a unit name or pattern without spaces or slashes, for example apt-daily*.service")
	}
	return nil
}

// checkChatID checks a Telegram chat ID: a number, optionally negative, or an
// @username.
func checkChatID(s string) error {
	message := errors.New("must be a chat ID number or an @username")
	if username, ok := strings.CutPrefix(s, "@"); ok {
		if username == "" {
			return message
		}
		for i := 0; i < len(username); i++ {
			b := username[i]
			if !isDigit(b) && !isLetter(b) && b != '_' {
				return message
			}
		}
		return nil
	}
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return message
	}
	for i := 0; i < len(digits); i++ {
		if !isDigit(digits[i]) {
			return message
		}
	}
	return nil
}

// checkPhoneNumber checks a phone number: digits with an optional leading +.
func checkPhoneNumber(s string) error {
	message := errors.New("must be a phone number, for example +989120000000")
	digits := strings.TrimPrefix(s, "+")
	if digits == "" {
		return message
	}
	for i := 0; i < len(digits); i++ {
		if !isDigit(digits[i]) {
			return message
		}
	}
	return nil
}

// parseDatabaseURL checks a PostgreSQL connection URL or keyword/value string.
func parseDatabaseURL(s string) (string, error) {
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || (!strings.EqualFold(u.Scheme, "postgres") && !strings.EqualFold(u.Scheme, "postgresql")) {
			return "", errors.New("must be a PostgreSQL URL, for example postgres://raedar:password@postgres:5432/raedar?sslmode=disable")
		}
	}
	return s, nil
}

// parseLogLevel parses a log level name.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, errors.New("must be debug, info, warn or error")
}

// parseLocation parses an IANA time zone name.
func parseLocation(s string) (*time.Location, error) {
	location, err := time.LoadLocation(s)
	if err != nil {
		return nil, errors.New("must be an IANA time zone, for example Asia/Tehran")
	}
	return location, nil
}

// parseSystemdMode parses auto, on or off.
func parseSystemdMode(s string) (SystemdMode, error) {
	value, err := oneOfFold("auto", "on", "off")(s)
	if err != nil {
		return "", err
	}
	return SystemdMode(value), nil
}

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// isLetter reports whether b is an ASCII letter.
func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
