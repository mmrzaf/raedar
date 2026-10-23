package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// parseCase is one input for a parser test.
type parseCase[T any] struct {
	name    string
	value   string
	want    T
	wantErr string
}

// runParseCases runs table-driven parser tests, asserting the parsed value or
// that the error mentions wantErr.
func runParseCases[T any](t *testing.T, parse func(string) (T, error), cases []parseCase[T]) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.value)
			if tt.wantErr != "" {
				assertErrorContains(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("parse(%q) error = %v", tt.value, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parse(%q) = %#v, want %#v", tt.value, got, tt.want)
			}
		})
	}
}

// checkCase is one input for a check function test.
type checkCase struct {
	name    string
	value   string
	wantErr string
}

// runCheckCases runs table-driven tests for a validation function.
func runCheckCases(t *testing.T, check func(string) error, cases []checkCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := check(tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("check(%q) error = %v, want nil", tt.value, err)
				}
				return
			}
			assertErrorContains(t, err, tt.wantErr)
		})
	}
}

// assertErrorContains asserts that err is not nil and mentions want.
func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestParseDuration(t *testing.T) {
	runParseCases(t, parseDuration, []parseCase[time.Duration]{
		{name: "milliseconds", value: "300ms", want: 300 * time.Millisecond},
		{name: "seconds", value: "90s", want: 90 * time.Second},
		{name: "minutes", value: "5m", want: 5 * time.Minute},
		{name: "hours", value: "6h", want: 6 * time.Hour},
		{name: "days", value: "14d", want: 14 * 24 * time.Hour},
		{name: "fractional hours", value: "1.5h", want: 90 * time.Minute},
		{name: "fractional days", value: "2.5d", want: 60 * time.Hour},
		{name: "fractional seconds", value: "0.5s", want: 500 * time.Millisecond},
		{name: "compound minutes and seconds", value: "1m30s", want: 90 * time.Second},
		{name: "compound hours and minutes", value: "1h30m", want: 90 * time.Minute},
		{name: "empty", value: "", wantErr: "must be a duration"},
		{name: "missing unit", value: "5", wantErr: "must be a duration"},
		{name: "unknown unit", value: "5x", wantErr: "must be a duration"},
		{name: "negative", value: "-5m", wantErr: "must be a duration"},
		{name: "uppercase unit", value: "5M", wantErr: "must be a duration"},
		{name: "scientific notation", value: "1e3s", wantErr: "must be a duration"},
		{name: "two dots", value: "1.2.3s", wantErr: "must be a duration"},
		{name: "leading space", value: " 5m", wantErr: "must be a duration"},
		{name: "trailing space", value: "5m ", wantErr: "must be a duration"},
		{name: "zero seconds", value: "0s", wantErr: "greater than zero"},
		{name: "zero days", value: "0d", wantErr: "greater than zero"},
		{name: "rounds to zero", value: "0.0000001ms", wantErr: "greater than zero"},
	})
}

func TestParsePercent(t *testing.T) {
	runParseCases(t, parsePercent, []parseCase[Percent]{
		{name: "plain", value: "85", want: Percent(85)},
		{name: "with sign", value: "85%", want: Percent(85)},
		{name: "fraction", value: "2.5%", want: Percent(2.5)},
		{name: "fraction without sign", value: "2.5", want: Percent(2.5)},
		{name: "zero", value: "0", want: Percent(0)},
		{name: "maximum", value: "100%", want: Percent(100)},
		{name: "above maximum", value: "100.5", wantErr: "between 0 and 100"},
		{name: "negative", value: "-5", wantErr: "between 0 and 100"},
		{name: "sign only", value: "%", wantErr: "between 0 and 100"},
		{name: "double sign", value: "85%%", wantErr: "between 0 and 100"},
		{name: "empty", value: "", wantErr: "between 0 and 100"},
		{name: "scientific notation", value: "1e2", wantErr: "between 0 and 100"},
		{name: "leading space", value: " 85", wantErr: "between 0 and 100"},
	})
}

func TestParseTimeOfDay(t *testing.T) {
	runParseCases(t, parseTimeOfDay, []parseCase[TimeOfDay]{
		{name: "morning", value: "09:00", want: TimeOfDay{Hour: 9}},
		{name: "midnight", value: "00:00", want: TimeOfDay{}},
		{name: "last minute", value: "23:59", want: TimeOfDay{Hour: 23, Minute: 59}},
		{name: "hour out of range", value: "24:00", wantErr: "HH:MM"},
		{name: "minute out of range", value: "09:60", wantErr: "HH:MM"},
		{name: "single digit hour", value: "9:00", wantErr: "HH:MM"},
		{name: "no colon", value: "0900", wantErr: "HH:MM"},
		{name: "short", value: "09:0", wantErr: "HH:MM"},
		{name: "with seconds", value: "09:00:00", wantErr: "HH:MM"},
		{name: "signed", value: "+9:00", wantErr: "HH:MM"},
		{name: "empty", value: "", wantErr: "HH:MM"},
	})
}

func TestParseList(t *testing.T) {
	runParseCases(t, func(s string) ([]string, error) { return parseList(s), nil }, []parseCase[[]string]{
		{name: "two items", value: "telegram,sms", want: []string{"telegram", "sms"}},
		{name: "spaces around items", value: " a , b ", want: []string{"a", "b"}},
		{name: "empty items dropped", value: "a,,b", want: []string{"a", "b"}},
		{name: "empty", value: "", want: nil},
		{name: "commas only", value: ",,", want: nil},
		{name: "single item", value: "host-2", want: []string{"host-2"}},
	})
}

func TestParseChannels(t *testing.T) {
	runParseCases(t, parseChannels, []parseCase[[]string]{
		{name: "lowercase", value: "telegram,webhook", want: []string{"telegram", "webhook"}},
		{name: "mixed case becomes lowercase", value: "Telegram, WEBHOOK", want: []string{"telegram", "webhook"}},
		{name: "sms", value: "sms", want: []string{"sms"}},
		{name: "none", value: "none", want: nil},
		{name: "none with spaces", value: " NONE ", want: nil},
		{name: "unknown channel", value: "email", wantErr: "must be one of telegram, sms, webhook"},
		{name: "empty", value: "", want: nil},
	})
}

func TestParseProxyFor(t *testing.T) {
	runParseCases(t, parseProxyFor, []parseCase[[]string]{
		{name: "all", value: "all", want: nil},
		{name: "every kind", value: "telegram,SMS, webhook ,heartbeat", want: []string{"telegram", "sms", "webhook", "heartbeat"}},
		{name: "subset", value: "telegram,webhook", want: []string{"telegram", "webhook"}},
		{name: "duplicate", value: "sms,sms", want: []string{"sms"}},
		{name: "unknown", value: "pager", wantErr: "must be one of telegram, sms, webhook, heartbeat"},
		{name: "all with others", value: "all,sms", wantErr: "all alone"},
	})
}

func TestParseHeaders(t *testing.T) {
	runParseCases(t, parseHeaders, []parseCase[[]Header]{
		{name: "one header", value: "Authorization: Bearer token", want: []Header{{Name: "Authorization", Value: "Bearer token"}}},
		{name: "two headers", value: "Authorization: Bearer token\nX-Extra: 1", want: []Header{
			{Name: "Authorization", Value: "Bearer token"},
			{Name: "X-Extra", Value: "1"},
		}},
		{name: "carriage returns", value: "A: 1\r\nB: 2\r\n", want: []Header{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}},
		{name: "blank lines ignored", value: "\nA: 1\n\n", want: []Header{{Name: "A", Value: "1"}}},
		{name: "empty value", value: "X-Empty:", want: []Header{{Name: "X-Empty"}}},
		{name: "empty", value: "", want: nil},
		{name: "missing colon", value: "NoColon", wantErr: "Name: value"},
		{name: "empty name", value: ": value", wantErr: "Name: value"},
		{name: "space in name", value: "Bad Name: value", wantErr: "Name: value"},
	})
}

func TestParseOneHeader(t *testing.T) {
	runParseCases(t, parseOneHeader, []parseCase[*Header]{
		{name: "one header", value: "Authorization: Bearer token", want: &Header{Name: "Authorization", Value: "Bearer token"}},
		{name: "two headers", value: "A: 1\nB: 2", wantErr: "must be one Name: value header"},
		{name: "empty", value: "", wantErr: "must be one Name: value header"},
	})
}

func TestParseHTTPURL(t *testing.T) {
	runParseCases(t, parseHTTPURL, []parseCase[string]{
		{name: "https", value: "https://raedar.example.com", want: "https://raedar.example.com"},
		{name: "trailing slash removed", value: "https://raedar.example.com/", want: "https://raedar.example.com"},
		{name: "http with port and path", value: "http://raedar.example.com:8080/ui", want: "http://raedar.example.com:8080/ui"},
		{name: "uppercase scheme", value: "HTTPS://raedar.example.com", want: "HTTPS://raedar.example.com"},
		{name: "other scheme", value: "ftp://raedar.example.com", wantErr: "http:// or https://"},
		{name: "no scheme", value: "raedar.example.com", wantErr: "http:// or https://"},
		{name: "no host", value: "https://", wantErr: "http:// or https://"},
		{name: "empty", value: "", wantErr: "http:// or https://"},
	})
}

func TestParseDatabaseURL(t *testing.T) {
	runParseCases(t, parseDatabaseURL, []parseCase[string]{
		{name: "postgres URL", value: "postgres://raedar:pw@postgres:5432/raedar?sslmode=disable", want: "postgres://raedar:pw@postgres:5432/raedar?sslmode=disable"},
		{name: "postgresql URL", value: "postgresql://raedar@postgres/raedar", want: "postgresql://raedar@postgres/raedar"},
		{name: "keyword form", value: "host=postgres user=raedar", want: "host=postgres user=raedar"},
		{name: "wrong scheme", value: "mysql://raedar@postgres/raedar", wantErr: "must be a PostgreSQL URL"},
	})
}

func TestParseLogLevel(t *testing.T) {
	runParseCases(t, parseLogLevel, []parseCase[slog.Level]{
		{name: "debug", value: "debug", want: slog.LevelDebug},
		{name: "info", value: "INFO", want: slog.LevelInfo},
		{name: "warn", value: "warn", want: slog.LevelWarn},
		{name: "error", value: "Error", want: slog.LevelError},
		{name: "unknown", value: "warning", wantErr: "debug, info, warn or error"},
		{name: "empty", value: "", wantErr: "debug, info, warn or error"},
	})
}

func TestParseSystemdMode(t *testing.T) {
	runParseCases(t, parseSystemdMode, []parseCase[SystemdMode]{
		{name: "auto", value: "auto", want: SystemdAuto},
		{name: "on", value: "ON", want: SystemdOn},
		{name: "off", value: "off", want: SystemdOff},
		{name: "unknown", value: "maybe", wantErr: "must be one of auto, on, off"},
	})
}

func TestParseIntAtLeast(t *testing.T) {
	runParseCases(t, parseIntAtLeast(1), []parseCase[int]{
		{name: "positive", value: "20", want: 20},
		{name: "one", value: "1", want: 1},
		{name: "zero", value: "0", wantErr: "must be at least 1"},
		{name: "negative", value: "-3", wantErr: "must be at least 1"},
		{name: "not a number", value: "abc", wantErr: "must be a whole number"},
		{name: "fraction", value: "2.5", wantErr: "must be a whole number"},
	})
}

func TestParseFactor(t *testing.T) {
	runParseCases(t, parseFactor, []parseCase[float64]{
		{name: "whole", value: "3", want: 3},
		{name: "one", value: "1", want: 1},
		{name: "fraction", value: "2.5", want: 2.5},
		{name: "below one", value: "0.5", wantErr: "at least 1"},
		{name: "negative", value: "-2", wantErr: "at least 1"},
		{name: "trailing text", value: "3 times", wantErr: "at least 1"},
		{name: "empty", value: "", wantErr: "at least 1"},
	})
}

func TestCheckListenAddr(t *testing.T) {
	runCheckCases(t, checkListenAddr, []checkCase{
		{name: "all interfaces", value: ":8080"},
		{name: "ipv4", value: "127.0.0.1:8080"},
		{name: "hostname", value: "localhost:80"},
		{name: "ipv6", value: "[::1]:8080"},
		{name: "no port", value: "8080", wantErr: "host:port"},
		{name: "empty", value: "", wantErr: "host:port"},
		{name: "port zero", value: ":0", wantErr: "between 1 and 65535"},
		{name: "port too high", value: ":65536", wantErr: "between 1 and 65535"},
		{name: "named port", value: "localhost:http", wantErr: "between 1 and 65535"},
	})
}

func TestCheckTLSCheckAddr(t *testing.T) {
	runCheckCases(t, checkTLSCheckAddr, []checkCase{
		{name: "host and port", value: "traefik:443"},
		{name: "no host", value: ":443", wantErr: "must include a host"},
		{name: "no port", value: "traefik", wantErr: "host:port"},
	})
}

func TestCheckDockerHost(t *testing.T) {
	runCheckCases(t, checkDockerHost, []checkCase{
		{name: "unix socket", value: "unix:///var/run/docker.sock"},
		{name: "tcp with port", value: "tcp://socket-proxy:2375"},
		{name: "tcp without port", value: "tcp://socket-proxy"},
		{name: "empty unix path", value: "unix://", wantErr: "unix:// or tcp://"},
		{name: "http scheme", value: "http://socket-proxy:2375", wantErr: "unix:// or tcp://"},
		{name: "plain word", value: "socket-proxy", wantErr: "unix:// or tcp://"},
		{name: "empty", value: "", wantErr: "unix:// or tcp://"},
	})
}

func TestCheckProxyURL(t *testing.T) {
	runCheckCases(t, checkProxyURL, []checkCase{
		{name: "http proxy", value: "http://proxy.example.com:3128"},
		{name: "socks5 proxy", value: "socks5://proxy.example.com:1080"},
		{name: "socks4", value: "socks4://proxy.example.com:1080", wantErr: "http:// or socks5://"},
		{name: "no scheme", value: "proxy.example.com:3128", wantErr: "http:// or socks5://"},
		{name: "empty", value: "", wantErr: "http:// or socks5://"},
	})
}

func TestCheckSMSURL(t *testing.T) {
	runCheckCases(t, checkSMSURL, []checkCase{
		{name: "plain URL", value: "https://sms.example.com/v1/send"},
		{name: "placeholders in URL", value: "https://sms.example.com/send?to={{to}}&text={{message}}"},
		{name: "wrong scheme", value: "ftp://sms.example.com", wantErr: "http:// or https://"},
		{name: "no scheme", value: "sms.example.com", wantErr: "http:// or https://"},
	})
}

func TestCheckDomain(t *testing.T) {
	runCheckCases(t, checkDomain, []checkCase{
		{name: "simple", value: "example.com"},
		{name: "subdomain", value: "shop.example.com"},
		{name: "wildcard", value: "*.example.com"},
		{name: "single label", value: "example"},
		{name: "uppercase", value: "Example.COM"},
		{name: "hyphen", value: "my-shop.example.com"},
		{name: "with port", value: "example.com:443", wantErr: "domain name"},
		{name: "leading dot", value: ".example.com", wantErr: "domain name"},
		{name: "double dot", value: "example..com", wantErr: "domain name"},
		{name: "leading hyphen", value: "-example.com", wantErr: "domain name"},
		{name: "trailing hyphen", value: "example-.com", wantErr: "domain name"},
		{name: "space", value: "exa mple.com", wantErr: "domain name"},
		{name: "underscore", value: "exa_mple.com", wantErr: "domain name"},
		{name: "empty", value: "", wantErr: "domain name"},
	})
}

func TestCheckAbsolutePath(t *testing.T) {
	runCheckCases(t, checkAbsolutePath, []checkCase{
		{name: "root", value: "/"},
		{name: "host root", value: "/host/root"},
		{name: "relative", value: "host/root", wantErr: "absolute path"},
		{name: "empty", value: "", wantErr: "absolute path"},
	})
}

func TestCheckContainerName(t *testing.T) {
	runCheckCases(t, checkContainerName, []checkCase{
		{name: "simple", value: "traefik"},
		{name: "compose name", value: "my-app_web_1"},
		{name: "space", value: "my app", wantErr: "without spaces or slashes"},
		{name: "slash", value: "app/web", wantErr: "without spaces or slashes"},
	})
}

func TestCheckHostName(t *testing.T) {
	runCheckCases(t, checkHostName, []checkCase{
		{name: "simple", value: "host-1"},
		{name: "fqdn", value: "host-1.example.com"},
		{name: "space", value: "host 1", wantErr: "must not contain spaces"},
	})
}

func TestCheckPeerName(t *testing.T) {
	runCheckCases(t, checkPeerName, []checkCase{
		{name: "simple", value: "host-2"},
		{name: "dotted", value: "host-2.example.com"},
		{name: "slash", value: "host/2", wantErr: "without spaces, slashes or colons"},
		{name: "space", value: "host 2", wantErr: "without spaces, slashes or colons"},
		{name: "colon", value: "host:2", wantErr: "without spaces, slashes or colons"},
	})
}

func TestCheckUnitPattern(t *testing.T) {
	runCheckCases(t, checkUnitPattern, []checkCase{
		{name: "unit", value: "man-db.service"},
		{name: "wildcard", value: "apt-daily*.service"},
		{name: "template", value: "backup@daily.service"},
		{name: "space", value: "man db.service", wantErr: "without spaces or slashes"},
		{name: "slash", value: "man/db.service", wantErr: "without spaces or slashes"},
	})
}

func TestCheckChatID(t *testing.T) {
	runCheckCases(t, checkChatID, []checkCase{
		{name: "number", value: "123456789"},
		{name: "group id", value: "-1001234567890"},
		{name: "channel", value: "@channelname"},
		{name: "at sign only", value: "@", wantErr: "chat ID number or an @username"},
		{name: "letters", value: "abc", wantErr: "chat ID number or an @username"},
		{name: "space", value: "12 3", wantErr: "chat ID number or an @username"},
		{name: "minus only", value: "-", wantErr: "chat ID number or an @username"},
		{name: "space in channel", value: "@chan nel", wantErr: "chat ID number or an @username"},
	})
}

func TestCheckPhoneNumber(t *testing.T) {
	runCheckCases(t, checkPhoneNumber, []checkCase{
		{name: "with plus", value: "+989120000000"},
		{name: "plain digits", value: "00989120000000"},
		{name: "space", value: "0912 0000000", wantErr: "phone number"},
		{name: "plus only", value: "+", wantErr: "phone number"},
		{name: "letters", value: "+98912abc", wantErr: "phone number"},
	})
}

func TestPercentString(t *testing.T) {
	tests := []struct {
		name    string
		percent Percent
		want    string
	}{
		{name: "whole", percent: 85, want: "85%"},
		{name: "fraction", percent: 2.5, want: "2.5%"},
		{name: "zero", percent: 0, want: "0%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.percent.String(); got != tt.want {
				t.Errorf("Percent(%v).String() = %q, want %q", float64(tt.percent), got, tt.want)
			}
		})
	}
}

func TestTimeOfDayString(t *testing.T) {
	tests := []struct {
		name string
		at   TimeOfDay
		want string
	}{
		{name: "padded", at: TimeOfDay{Hour: 9}, want: "09:00"},
		{name: "afternoon", at: TimeOfDay{Hour: 21, Minute: 4}, want: "21:04"},
		{name: "midnight", at: TimeOfDay{}, want: "00:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.at.String(); got != tt.want {
				t.Errorf("TimeOfDay%+v.String() = %q, want %q", tt.at, got, tt.want)
			}
		})
	}
}
