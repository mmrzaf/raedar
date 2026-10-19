package aggregate

import "strings"

// The patterns used for 404 responses and for patterns beyond the cap.
const (
	// NotFoundPattern is the pattern all 404 responses are counted under.
	NotFoundPattern = "(not found)"
	// OtherPattern is the pattern further patterns are counted under once a
	// route reaches MaxPatterns patterns in one window.
	OtherPattern = "(other)"
)

// pathDepth is the number of path segments kept before a path is truncated.
const pathDepth = 6

// truncation is appended to paths deeper than pathDepth segments.
const truncation = "…"

// NormalizePath reduces a request path to a pattern: the query string and
// fragment are removed, identifier-looking segments become :n, :uuid, :hex or
// :token, a last segment with a static file extension becomes *.<ext>, and
// paths deeper than six segments keep the first six and end in /….
func NormalizePath(path string) string {
	if index := strings.IndexAny(path, "?#"); index >= 0 {
		path = path[:index]
	}
	segments := splitPath(path)
	if len(segments) == 0 {
		return "/"
	}
	for i, segment := range segments {
		segments[i] = normalizeSegment(segment, i == len(segments)-1)
	}
	if len(segments) > pathDepth {
		segments = append(segments[:pathDepth:pathDepth], truncation)
	}
	return "/" + strings.Join(segments, "/")
}

// splitPath splits a path into its segments; the leading slash is dropped.
func splitPath(path string) []string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// normalizeSegment replaces one identifier-looking path segment. A last
// segment with a static file extension becomes *.<ext>.
func normalizeSegment(segment string, last bool) string {
	if segment == "" {
		return segment
	}
	if last {
		if dot := strings.LastIndexByte(segment, '.'); dot >= 0 && dot < len(segment)-1 {
			if pattern, ok := staticExtension(strings.ToLower(segment[dot+1:])); ok {
				return pattern
			}
		}
	}
	switch {
	case isDigits(segment):
		return ":n"
	case isUUID(segment):
		return ":uuid"
	case len(segment) >= 16 && isHex(segment):
		return ":hex"
	case len(segment) >= 24 && mixesLettersAndDigits(segment):
		return ":token"
	}
	return segment
}

// staticExtension returns the pattern for a static file extension.
func staticExtension(extension string) (string, bool) {
	switch extension {
	case "js", "mjs", "css", "map", "png", "jpg", "jpeg", "gif", "webp", "avif",
		"svg", "ico", "woff", "woff2", "ttf":
		return "*." + extension, true
	}
	return "", false
}

// isDigits reports whether a segment consists of digits only.
func isDigits(segment string) bool {
	for i := 0; i < len(segment); i++ {
		if !isDigit(segment[i]) {
			return false
		}
	}
	return true
}

// isUUID reports whether a segment is a UUID.
func isUUID(segment string) bool {
	if len(segment) != 36 {
		return false
	}
	for i := 0; i < len(segment); i++ {
		switch i {
		case 8, 13, 18, 23:
			if segment[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(segment[i]) {
				return false
			}
		}
	}
	return true
}

// isHex reports whether a segment consists of hexadecimal characters only.
func isHex(segment string) bool {
	for i := 0; i < len(segment); i++ {
		if !isHexDigit(segment[i]) {
			return false
		}
	}
	return true
}

// mixesLettersAndDigits reports whether a segment has at least one letter and
// at least one digit.
func mixesLettersAndDigits(segment string) bool {
	var letters, digits bool
	for i := 0; i < len(segment); i++ {
		switch b := segment[i]; {
		case isDigit(b):
			digits = true
		case (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z'):
			letters = true
		}
	}
	return letters && digits
}

// isDigit reports whether a byte is an ASCII digit.
func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// isHexDigit reports whether a byte is a hexadecimal digit.
func isHexDigit(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}
