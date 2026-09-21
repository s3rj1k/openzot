package config

import (
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
)

// ProviderConfig is the model-provider connection agent runs against. It
// authenticates with a Bearer credential.
type ProviderConfig struct {
	// BaseURL is the API endpoint root. Required, and https unless loopback.
	BaseURL string `yaml:"base_url"`
	// APIKey is the provider credential. Supports "$ENV_VAR" references, so no
	// secret need be written to disk. Required unless base_url is loopback.
	APIKey string `yaml:"api_key"`
	// The models the provider serves. Required, since every model must state its own context window.
	// Keys are the selectable names, and an entry may alias or override the real model id.
	Models map[string]ModelConfig `yaml:"models"`
}

// ModelConfig is a model definition under the provider. Context is required. Any
// other field set here overrides the run's defaults when the model is selected.
type ModelConfig struct {
	// Model is the underlying model id to send. Lets a custom name alias a real
	// model. Leave empty to use the selected name as-is.
	Model string `yaml:"model"`
	// MaxIterations overrides the global iteration cap for this model.
	MaxIterations int `yaml:"max_iterations"`
	// The model's context window, in tokens. Required, and the only source of it. Agent keeps no table
	// of models, since the endpoint's real ceiling can be below the model's card. It sizes what is kept.
	Context int `yaml:"context"`

	// ContentArray sends every message's content as an array of parts, for
	// endpoints whose chat template rejects the bare string.
	ContentArray bool `yaml:"content_array"`

	// ReasoningEffort is sent as reasoning_effort. None, minimal, low, medium,
	// high, xhigh or max. Empty sends nothing and leaves the model's default.
	ReasoningEffort string `yaml:"reasoning_effort"`

	// ExtraBody is merged into every request body for this model, as written.
	// The escape hatch for what a server takes that has no key of its own here.
	ExtraBody map[string]any `yaml:"extra_body"`
}

// ModelNames returns the names of the models the provider serves, sorted.
func (p ProviderConfig) ModelNames() []string {
	return slices.Sorted(maps.Keys(p.Models))
}

// Label is what the viewer and the session log call the provider. The host of
// its base_url, which says where the run is talking without a second name to
// keep in step with it.
func (p ProviderConfig) Label() string {
	if u, err := url.Parse(p.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}

	return p.BaseURL
}

// ReasoningEfforts are the values reasoning_effort may take. The ones fantasy
// knows how to send, in increasing order of effort.
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// resolveSecret expands a "$ENV_VAR" or "${ENV_VAR}" reference and returns a literal unchanged. An unset variable
// resolves to empty, so a missing credential is reported as missing rather than sent as the text "$MY_KEY".
func resolveSecret(v string) string {
	v = strings.TrimSpace(v)

	if v == "" {
		return ""
	}

	if after, ok := strings.CutPrefix(v, "$"); ok {
		name := strings.TrimSuffix(strings.TrimPrefix(after, "{"), "}")
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(name)))
	}

	return v
}

// ResolveProvider resolves the credential from its "$ENV" reference, when it is one. The credential is only what the
// config says, with no conventional-variable fallback, since a key is scoped to its host. An unexpanded reference
// would send the literal "$MY_KEY" and come back as a 401 that reads like a bad key.
func ResolveProvider(cfg *Config) {
	cfg.Provider.APIKey = resolveSecret(cfg.Provider.APIKey)
}

// ScrubProviderSecrets removes the resolved provider credential from the process
// environment. Config keeps the resolved value used by the SDK client, while
// shell commands launched by the agent no longer inherit it.
func ScrubProviderSecrets(cfg *Config) {
	secret := cfg.Provider.APIKey
	if secret == "" {
		return
	}

	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value == secret {
			_ = os.Unsetenv(name)
		}
	}
}
