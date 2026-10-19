package extproc

import "strings"

// parseTraceparent extracts the trace and span IDs from a W3C traceparent
// header (https://www.w3.org/TR/trace-context/). It is intentionally strict:
// malformed values are ignored rather than propagated into logs.
//
// Format: version(2hex)-trace-id(32hex)-parent-id(16hex)-flags(2hex)
func parseTraceparent(value string) (traceID, spanID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", "", false
	}
	// Version 00 is fixed at four fields. Future versions may append fields,
	// but only if the version is not "ff".
	if parts[0] == "00" && len(parts) != 4 {
		return "", "", false
	}
	if strings.EqualFold(parts[0], "ff") {
		return "", "", false
	}
	if !isLowerHex(parts[0]) || !isLowerHex(parts[1]) || !isLowerHex(parts[2]) || !isLowerHex(parts[3]) {
		return "", "", false
	}
	if allZero(parts[1]) || allZero(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func allZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
