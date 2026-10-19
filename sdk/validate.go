package extproc

import (
	"fmt"
	"strings"
)

// ValidateHeaderName validates an HTTP header name that a plugin may write.
//
// Pseudo headers are rejected: Envoy owns them and silently ignores attempts
// to mutate them, which would turn a plugin configuration mistake into a
// mystery in production. Names must follow the HTTP token grammar.
func ValidateHeaderName(name string) error {
	if name == "" {
		return fmt.Errorf("header name must not be empty")
	}
	if strings.HasPrefix(name, ":") {
		return fmt.Errorf("header %q is a pseudo header and cannot be mutated", name)
	}
	for i := 0; i < len(name); i++ {
		if !isTokenChar(name[i]) {
			return fmt.Errorf("header name %q contains an invalid character at byte %d", name, i)
		}
	}
	return nil
}

// ValidateHeaderValue validates an HTTP header value. CR, LF and NUL are
// rejected to keep attacker-controlled body/query values from smuggling
// header boundaries; other control characters are rejected as well (HTAB is
// allowed by HTTP field-value grammar).
func ValidateHeaderValue(value []byte) error {
	for i, b := range value {
		if b == '\t' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return fmt.Errorf("header value contains an invalid control character at byte %d", i)
		}
	}
	return nil
}

// ValidateStatus validates an HTTP status code used by an immediate response.
func ValidateStatus(status int32) error {
	if status < 100 || status > 599 {
		return fmt.Errorf("invalid HTTP status %d: must be between 100 and 599", status)
	}
	return nil
}

// ValidateMutableHeaderName validates a header name that a plugin may write or
// remove. In addition to ValidateHeaderName it rejects the headers Envoy owns:
// pseudo headers, "host" and the x-envoy-* namespace.
func ValidateMutableHeaderName(name string) error { return validateMutableHeaderName(name) }

// validateMutableHeaderName additionally rejects headers that Envoy protects:
// all pseudo headers, "host" and the x-envoy-* namespace.
func validateMutableHeaderName(name string) error {
	if err := ValidateHeaderName(name); err != nil {
		return err
	}
	lower := strings.ToLower(name)
	if lower == "host" || strings.HasPrefix(lower, "x-envoy-") {
		return fmt.Errorf("header %q is protected by Envoy and cannot be mutated", name)
	}
	return nil
}

// isTokenChar reports whether b is allowed in an HTTP token (RFC 9110).
func isTokenChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z',
		b >= 'A' && b <= 'Z',
		b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
