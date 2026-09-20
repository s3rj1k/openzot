package provider

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ClientConfig identifies which endpoint to call and with what credential.
type ClientConfig struct {
	// Provider is the operator's name for this connection. Informational. It
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

	/*
		ReasoningEffort is sent as reasoning_effort when set, as the operator
		wrote it apart from case and spacing. The endpoint rejects what it does not
		know. Empty sends nothing and leaves the model's default.
	*/
	ReasoningEffort string

	/*
		ExtraBody is merged into every request body as it is, for whatever a server
		takes that has no field of its own here - a chat-template switch that turns
		thinking off, say.
	*/
	ExtraBody map[string]any
}

// ErrMissingCredential is returned when a provider that needs a key has none.
var ErrMissingCredential = errors.New("provider: no API key configured")

// isLoopbackHost reports whether a hostname is local, which is the one case
// where a plaintext endpoint is reasonable.
//
// It takes a hostname rather than a host. Url.URL.Hostname() is what strips both
// the port and the brackets an IPv6 address is written in. Splitting the raw host
// on its last colon cannot do that - "[::1]" has no port and every colon in it
// belongs to the address - and got the bracketed form wrong in both directions.
func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}

	// an address rather than a name. 127.0.0.0/8 and the IPv6 loopback address are all local
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback()
	}

	return false
}

// Resolve validates the configuration and fills in its defaults.
//
// The endpoint is always the operator's, so the credential is too. A key is
// scoped to the host it was issued for, and there is no ambient one to fall
// back on. Only a loopback endpoint may go without.
func (c ClientConfig) Resolve() (ClientConfig, error) {
	resolved := c

	resolved.Provider = strings.TrimSpace(c.Provider)

	if resolved.Model == "" {
		return ClientConfig{}, errors.New("provider: no model specified")
	}

	resolved.ReasoningEffort = strings.ToLower(strings.TrimSpace(c.ReasoningEffort))

	if resolved.BaseURL == "" {
		return ClientConfig{}, errors.New("provider: no base URL configured (set base_url on the provider)")
	}

	parsed, err := url.Parse(resolved.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ClientConfig{}, fmt.Errorf("provider: invalid base URL %q", resolved.BaseURL)
	}

	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return ClientConfig{}, fmt.Errorf("provider: base URL must use https (got %q)", resolved.BaseURL)
	}

	resolved.BaseURL = strings.TrimRight(resolved.BaseURL, "/")

	if resolved.APIKey == "" && !isLoopbackHost(parsed.Hostname()) {
		name := resolved.Provider
		if name == "" {
			name = "the provider"
		}

		return ClientConfig{}, fmt.Errorf(
			"%w: %s at %s needs a key of its own - a credential is scoped to the host it was issued for",
			ErrMissingCredential, name, resolved.BaseURL)
	}

	return resolved, nil
}
