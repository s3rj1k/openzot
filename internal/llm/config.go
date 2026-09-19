// Package llm is zot's connection to a model. It speaks the OpenAI
// chat-completions wire format through fantasy and keeps the rules that make an
// unattended run safe: an endpoint the operator named, a credential scoped to
// it, and errors classified for the loop to retry, back off from, or give up on.
package llm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config identifies which endpoint to call and with what credential.
type Config struct {
	// Provider is the operator's name for this connection. Informational: it
	// labels errors, the session log and the viewer, and selects nothing.
	Provider string

	// Model is the provider's own model name.
	Model string

	// APIKey authenticates the request. Required unless BaseURL is loopback.
	APIKey string

	// BaseURL is the endpoint root the request is sent under. Required.
	BaseURL string

	// ContentArray sends every message's content as an array of parts, and an
	// empty one as []. Off by default, for endpoints whose template wants arrays.
	ContentArray bool
}

// ErrMissingCredential is returned when a provider that needs a key has none.
var ErrMissingCredential = errors.New("provider: no API key configured")

// Resolve validates the configuration and fills in its defaults.
//
// The endpoint is always the operator's, so the credential is too: a key is
// scoped to the host it was issued for, and there is no ambient one to fall
// back on. Only a loopback endpoint may go without.
func (c Config) Resolve() (Config, error) {
	resolved := c

	resolved.Provider = strings.TrimSpace(c.Provider)

	if resolved.Model == "" {
		return Config{}, errors.New("provider: no model specified")
	}

	if resolved.BaseURL == "" {
		return Config{}, errors.New("provider: no base URL configured (set base_url on the provider)")
	}

	parsed, err := url.Parse(resolved.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Config{}, fmt.Errorf("provider: invalid base URL %q", resolved.BaseURL)
	}

	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return Config{}, fmt.Errorf("provider: base URL must use https (got %q)", resolved.BaseURL)
	}

	resolved.BaseURL = strings.TrimRight(resolved.BaseURL, "/")

	if resolved.APIKey == "" && !isLoopbackHost(parsed.Hostname()) {
		return Config{}, fmt.Errorf(
			"%w: %s at %s needs a key of its own - a credential is scoped to the host it was issued for",
			ErrMissingCredential, firstNonEmpty(resolved.Provider, "the provider"), resolved.BaseURL)
	}

	return resolved, nil
}

// isLoopbackHost reports whether a hostname is local, which is the one case
// where a plaintext endpoint is reasonable.
//
// It takes a hostname rather than a host: url.URL.Hostname() is what strips both
// the port and the brackets an IPv6 address is written in. Splitting the raw host
// on its last colon cannot do that - "[::1]" has no port and every colon in it
// belongs to the address - and got the bracketed form wrong in both directions.
func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}

	// an address rather than a name: 127.0.0.0/8 and ::1 are all local
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback()
	}

	return false
}

// firstNonEmpty returns value when it is set, else fallback.
func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}

	return fallback
}
