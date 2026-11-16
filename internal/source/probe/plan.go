// Package probe runs the optional active availability checks: one HTTP request
// per eligible route, sent to Traefik with the route's hostname, so that an
// idle application is noticed when nobody is calling it. The checks are
// synthetic and stored apart from the passive access-log traffic.
package probe

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mmrzaf/raedar/internal/discovery"
)

// Target is one check: the hostname and path requested through Traefik, and
// the status codes that count as healthy.
type Target struct {
	// Host is the exact domain of the route; it is the Host header and the TLS
	// server name. It is never used as a network address.
	Host string
	// Path is the request path.
	Path string
	// Expect decides which statuses are healthy.
	Expect Expectation
}

// String returns the target for display.
func (t Target) String() string { return t.Host + t.Path }

var (
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	pathMatcher = regexp.MustCompile(`(?:^|[^A-Za-z])Path(?:Prefix)?\(\x60(/[^\x60]*)\x60\)`)
	// unsupported are matchers a plain GET cannot satisfy.
	unsupported = regexp.MustCompile(`(?:Header|HeaderRegexp|Query|QueryRegexp|ClientIP|Method)\(`)
)

// Plan decides what to check for a route. It returns a target, or the reason
// the route is not checked, which the interface shows instead of an empty
// history. Only routes Raedar discovered from Docker labels with an exact
// hostname are eligible, so the set of things that can be requested is
// bounded by what the operator already routes through Traefik.
func Plan(route *discovery.Route, defaultPath string) (Target, string) {
	app := route.App
	switch {
	case route.Kind != "http":
		return Target{}, "not an HTTP route"
	case route.App == nil:
		return Target{}, "the route belongs to no app"
	case discovery.IsCatchAllRoute(route.Name):
		return Target{}, "catch-all route"
	case app.ProbeOff:
		return Target{}, "disabled by raedar.probe=false"
	case !route.TLS:
		return Target{}, "the route has no TLS, so Raedar cannot reach it through Traefik's HTTPS address"
	case unsupported.MatchString(route.Rule):
		return Target{}, "the rule needs a header, query, client IP or method a plain request does not have"
	}
	host := ""
	for _, domain := range route.Domains {
		domain = strings.ToLower(domain)
		if hostPattern.MatchString(domain) {
			host = domain
			break
		}
	}
	if host == "" {
		return Target{}, "the rule has no exact hostname"
	}
	path := defaultPath
	if match := pathMatcher.FindStringSubmatch(route.Rule); match != nil {
		path = match[1]
	}
	if app.ProbePath != "" {
		path = app.ProbePath
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, " \t\r\n?#\\") {
		return Target{}, fmt.Sprintf("invalid probe path %q", path)
	}
	expect := DefaultExpectation
	if app.ProbeExpect != "" {
		parsed, err := ParseExpectation(app.ProbeExpect)
		if err != nil {
			return Target{}, fmt.Sprintf("invalid raedar.probe.expect: %v", err)
		}
		expect = parsed
	}
	return Target{Host: host, Path: path, Expect: expect}, ""
}

// Expectation is the set of HTTP statuses that count as healthy.
type Expectation struct {
	ranges [][2]int
}

// DefaultExpectation accepts every status below 500. A protected or
// authenticated route that answers 401 or 403 is alive, so it is not a
// failure; a 5xx means the application or its backend is broken.
var DefaultExpectation = Expectation{ranges: [][2]int{{100, 499}}}

// ParseExpectation parses a list such as "200", "200-299" or "200,301-302,401".
func ParseExpectation(text string) (Expectation, error) {
	var expect Expectation
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		low, high, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(strings.TrimSpace(low))
		if err != nil {
			return Expectation{}, fmt.Errorf("%q is not a status code or range", part)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(strings.TrimSpace(high)); err != nil {
				return Expectation{}, fmt.Errorf("%q is not a status code or range", part)
			}
		}
		if first < 100 || last > 599 || first > last {
			return Expectation{}, fmt.Errorf("%q is outside 100-599", part)
		}
		expect.ranges = append(expect.ranges, [2]int{first, last})
	}
	return expect, nil
}

// Accepts reports whether the status is expected.
func (e Expectation) Accepts(status int) bool {
	for _, r := range e.ranges {
		if status >= r[0] && status <= r[1] {
			return true
		}
	}
	return false
}
