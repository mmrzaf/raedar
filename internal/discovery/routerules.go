package discovery

import (
	"sort"
	"strconv"
	"strings"
)

// router label prefixes.
const (
	httpRouterPrefix = "traefik.http.routers."
	tcpRouterPrefix  = "traefik.tcp.routers."
	dockerProvider   = "@docker"
)

// routesFromLabels returns the routes a container's Traefik labels define.
func routesFromLabels(labels map[string]string) []Route {
	if isFalse(labels[labelTraefikEnable]) {
		return nil
	}
	byName := make(map[string]*Route)
	for _, key := range sortedKeys(labels) {
		var kind, remainder string
		switch {
		case strings.HasPrefix(key, httpRouterPrefix):
			kind, remainder = "http", strings.TrimPrefix(key, httpRouterPrefix)
		case strings.HasPrefix(key, tcpRouterPrefix):
			kind, remainder = "tcp", strings.TrimPrefix(key, tcpRouterPrefix)
		default:
			continue
		}
		name, field, ok := splitRouterLabel(remainder)
		if !ok || name == "" {
			continue
		}
		routeName := name + dockerProvider
		if existing, ok := byName[routeName]; ok && existing.Kind != kind {
			continue // the same name is defined for both protocols; HTTP wins
		}
		route, ok := byName[routeName]
		if !ok {
			route = &Route{Name: routeName, Kind: kind, Origin: "labels"}
			byName[routeName] = route
		}
		switch field {
		case "rule":
			route.Rule = labels[key]
			route.Domains = ruleDomains(route.Rule)
		case "entrypoints":
			route.Entrypoints = splitList(labels[key])
		case "service":
			route.Service = labels[key]
		case "tls":
			route.TLS = !isFalse(labels[key])
		case "tls.certresolver":
			route.TLS = true
		case "tls.passthrough":
			route.Passthrough = !isFalse(labels[key])
		}
	}
	var routes []Route
	for _, name := range sortedKeys(byName) {
		if byName[name].Rule == "" {
			continue // without a rule there is nothing to monitor
		}
		routes = append(routes, *byName[name])
	}
	return routes
}

// splitRouterLabel separates the router name from the field in a router
// label. It reports false for fields Raedar does not read.
func splitRouterLabel(remainder string) (name, field string, ok bool) {
	for _, field := range []string{"tls.certresolver", "tls.passthrough", "entrypoints", "service", "rule", "tls"} {
		if suffix := "." + field; strings.HasSuffix(remainder, suffix) {
			return strings.TrimSuffix(remainder, suffix), field, true
		}
	}
	return "", "", false
}

// ruleDomains returns the hostnames in the Host and HostSNI matchers of a
// Traefik rule, in the order they appear. HostRegexp, HostSNIRegexp and a
// wildcard HostSNI contribute no domains.
func ruleDomains(rule string) []string {
	var domains []string
	seen := make(map[string]bool)
	rest := rule
	for {
		name, arguments, remainder, ok := nextMatcher(rest)
		if !ok {
			return domains
		}
		rest = remainder
		if name == "HostRegexp" || name == "HostSNIRegexp" {
			continue
		}
		for _, argument := range arguments {
			if argument == "*" || argument == "" || seen[argument] {
				continue
			}
			seen[argument] = true
			domains = append(domains, argument)
		}
	}
}

// nextMatcher returns the first matcher call in a rule with its string
// arguments and the text that follows it.
func nextMatcher(text string) (name string, arguments []string, rest string, ok bool) {
	best := -1
	for _, candidate := range []string{"Host", "HostSNI", "HostRegexp", "HostSNIRegexp"} {
		if index := indexMatcher(text, candidate); index >= 0 && (best < 0 || index < best) {
			best, name = index, candidate
		}
	}
	if best < 0 {
		return "", nil, "", false
	}
	open := best + len(name)
	closing := matchingParen(text, open)
	if closing < 0 {
		return "", nil, "", false
	}
	return name, stringArgs(text[open+1 : closing]), text[closing+1:], true
}

// indexMatcher returns the position of a matcher call. The name must start at
// a word boundary and be followed by an opening parenthesis.
func indexMatcher(text, name string) int {
	offset := 0
	for offset < len(text) {
		index := strings.Index(text[offset:], name)
		if index < 0 {
			return -1
		}
		index += offset
		after := index + len(name)
		if after < len(text) && text[after] == '(' && (index == 0 || !isWordByte(text[index-1])) {
			return index
		}
		offset = after
	}
	return -1
}

// matchingParen returns the index of the parenthesis that closes the call
// starting at open, skipping quoted strings.
func matchingParen(text string, open int) int {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '`':
			end := strings.IndexByte(text[i+1:], '`')
			if end < 0 {
				return -1
			}
			i += end + 1
		case '"':
			end := quotedEnd(text, i+1)
			if end < 0 {
				return -1
			}
			i = end
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// quotedEnd returns the index of the closing double quote at or after start.
func quotedEnd(text string, start int) int {
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// stringArgs returns the string arguments of one matcher call.
func stringArgs(text string) []string {
	var arguments []string
	for _, part := range splitArgs(text) {
		if value, ok := stringLiteral(strings.TrimSpace(part)); ok {
			arguments = append(arguments, value)
		}
	}
	return arguments
}

// splitArgs splits on commas that are outside quoted strings.
func splitArgs(text string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '`':
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				i += end + 1
			}
		case '"':
			if end := quotedEnd(text, i+1); end >= 0 {
				i = end
			}
		case ',':
			parts = append(parts, text[start:i])
			start = i + 1
		}
	}
	return append(parts, text[start:])
}

// stringLiteral returns the value of a backquoted or double-quoted string.
func stringLiteral(text string) (string, bool) {
	if len(text) < 2 {
		return "", false
	}
	switch {
	case text[0] == '`' && text[len(text)-1] == '`':
		return text[1 : len(text)-1], true
	case text[0] == '"' && text[len(text)-1] == '"':
		value, err := strconv.Unquote(text)
		if err != nil {
			return "", false
		}
		return value, true
	}
	return "", false
}

// isWordByte reports whether a byte can be part of an identifier.
func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// defaultRouterApp returns the app that runs the container whose default
// Traefik router name is routerName, which has the form <service>-<project>.
func defaultRouterApp(containers []*Container, routerName string) (*App, bool) {
	name, provider, found := strings.Cut(routerName, "@")
	if !found || provider != "docker" {
		return nil, false
	}
	for _, container := range containers {
		service := container.Labels[labelComposeService]
		project := container.Labels[labelComposeProject]
		if service == "" || project == "" {
			continue
		}
		if service+"-"+project == name {
			return container.App, true
		}
	}
	return nil, false
}

// sortedKeys returns the keys of a string map in order.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
