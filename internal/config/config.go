// Package config loads agent's configuration, layering built-in defaults under a YAML file. Agent ships no provider.
// The one connection every run uses is declared under `provider:` with its endpoint, credential and models, and
// agent.model picks which of them runs.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

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

// UI holds presentation options for the read-only viewer.
type UI struct {
	// How many log lines the viewer keeps on screen. Zero uses the built-in default. Raising it keeps
	// more of a long run visible at more memory. The session log always has the full run.
	Scrollback int `yaml:"scrollback"`
}

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
