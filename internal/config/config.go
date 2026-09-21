// Package config loads agent's configuration, layering built-in defaults under a YAML file. Agent ships no provider.
// The one connection every run uses is declared under `provider:` with its endpoint, credential and models, and
// agent.model picks which of them runs.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully-resolved agent configuration.
type Config struct {
	Agent Agent `yaml:"agent"`
	UI    UI    `yaml:"ui"`
	// The system prompt every run starts from, a Go text/template that reads the order and the run. Required,
	// since agent carries no prompt of its own. The starter config holds the default.
	Prompt string `yaml:"prompt"`
	// The folder of skills - subdirectories each holding a SKILL.md - loaded at startup and offered
	// through the skills tool. "~/" is home, a relative path is taken against --dir, empty means none.
	SkillsDir string `yaml:"skills_dir"`
	// The one model-provider connection every run uses. None is built in. It holds a base_url, an
	// api_key and the models it serves, and agent.model picks which of them runs.
	Provider ProviderConfig `yaml:"provider"`
}

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

// UI holds presentation options for the read-only viewer.
type UI struct {
	// How many log lines the viewer keeps on screen. Zero uses the built-in default. Raising it keeps
	// more of a long run visible at more memory. The session log always has the full run.
	Scrollback int `yaml:"scrollback"`
}

// Agent holds the knobs that shape an autonomous run.
type Agent struct {
	// Model is the model name driving the agent.
	Model string `yaml:"model"`
	// MaxIterations caps how many plan/act/observe cycles the agent may run
	// before it is forced to stop.
	MaxIterations int `yaml:"max_iterations"`
	// How many times the agent is nudged to record an outcome (success or failure) before the run is
	// reported unsettled. Zero uses the built-in default.
	MaxSettles int `yaml:"max_settles"`
	// Caps total tool calls across a run, independent of iterations (one can request several).
	// Zero is unbounded, and max_iterations is the only finite default.
	MaxCalls int `yaml:"max_calls"`
	// MaxTime caps the wall-clock time of a run, as a duration string ("30m",
	// "2h", "90s"). Empty is unbounded.
	MaxTime string `yaml:"max_time"`
	// Caps the output tokens of one model response. Zero sends no cap, like max_calls and max_time,
	// so the model produces its full output.
	MaxTokens int `yaml:"max_tokens"`
	// Caps one tool result at this share of the context window, in percent, before it is truncated.
	// Zero uses the default (25). A share, not a size, so a small-window model gets a tighter bound.
	MaxToolOutputPercent int `yaml:"max_tool_output_percent"`
	// Caps CONSECUTIVE recovery attempts (truncated response, retriable provider error) with no
	// good turn between them. A whole turn resets the count. Zero uses the built-in default.
	MaxContinuations int `yaml:"max_continuations"`
	// Caps recovery attempts across a whole run, however spaced. max_continuations catches a provider
	// failing now, this catches one that answers just often enough to keep resetting it. Zero uses the default.
	MaxRecoveries int `yaml:"max_recoveries"`
	// How many times the loop nudges the model out of a detected repetition before giving up. Zero
	// uses the built-in default. The default encodes a real failure, so raise it with care.
	MaxCycles int `yaml:"max_cycles"`
	// MaxEmpties caps consecutive empty turns before the run bails. Zero uses the
	// built-in default.
	MaxEmpties int `yaml:"max_empties"`
	// Percent of the context window where the oldest message starts to be forgotten on every request.
	// Zero uses the built-in default (50).
	ContextSoft int `yaml:"context_soft"`
	// Percent of the window a request may never reach. Past it, as many of the oldest messages are
	// forgotten as it takes. Must be above context_soft. Zero uses the built-in default (90).
	ContextHard int `yaml:"context_hard"`
	// Iterations between reminders that the plan tool exists. Zero uses the built-in default (5).
	// A negative value turns the reminders off.
	PlanNudgeEvery int `yaml:"plan_nudge_every"`
	// How few turns may be left in the window after forgetting before the plan is posted again.
	// Zero uses the built-in default (5).
	PlanMinTurns int `yaml:"plan_min_turns"`
}

// MaxDuration parses Agent.MaxTime into a duration. An empty value is zero
// (unbounded). A malformed value is an error so a typo in the config is caught
// at load rather than silently ignored.
func (a *Agent) MaxDuration() (time.Duration, error) {
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

// The defaults of the two context thresholds live here because the rule between them is the config's. The
// engine has its own fallbacks for callers building options by hand, and a test holds the two to the same numbers.
const (
	defaultContextSoft = 50
	defaultContextHard = 90
)

// Defaults returns the built-in configuration used when nothing else is set. There is no default provider or
// model, since a pair that cannot talk to each other fails as a hard-to-read provider error. Validate says what is missing.
func Defaults() Config {
	return Config{
		Agent: Agent{
			MaxIterations: 1_000_000,
			ContextSoft:   defaultContextSoft,
			ContextHard:   defaultContextHard,
		},
	}
}

// ReasoningEfforts are the values reasoning_effort may take. The ones fantasy
// knows how to send, in increasing order of effort.
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// validateContext holds the thresholds to 1 <= soft < hard <= 99, percent of the
// window. Zero is the default.
func (a *Agent) validateContext() error {
	soft, hard := a.ContextSoft, a.ContextHard

	if soft == 0 {
		soft = defaultContextSoft
	}

	if hard == 0 {
		hard = defaultContextHard
	}

	if soft < 1 || hard > 99 || soft >= hard {
		return fmt.Errorf(
			"agent.context_soft/context_hard: soft=%d hard=%d: want 1 <= soft < hard <= 99 (percent of the window)", soft, hard)
	}

	return nil
}

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

// Load resolves the configuration. Defaults, then the YAML file (if present).
// A missing file at the default path is fine. A bad explicit --config file is
// an error.
func Load(path string) (Config, error) {
	cfg := Defaults()

	explicit := path != ""
	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the config path is the one the operator named
	switch {
	case err == nil:
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)

		if err := decoder.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case os.IsNotExist(err) && !explicit:
		// No default config file. Rely on defaults + env.
	default:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	ResolveProvider(&cfg)

	return cfg, nil
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

// Validate checks the fully-merged configuration.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Agent.Model) == "" {
		return errors.New("agent.model must be set in the config: agent has no default model")
	}

	if strings.TrimSpace(c.Prompt) == "" {
		return errors.New("prompt must be set in the config: agent has no built-in system prompt (`agent config` seeds one)")
	}

	if c.Agent.MaxIterations <= 0 {
		return errors.New("agent.max_iterations must be a positive number")
	}

	if _, err := c.Agent.MaxDuration(); err != nil {
		return fmt.Errorf("agent.max_time: %w", err)
	}

	if err := c.Agent.validateContext(); err != nil {
		return err
	}

	if c.Agent.MaxToolOutputPercent < 0 || c.Agent.MaxToolOutputPercent > 100 {
		return fmt.Errorf("agent.max_tool_output_percent: %d is out of range (1-100, or 0 for the default)", c.Agent.MaxToolOutputPercent)
	}

	if c.Agent.PlanMinTurns < 0 {
		return errors.New("agent.plan_min_turns must not be negative")
	}

	if c.UI.Scrollback < 0 {
		return errors.New("ui.scrollback must not be negative")
	}

	p := c.Provider

	// there is no built-in endpoint to fall back on, and finding out mid-run that
	// there is nowhere to send the request is worse than at load
	if p.BaseURL == "" {
		return errors.New("no provider: declare one under provider: in the config, with a base_url, an api_key and its models - agent has no built-in provider")
	}

	if len(p.Models) == 0 {
		return fmt.Errorf(
			"provider.models is empty: list %q under provider.models, with its context window", c.Agent.Model)
	}

	if _, ok := p.Models[c.Agent.Model]; !ok {
		return fmt.Errorf("agent.model %q is not under provider.models (available: %s)",
			c.Agent.Model, strings.Join(p.ModelNames(), ", "))
	}

	// Every model states its own window, in sorted order so the first error is
	// the same one every time.
	for _, model := range p.ModelNames() {
		if p.Models[model].Context <= 0 {
			return fmt.Errorf(
				"provider.models.%s: context is required - set the model's context window, in tokens", model)
		}

		if effort := strings.ToLower(strings.TrimSpace(p.Models[model].ReasoningEffort)); !slices.Contains(ReasoningEfforts, effort) && effort != "" {
			return fmt.Errorf("provider.models.%s: reasoning_effort %q is not known (use %s)",
				model, p.Models[model].ReasoningEffort, strings.Join(ReasoningEfforts, ", "))
		}
	}

	return nil
}
