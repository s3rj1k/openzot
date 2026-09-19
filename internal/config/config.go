// Package config loads zot's configuration, layering built-in defaults, an
// optional YAML file, and environment variables (defaults < file < env).
//
// zot ships no providers: every connection a run can target is declared under
// `providers:` in the config, with its own endpoint and credential.
package config

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/openzot/openzot/agent"
	"github.com/openzot/openzot/internal/catalogue"
	"github.com/openzot/openzot/tui"
)

// Config is the fully-resolved zot configuration.
type Config struct {
	Agent Agent `yaml:"agent"`
	UI    UI    `yaml:"ui"`
	// Skills supplied programmatically, on top of whatever SkillDirectories
	// yields. The engine describes them in the system prompt; a directory
	// skill with the same name wins.
	Skills []agent.SkillDefinition `yaml:"-"`
	// SkillDirectories are the context folders scanned for SKILL.md entries.
	// Not configured directly: LoadProjectContext records them, and the run
	// rescans them at every iteration - which is what lets a skill added
	// mid-run, by the operator or by the agent itself, surface on the model's
	// next turn.
	SkillDirectories []string `yaml:"-"`
	// DefaultProvider names the entry in Providers used when --provider is not
	// given. There is no built-in default: a run needs one named.
	DefaultProvider string `yaml:"default_provider"`
	// Providers are the named model-provider connections a run can target. None
	// are built in; each is declared here with a base_url and an api_key.
	Providers map[string]ProviderConfig `yaml:"providers"`
}

// ProviderConfig is a named model-provider connection zot can run against.
// Every provider authenticates with a Bearer credential.
type ProviderConfig struct {
	// Driver names the implementation this connection uses. Empty and "openai"
	// are the same thing, and the only one there is: the OpenAI-compatible
	// chat-completions API, which any endpoint may speak - it does not have to
	// be OpenAI's.
	Driver string `yaml:"driver"`
	// BaseURL is the API endpoint root. Required, and https unless loopback.
	BaseURL string `yaml:"base_url"`
	// APIKey is the provider credential. Supports "$ENV_VAR" references, so no
	// secret need be written to disk. Required unless base_url is loopback.
	APIKey string `yaml:"api_key"`
	// Models is an optional custom model list for this provider. When omitted,
	// any model name is accepted and described by the built-in catalogue. When
	// present, its keys are the selectable names and each entry may alias or
	// override that model.
	Models map[string]ModelConfig `yaml:"models"`
}

// ModelConfig is a custom model definition under a provider. Any field set here
// overrides the run's defaults when the model is selected.
type ModelConfig struct {
	// Model is the underlying model id to send. Lets a custom name alias a real
	// model; leave empty to use the selected name as-is.
	Model string `yaml:"model"`
	// MaxIterations overrides the global iteration cap for this model.
	MaxIterations int `yaml:"max_iterations"`
	// APIKey is this model's own credential, overriding the provider's. Useful
	// where one gateway fronts several providers, each wanting its own key.
	// Supports "$ENV_VAR".
	APIKey string `yaml:"api_key"`
	// Context overrides the model's total context window, in tokens. The
	// escape hatch for a serving endpoint whose real ceiling is smaller than
	// the model's card - an uncatalogued model is assumed large, so a small
	// upstream rejects the request outright, and an
	// upstream that reports overflow opaquely gives the recovery path nothing
	// to detect. Zero uses the catalogue.
	Context int `yaml:"context"`

	// Vision overrides whether the catalogue believes this model can be shown
	// images. Unset defers to the catalogue, which is why it is a pointer: "not
	// stated" and "stated as false" are different answers, and only the second
	// should be able to turn the capability off.
	//
	// The catalogue cannot know about a private deployment, a gateway that
	// strips capabilities on the way through, or a model released after this
	// binary was built. An uncatalogued model is assumed blind, so a model that
	// can in fact see needs saying so here before it is offered a tool for
	// looking.
	Vision *bool `yaml:"vision"`

	// ContentArray sends every message's content as an array of parts, for
	// endpoints whose chat template rejects the bare string.
	ContentArray bool `yaml:"content_array"`
}

// Capabilities applies this model's overrides to what the catalogue believes:
// whether it can see, and how large its window is.
//
// The resolution order is the same one Context follows: an explicit setting
// wins, otherwise the catalogue, otherwise its conservative default.
func (m ModelConfig) Capabilities(base catalogue.Model) catalogue.Model {
	if m.Vision != nil {
		base.SupportsVision = *m.Vision
	}

	if m.Context > 0 {
		base.ContextWindow = m.Context
	}

	return base
}

// ProviderDriver resolves which implementation a provider uses. Empty is the
// only driver there is, "openai".
func ProviderDriver(provider ProviderConfig) string {
	if provider.Driver == "" {
		return DriverOpenAI
	}

	return provider.Driver
}

// DriverOpenAI is the driver every provider uses: the OpenAI-compatible
// chat-completions API.
const DriverOpenAI = agent.DriverOpenAI

// ProviderCredential returns the credential configured for a provider.
func ProviderCredential(provider ProviderConfig) string {
	return provider.APIKey
}

// ProviderModels returns the model names a provider was configured with, sorted.
// Empty means no custom list: any model name is accepted.
func ProviderModels(provider ProviderConfig) []string {
	names := make([]string, 0, len(provider.Models))
	for name := range provider.Models {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// UI holds presentation options for the read-only viewer.
type UI struct {
	// Diff, when true, renders a framed, syntax-highlighted before/after diff
	// panel beneath every edit/write the agent makes.
	Diff bool `yaml:"diff"`
	// Plain forces the unstyled streaming renderer (no full-screen TUI). Without
	// a terminal Zot also streams, with styling decided separately by Color.
	Plain bool `yaml:"plain"`
	// Color controls ANSI styling for automatic non-interactive streams: auto,
	// always, or never. Plain still forces unstyled output, and Color never
	// enables terminal input.
	Color string `yaml:"color"`
	// Scrollback caps how many log lines the full-screen viewer keeps on screen.
	// Zero uses the built-in default; raise it to keep more of a long run visible
	// (at more memory). The full run is always in the session log regardless.
	Scrollback int `yaml:"scrollback"`
	// Stats selects which fields the header bar shows, and in what order (see
	// tui.KnownStats: provider, model, dir, iter, tools, edits, elapsed,
	// tokens, tps, pace, task, order). Order matters - the bar drops what does
	// not fit, so earlier fields survive a narrower terminal. Empty
	// uses the default set.
	Stats []string `yaml:"stats"`
}

// Agent holds the knobs that shape an autonomous run.
type Agent struct {
	// Model is the model name driving the agent.
	Model string `yaml:"model"`
	// MaxIterations caps how many plan/act/observe cycles the agent may run
	// before it is forced to stop.
	MaxIterations int `yaml:"max_iterations"`
	// MaxSettles bounds how many times the agent is nudged to record an outcome
	// (call success or failure) before the run is surfaced as unsettled. This
	// is "how hard we push the model to finish properly". Zero uses the built-in
	// default.
	MaxSettles int `yaml:"max_settles"`
	// MaxCalls caps the total number of tool calls across a run, independently of
	// iterations (one iteration can request several). Zero is unbounded - only
	// max_iterations is a finite default.
	MaxCalls int `yaml:"max_calls"`
	// MaxTime caps the wall-clock time of a run, as a duration string ("30m",
	// "2h", "90s"). Empty is unbounded.
	MaxTime string `yaml:"max_time"`
	// MaxTokens caps the output tokens of a single model response. Zero is
	// unbounded - like max_calls and max_time, zot sends no cap, so the model
	// produces its full output. A positive value caps a single response.
	MaxTokens int `yaml:"max_tokens"`
	// MaxToolOutput caps the bytes a single tool result may return before it is
	// truncated. Zero uses the built-in default. Lower it for a model served by
	// an endpoint with a small context window, where one large result can
	// overflow the whole request and be rejected wholesale.
	MaxToolOutput int `yaml:"max_tool_output"`
	// MaxContinuations caps CONSECUTIVE recovery attempts - a truncated
	// response, or a retriable provider error - with no good turn between
	// them; a turn that comes back whole resets the count. Zero uses the
	// built-in default.
	MaxContinuations int `yaml:"max_continuations"`
	// MaxRecoveries caps recovery attempts across a whole run, however they are
	// spaced - where max_continuations catches a provider refusing right now,
	// this catches one that answers just often enough to keep resetting it.
	// Zero uses the built-in default.
	MaxRecoveries int `yaml:"max_recoveries"`
	// MaxCycles is how many times the loop nudges the model out of a detected
	// repetition before giving up. Zero uses the built-in default. A safety
	// guard - the default encodes a real failure, so raise it with care.
	MaxCycles int `yaml:"max_cycles"`
	// MaxEmpties caps consecutive empty turns before the run bails. Zero uses the
	// built-in default.
	MaxEmpties int `yaml:"max_empties"`
	// LimitCheckpoints are the percentages of a bounded limit (iterations, calls,
	// time) at which the model is told it is approaching that limit, so it can
	// pace itself. Unset uses the built-in default (50, 80, 90); an explicit
	// empty list turns the notices off.
	LimitCheckpoints []int `yaml:"limit_checkpoints"`
	// Instructions optionally overrides the built-in system prompt. Leave
	// empty to use zot.DefaultInstructions. An override replaces the prompt
	// but not zot's non-interactive contract, which is re-attached to whatever
	// a run resolves to: the run has no input channel to opt back into.
	Instructions string `yaml:"instructions"`
}

// MaxDuration parses Agent.MaxTime into a duration. An empty value is zero
// (unbounded); a malformed value is an error so a typo in the config is caught
// at load rather than silently ignored.
func (a Agent) MaxDuration() (time.Duration, error) {
	value := strings.TrimSpace(a.MaxTime)
	if value == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (use forms like \"30m\", \"2h\", \"90s\")", a.MaxTime)
	}

	if d < 0 {
		return 0, fmt.Errorf("%q is negative", a.MaxTime)
	}

	return d, nil
}

// Defaults returns the built-in configuration used when nothing else is set.
//
// There is deliberately no default provider or model. Both name something the
// operator runs against, and a pair that cannot actually talk to each other
// fails as a provider error rather than a configuration one, which is much
// harder to read. Validate says what is missing instead.
func Defaults() Config {
	return Config{
		Agent: Agent{
			MaxIterations: 1_000_000,
		},
		UI: UI{Color: "auto"},
	}
}

// Load resolves the configuration: defaults, then the YAML file (if present),
// then environment overrides. A missing file at the default path is fine -
// env vars alone can configure zot; a bad explicit --config file is an error.
func Load(path string) (Config, error) {
	cfg := Defaults()

	explicit := path != ""
	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case os.IsNotExist(err) && !explicit:
		// No default config file: rely on defaults + env.
	default:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}

	resolveProviders(&cfg)

	return cfg, nil
}

// resolveProviders resolves every credential - the provider-level key and each
// model's own key - from its "$ENV" reference, when it is one.
//
// The credential is only ever what the config says. There is no fallback to a
// conventional environment variable: a key is scoped to the host it was issued
// for, and guessing which one belongs to a URL somebody typed is how a
// credential ends up in someone else's logs.
func resolveProviders(cfg *Config) {
	for name, p := range cfg.Providers {
		// Every spelling is resolved: a `$VAR` reference left unexpanded would
		// send the literal string "$MY_KEY" to the provider and come back as a
		// 401 that reads like a bad key.
		p.APIKey = resolveSecret(p.APIKey)

		for mName, mc := range p.Models {
			if mc.APIKey != "" {
				mc.APIKey = resolveSecret(mc.APIKey)
				p.Models[mName] = mc
			}
		}

		cfg.Providers[name] = p
	}
}

// resolveSecret expands a "$ENV_VAR" / "${ENV_VAR}" reference; a literal value
// is returned unchanged.
//
// An unset variable resolves to empty rather than to its own name, so a missing
// credential is reported as a missing credential instead of being sent to the
// provider as the literal text "$MY_KEY".
func resolveSecret(v string) string {
	v = strings.TrimSpace(v)

	if v == "" {
		return ""
	}

	if strings.HasPrefix(v, "$") {
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(v, "$"), "{"), "}")
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(name)))
	}
	return v
}

// ScrubProviderSecrets removes every resolved provider credential, both
// provider-level and per-model, from the process environment. Config retains
// the resolved values used by the SDK client, while shell commands launched by
// the agent no longer inherit those credentials.
func ScrubProviderSecrets(cfg Config) {
	secrets := map[string]bool{}
	add := func(v string) {
		if v != "" {
			secrets[v] = true
		}
	}
	for _, provider := range cfg.Providers {
		add(provider.APIKey)
		for _, mc := range provider.Models {
			add(mc.APIKey)
		}
	}
	if len(secrets) == 0 {
		return
	}

	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && secrets[value] {
			_ = os.Unsetenv(name)
		}
	}
}

// Validate checks the fully-merged configuration.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Agent.Model) == "" {
		return fmt.Errorf("agent.model must be set (in the config, or with --model): zot has no default model")
	}
	if c.Agent.MaxIterations <= 0 {
		return fmt.Errorf("agent.max_iterations must be a positive number")
	}
	if _, err := c.Agent.MaxDuration(); err != nil {
		return fmt.Errorf("agent.max_time: %w", err)
	}
	for _, p := range c.Agent.LimitCheckpoints {
		if p < 1 || p > 99 {
			return fmt.Errorf("agent.limit_checkpoints: %d is out of range (each must be 1-99)", p)
		}
	}
	if c.UI.Scrollback < 0 {
		return fmt.Errorf("ui.scrollback must not be negative")
	}
	switch strings.ToLower(strings.TrimSpace(c.UI.Color)) {
	case "", "auto", "always", "never":
	default:
		return fmt.Errorf("ui.color: %q is not valid (use auto, always, or never)", c.UI.Color)
	}
	for _, s := range c.UI.Stats {
		if !tui.IsKnownStat(s) {
			return fmt.Errorf("ui.stats: %q is not a known field (valid: %s)",
				s, strings.Join(tui.KnownStats, ", "))
		}
	}
	if strings.TrimSpace(c.DefaultProvider) == "" {
		return fmt.Errorf(
			"no provider selected: declare one under providers: in the config and name it with default_provider (or --provider) - zot has no built-in providers")
	}
	if _, ok := c.Providers[c.DefaultProvider]; !ok {
		return fmt.Errorf("provider %q is not configured (declare it under providers: with a base_url and api_key)", c.DefaultProvider)
	}
	if provider := c.Providers[c.DefaultProvider]; len(provider.Models) > 0 {
		if _, ok := provider.Models[c.Agent.Model]; !ok {
			return fmt.Errorf("model %q is not configured for provider %q (available: %s)",
				c.Agent.Model, c.DefaultProvider, strings.Join(ProviderModels(provider), ", "))
		}
	}
	for name, provider := range c.Providers {
		if driver := ProviderDriver(provider); driver != DriverOpenAI {
			return fmt.Errorf("providers.%s: driver %q is not known (the only driver is %q)",
				name, driver, DriverOpenAI)
		}

		// there is no built-in endpoint to fall back on, and finding out
		// mid-run that there is nowhere to send the request is worse than at
		// load
		if provider.BaseURL == "" {
			return fmt.Errorf("providers.%s: base_url is not set", name)
		}
	}
	return nil
}
