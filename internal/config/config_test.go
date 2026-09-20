package config

import (
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// validConfig returns a minimal config that passes Validate, optionally tweaked.
func validConfig(tweak func(*Config)) Config {
	c := Config{
		Agent: Agent{Model: "m", MaxIterations: 1},
		Provider: ProviderConfig{
			BaseURL: "https://gw.example.com/v1", APIKey: "x",
			Models: map[string]ModelConfig{"m": {Context: 100_000}},
		},
	}
	if tweak != nil {
		tweak(&c)
	}

	return c
}

// There is no default provider or model: both name something the operator runs
// against, so the defaults carry neither and Validate says what is missing.
func TestDefaultsCarryNoProviderOrModel(t *testing.T) {
	c := Defaults()
	if c.Agent.Model != "" {
		t.Errorf("default model = %q, want none", c.Agent.Model)
	}

	if c.Provider.BaseURL != "" || len(c.Provider.Models) != 0 {
		t.Errorf("default provider = %+v, want none", c.Provider)
	}

	if c.Agent.MaxIterations <= 0 {
		t.Error("expected a positive default max_iterations")
	}
}

// Nothing is seeded, and no conventional credential variable is read: a config
// that declares no provider has none, whatever the environment holds.
func TestLoadSeedsNoProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZAI_API_KEY", "sk-zai")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Provider.BaseURL != "" || cfg.Provider.APIKey != "" {
		t.Errorf("provider = %+v, want none", cfg.Provider)
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("a config with no provider or model must not validate")
	}
}

// A provider that names no endpoint gets none, and no ambient key: nothing is
// filled in on its behalf.
func TestAnEmptyProviderGetsNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-openai")

	path := writeConfig(t, `
agent:
  model: gpt-5.4
provider: {}
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if provider := cfg.Provider; provider.BaseURL != "" || provider.APIKey != "" {
		t.Errorf("provider = %+v, want no endpoint and no key filled in", provider)
	}

	if err := cfg.Validate(); err == nil {
		t.Error("a provider with no base_url must not validate")
	}
}

// Removed configuration vocabulary must fail loudly rather than being ignored
// and silently sending a run to the default provider.
func TestLoadRejectsRemovedConfigKeys(t *testing.T) {
	tests := map[string]string{
		"provider collection": `
default_backend: openai
backends:
  openai:
    api_key: sk-test
	`,
		"several named providers": `
default_provider: corporate
providers:
  corporate:
    base_url: https://gw.example.com/v1
	`,
		"agent instructions (the prompt lives in the order now)": `
agent:
  instructions: be brief
	`,
		"attribution": `
attribution:
  name: acme-bot
	`,
		"per-model tools override": `
provider:
  base_url: https://gw.example.com/v1
  models:
    fast:
      tools: false
	`,
		"per-model reasoning override": `
provider:
  base_url: https://gw.example.com/v1
  models:
    fast:
      reasoning: true
	`,
		"per-model driver": `
provider:
  base_url: https://gw.example.com/v1
  models:
    fast:
      driver: openai
	`,
		"per-model credential (the key belongs to the provider)": `
provider:
  base_url: https://gw.example.com/v1
  models:
    fast:
      api_key: sk-other
	`,
		"provider driver": `
provider:
  driver: openai
  base_url: https://gw.example.com/v1
	`,
		"provider headers": `
provider:
  headers:
    X-Team: core
  base_url: https://gw.example.com/v1
	`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Error("removed provider configuration keys must be rejected")
			}
		})
	}
}

func TestLoadExplicitMissingIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("expected an error for a missing explicit --config file")
	}
}

// A credential in the file may name an env var with $VAR, so no secret has to be
// written to disk.
func TestSecretEnvReference(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MY_PROVIDER_KEY", "sk-from-env")
	path := writeConfig(t, `
provider:
  api_key: '$MY_PROVIDER_KEY'
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Provider.APIKey; got != "sk-from-env" {
		t.Errorf("resolved secret = %q, want sk-from-env", got)
	}
}

// A braced reference resolves the same as a bare one, through a whole load.
func TestAuthorizationEnvReference(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GATEWAY_DEFAULT_KEY", "sk-gateway-default")
	path := writeConfig(t, `
provider:
  api_key: '${GATEWAY_DEFAULT_KEY}'
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Provider.APIKey; got != "sk-gateway-default" {
		t.Errorf("provider key = %q, want sk-gateway-default", got)
	}
}

func TestValidate(t *testing.T) {
	if err := validConfig(nil).Validate(); err != nil {
		t.Errorf("unexpected error for a valid config: %v", err)
	}

	if err := validConfig(func(c *Config) { c.Agent.Model = "" }).Validate(); err == nil {
		t.Error("expected an error for an empty model")
	}

	if err := validConfig(func(c *Config) { c.Agent.MaxIterations = 0 }).Validate(); err == nil {
		t.Error("expected an error for non-positive max_iterations")
	}

	if err := validConfig(func(c *Config) { c.Provider = ProviderConfig{} }).Validate(); err == nil {
		t.Error("expected an error when no provider is declared")
	}

	if err := validConfig(func(c *Config) { c.Provider.BaseURL = "" }).Validate(); err == nil {
		t.Error("expected an error for a provider with no base_url")
	}

	if err := validConfig(func(c *Config) {
		c.Provider = ProviderConfig{BaseURL: "https://gw.example.com/v1", Models: map[string]ModelConfig{"allowed": {Model: "gpt-5.4", Context: 100_000}}}
	}).Validate(); err == nil {
		t.Error("expected the model list to reject an unlisted model")
	}

	if err := validConfig(func(c *Config) {
		c.Agent.Model = "allowed"
		c.Provider = ProviderConfig{BaseURL: "https://gw.example.com/v1", Models: map[string]ModelConfig{"allowed": {Model: "gpt-5.4", Context: 100_000}}}
	}).Validate(); err != nil {
		t.Errorf("a declared model was rejected: %v", err)
	}
}

// Several models on the one provider are the point: agent.model picks which runs,
// and naming one the provider does not list says what is available.
func TestAgentModelSelectsAmongTheProvidersModels(t *testing.T) {
	models := map[string]ModelConfig{
		"fast":  {Context: 32_000},
		"smart": {Context: 200_000},
	}

	for _, name := range []string{"fast", "smart"} {
		err := validConfig(func(c *Config) {
			c.Agent.Model = name
			c.Provider.Models = models
		}).Validate()
		if err != nil {
			t.Errorf("model %q is listed and was refused: %v", name, err)
		}
	}

	err := validConfig(func(c *Config) {
		c.Agent.Model = "huge"
		c.Provider.Models = models
	}).Validate()
	if err == nil {
		t.Fatal("a model the provider does not list was accepted")
	}

	for _, want := range []string{`"huge"`, "provider.models", "fast, smart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// The context window is the only source of what a model can take: there is no
// table to fall back on, so a model without one cannot run, and the error says
// which model and what to set.
func TestValidateRequiresEveryModelToStateItsContextWindow(t *testing.T) {
	for _, window := range []int{0, -1} {
		err := validConfig(func(c *Config) {
			c.Provider.Models = map[string]ModelConfig{"m": {Context: window}}
		}).Validate()
		if err == nil {
			t.Fatalf("a context of %d was accepted", window)
		}

		for _, want := range []string{"provider.models.m", "context is required"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err, want)
			}
		}
	}

	// not only the selected model: a listed model with no window is a mistake
	// whether or not this run uses it
	err := validConfig(func(c *Config) {
		c.Provider.Models = map[string]ModelConfig{"m": {Context: 100_000}, "spare": {Model: "gpt-5.4"}}
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "provider.models.spare") {
		t.Errorf("an unused model with no context should still be refused, got %v", err)
	}
}

// With no model list there is nowhere to state a window, so the model cannot run
// at all. Silently accepting any model name is what a built-in table allowed.
func TestValidateRefusesAProviderThatDeclaresNoModels(t *testing.T) {
	err := validConfig(func(c *Config) {
		c.Provider = ProviderConfig{BaseURL: "https://gw.example.com/v1", APIKey: "x"}
	}).Validate()
	if err == nil {
		t.Fatal("a provider with no models was accepted")
	}

	for _, want := range []string{"models is empty", "context window"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestModelNamesAreSorted(t *testing.T) {
	custom := ProviderConfig{Models: map[string]ModelConfig{
		"small": {Model: "gpt-5.4-mini"},
		"large": {Model: "gpt-5.4"},
	}}
	if got, want := custom.ModelNames(), []string{"large", "small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("model names = %v, want %v", got, want)
	}

	if got := (ProviderConfig{}).ModelNames(); len(got) != 0 {
		t.Fatalf("no models should mean no names, got %v", got)
	}
}

// The viewer and the log call the provider by the host of its base_url.
func TestTheProviderIsLabelledByItsHost(t *testing.T) {
	for base, want := range map[string]string{
		"https://gateway.example.com/v1": "gateway.example.com",
		"http://127.0.0.1:8080/v1":       "127.0.0.1:8080",
		"not a url":                      "not a url",
		"":                               "",
	} {
		if got := (ProviderConfig{BaseURL: base}).Label(); got != want {
			t.Errorf("label of %q = %q, want %q", base, got, want)
		}
	}
}

// Scrubbing removes the resolved credential from the environment, while leaving
// unrelated variables intact.
func TestScrubProviderSecrets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZAI_API_KEY", "sk-zai")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZOT_TEST_UNRELATED", "keep-me")
	path := writeConfig(t, `
provider:
  api_key: $ZAI_API_KEY
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ScrubProviderSecrets(cfg)

	if _, ok := os.LookupEnv("ZAI_API_KEY"); ok {
		t.Error("ZAI_API_KEY should be removed after scrub")
	}

	if got := os.Getenv("OPENAI_API_KEY"); got != "sk-openai" {
		t.Errorf("a variable the config never named was touched: OPENAI_API_KEY = %q", got)
	}

	if got := os.Getenv("ZOT_TEST_UNRELATED"); got != "keep-me" {
		t.Errorf("ZOT_TEST_UNRELATED = %q, want keep-me", got)
	}
}

// The config path resolves through an explicit override, then XDG, then the
// home fallback - so a container that sets neither still lands somewhere real.
func TestConfigPathResolution(t *testing.T) {
	t.Run("an explicit ZOT_CONFIG wins", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "/custom/zot.yaml")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")

		if got := DefaultConfigPath(); got != "/custom/zot.yaml" {
			t.Errorf("DefaultConfigPath = %q", got)
		}
	})

	t.Run("then XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")

		if got := DefaultConfigPath(); got != "/xdg/zot/config.yaml" {
			t.Errorf("DefaultConfigPath = %q", got)
		}
	})

	t.Run("then the home directory", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/someone")

		if got := DefaultConfigPath(); got != "/home/someone/.config/zot/config.yaml" {
			t.Errorf("DefaultConfigPath = %q", got)
		}
	})

	t.Run("whitespace counts as unset", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "   ")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")

		if got := DefaultConfigPath(); got != "/xdg/zot/config.yaml" {
			t.Errorf("DefaultConfigPath = %q, want the blank override ignored", got)
		}
	})
}

// A container with no HOME still needs somewhere to look, or the binary cannot
// start at all.
func TestHomeDirHasAFallback(t *testing.T) {
	t.Setenv("HOME", "")

	if got := homeDir(); got == "" {
		t.Error("homeDir must always return something")
	}

	t.Setenv("HOME", "/home/real")

	if got := homeDir(); got != "/home/real" {
		t.Errorf("homeDir = %q", got)
	}
}

// ConfigDir is where a global AGENTS.md and skills live, so it has to track
// whichever config path is in play.
func TestConfigDir(t *testing.T) {
	if got := ConfigDir("/somewhere/zot.yaml"); got != "/somewhere" {
		t.Errorf("ConfigDir = %q", got)
	}

	t.Setenv("ZOT_CONFIG", "/fallback/zot.yaml")

	if got := ConfigDir("   "); got != "/fallback" {
		t.Errorf("ConfigDir with a blank path = %q, want the default's directory", got)
	}
}

func TestValidateRejectsAnUnreachableProvider(t *testing.T) {
	cfg := Defaults()
	cfg.Agent.Model = "m"

	if err := cfg.Validate(); err == nil {
		t.Error("a provider with no base_url must be rejected")
	}

	cfg.Provider = ProviderConfig{
		BaseURL: "https://gw.example.com/v1",
		Models:  map[string]ModelConfig{"m": {Context: 100_000}},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("a provider with its own endpoint is valid: %v", err)
	}
}

// A `$VAR` reference is the documented way to keep a key out of the config
// file. It silently did not expand, which sent the literal text "$MY_KEY" to
// the provider and came back as a 401 that reads like a bad key rather than a
// config that never resolved.
func TestAnEnvReferenceIsExpanded(t *testing.T) {
	t.Setenv("ZOT_TEST_PROVIDER_KEY", "sk-resolved")

	tests := []struct {
		name     string
		provider ProviderConfig
	}{
		{name: "api_key", provider: ProviderConfig{APIKey: "$ZOT_TEST_PROVIDER_KEY"}},
		{name: "braced", provider: ProviderConfig{APIKey: "${ZOT_TEST_PROVIDER_KEY}"}},
		{name: "padded", provider: ProviderConfig{APIKey: "  $ZOT_TEST_PROVIDER_KEY  "}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Provider: test.provider}

			resolveProvider(&cfg)

			if got := cfg.Provider.APIKey; got != "sk-resolved" {
				t.Errorf("credential = %q, want the expanded value", got)
			}
		})
	}
}

// An unset variable must resolve to nothing, so the run fails with "no API key
// configured" rather than sending the literal reference to the provider.
func TestAnUnsetEnvReferenceResolvesToNothing(t *testing.T) {
	t.Setenv("ZOT_TEST_UNSET_KEY", "")

	cfg := Config{Provider: ProviderConfig{APIKey: "$ZOT_TEST_UNSET_KEY"}}

	resolveProvider(&cfg)

	if got := cfg.Provider.APIKey; got != "" {
		t.Errorf("credential = %q, want nothing", got)
	}
}

// A literal key is left exactly as written - a provider key that happens to
// contain a dollar sign is not a reference.
func TestALiteralCredentialIsUntouched(t *testing.T) {
	cfg := Config{Provider: ProviderConfig{APIKey: "sk-literal-with-$-inside"}}

	resolveProvider(&cfg)

	if got := cfg.Provider.APIKey; got != "sk-literal-with-$-inside" {
		t.Errorf("credential = %q, want it untouched", got)
	}
}

// The credential is only what the config says. A conventional variable named
// after the provider is never consulted, however well it matches.
func TestNoConventionalVariableIsRead(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	cfg := Config{}

	resolveProvider(&cfg)

	if got := cfg.Provider.APIKey; got != "" {
		t.Errorf("credential = %q, want none: nothing is read on the provider's behalf", got)
	}
}

// max_time is validated at load, so a typo is caught immediately rather than
// silently ignored into an unbounded run.
func TestMaxTimeIsValidated(t *testing.T) {
	base := func() Config {
		return Config{
			Agent: Agent{Model: "m", MaxIterations: 10},
			Provider: ProviderConfig{
				BaseURL: "https://gw.example.com/v1", APIKey: "k",
				Models: map[string]ModelConfig{"m": {Context: 100_000}},
			},
		}
	}

	good := base()

	good.Agent.MaxTime = "45m"
	if err := good.Validate(); err != nil {
		t.Errorf("a valid duration must pass: %v", err)
	}

	empty := base() // unbounded is valid
	if err := empty.Validate(); err != nil {
		t.Errorf("an empty max_time must pass (unbounded): %v", err)
	}

	bad := base()

	bad.Agent.MaxTime = "half an hour"
	if err := bad.Validate(); err == nil {
		t.Error("a malformed max_time must fail validation")
	}

	negative := base()

	negative.Agent.MaxTime = "-5m"
	if err := negative.Validate(); err == nil {
		t.Error("a negative max_time must fail validation")
	}
}

// Every budget the run honors is a config key. The two-word ones
// (max_continuations, max_recoveries) are the easy ones to misspell, and a
// misspelled key is rejected at load rather than silently ignored.
func TestEveryBudgetIsReadFromTheFile(t *testing.T) {
	path := writeConfig(t, `
agent:
  max_settles: 3
  max_calls: 4
  max_time: 45m
  max_tokens: 5
  max_continuations: 6
  max_recoveries: 9
  max_cycles: 7
  max_empties: 8
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	a := cfg.Agent

	for name, got := range map[string]int{
		"max_settles":       a.MaxSettles,
		"max_calls":         a.MaxCalls,
		"max_tokens":        a.MaxTokens,
		"max_continuations": a.MaxContinuations,
		"max_recoveries":    a.MaxRecoveries,
		"max_cycles":        a.MaxCycles,
		"max_empties":       a.MaxEmpties,
	} {
		if got == 0 {
			t.Errorf("%s was not read from the file", name)
		}
	}

	// and the string parses through to a real duration
	if d, err := a.MaxDuration(); err != nil || d.String() != "45m0s" {
		t.Errorf("MaxDuration = %v (err %v), want 45m", d, err)
	}
}

// The thresholds are validated at load, where the operator is looking, not when
// the run starts.
func TestContextThresholdsAreReadAndValidated(t *testing.T) {
	cfg, err := Load(writeConfig(t, "agent:\n  context_soft: 30\n  context_hard: 70\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.ContextSoft != 30 || cfg.Agent.ContextHard != 70 {
		t.Errorf("thresholds = %d/%d, want 30/70", cfg.Agent.ContextSoft, cfg.Agent.ContextHard)
	}

	for _, bad := range [][2]int{{95, 0}, {60, 60}, {0, 100}, {-1, 0}} {
		err := validConfig(func(c *Config) { c.Agent.ContextSoft, c.Agent.ContextHard = bad[0], bad[1] }).Validate()

		if err == nil || !strings.Contains(err.Error(), "context") {
			t.Errorf("thresholds %v: err = %v, want them refused and named", bad, err)
		}
	}
}

func TestPlanKnobsAreReadAndValidated(t *testing.T) {
	cfg, err := Load(writeConfig(t, "agent:\n  plan_nudge_every: -1\n  plan_min_turns: 8\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.PlanNudgeEvery != -1 || cfg.Agent.PlanMinTurns != 8 {
		t.Errorf("plan knobs = %d/%d, want -1/8", cfg.Agent.PlanNudgeEvery, cfg.Agent.PlanMinTurns)
	}

	if err := validConfig(func(c *Config) { c.Agent.PlanNudgeEvery = -1 }).Validate(); err != nil {
		t.Errorf("a negative plan_nudge_every turns the reminders off, got %v", err)
	}

	if err := validConfig(func(c *Config) { c.Agent.PlanMinTurns = -1 }).Validate(); err == nil {
		t.Error("a negative plan_min_turns was accepted")
	}
}

func TestToolOutputPercentIsReadAndValidated(t *testing.T) {
	cfg, err := Load(writeConfig(t, "agent:\n  max_tool_output_percent: 10\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.MaxToolOutputPercent != 10 {
		t.Errorf("max_tool_output_percent = %d, want 10", cfg.Agent.MaxToolOutputPercent)
	}

	for _, bad := range []int{-1, 101} {
		if err := validConfig(func(c *Config) { c.Agent.MaxToolOutputPercent = bad }).Validate(); err == nil {
			t.Errorf("max_tool_output_percent %d was accepted", bad)
		}
	}
}

func TestRemovedContextKnobsAreRejected(t *testing.T) {
	for _, key := range []string{
		"context_strategy: truncate",
		"context_strategy: compact",
		"compact_trigger_ratio: 0.8",
		"compact_min_tokens: 1000",
		"compact_min_messages: 10",
		"limit_checkpoints: [50, 80, 90]",
		"max_tool_output: 32000",
	} {
		t.Run(key, func(t *testing.T) {
			path := writeConfig(t, "agent:\n  "+key+"\n")

			if _, err := Load(path); err == nil {
				t.Errorf("agent.%s must be rejected", key)
			}
		})
	}
}

// skills_dir names the folder skills are loaded from; it is read as written and
// expanded by the caller, which knows the working directory.
func TestSkillsDirIsRead(t *testing.T) {
	cfg, err := Load(writeConfig(t, "skills_dir: ~/skills\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.SkillsDir != "~/skills" {
		t.Errorf("skills_dir = %q, want it as written", cfg.SkillsDir)
	}
}

// reasoning_effort and extra_body are per-model request settings: read as
// written, and an effort the provider would refuse is refused at load.
func TestAModelCarriesItsRequestSettings(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
agent:
  model: local
provider:
  base_url: http://127.0.0.1:8080/v1
  models:
    local:
      context: 8000
      reasoning_effort: Low
      extra_body:
        chat_template_kwargs:
          enable_thinking: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	model := cfg.Provider.Models["local"]

	if model.ReasoningEffort != "Low" {
		t.Errorf("reasoning_effort = %q, want it as written", model.ReasoningEffort)
	}

	kwargs, _ := model.ExtraBody["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false {
		t.Errorf("extra_body = %v, want the nested settings kept", model.ExtraBody)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("a known effort (any case) must validate: %v", err)
	}

	cfg.Provider = ProviderConfig{BaseURL: "http://127.0.0.1:8080/v1", Models: map[string]ModelConfig{
		"local": {Context: 8000, ReasoningEffort: "extreme"},
	}}

	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reasoning_effort") {
		t.Errorf("err = %v, want an unknown effort refused and named", err)
	}
}

// The viewer scrollback is a scalar UI field read from the file, and an
// out-of-range value is rejected at load.
func TestUIScrollbackIsReadAndValidated(t *testing.T) {
	path := writeConfig(t, `
ui:
  scrollback: 20000
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.UI.Scrollback != 20000 {
		t.Errorf("ui.scrollback not read: %d", cfg.UI.Scrollback)
	}

	if err := validConfig(func(c *Config) { c.UI.Scrollback = -1 }).Validate(); err == nil {
		t.Error("a negative ui.scrollback must fail validation")
	}
}

// The header is fixed, so ui.stats is gone rather than ignored: a config that
// still sets it must fail at load and name the key, not quietly show a header
// the operator did not ask for.
func TestUIStatsIsNoLongerAKey(t *testing.T) {
	path := writeConfig(t, `
ui:
  stats: [model, iter]
`)

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "stats") {
		t.Errorf("err = %v, want the removed ui.stats key refused and named", err)
	}
}

// A key written for the endpoint is exactly what it uses, and an ambient
// variable plays no part.
func TestAConfiguredKeyIsTheOneUsed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("PROXY_KEY", "sk-proxy")
	path := writeConfig(t, `
provider:
  base_url: https://proxy.example.com/v1
  api_key: $PROXY_KEY
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Provider.APIKey; got != "sk-proxy" {
		t.Errorf("key = %q, want the explicitly configured one", got)
	}
}

// The config is what everything else reads and validates itself against, so it
// must not depend on any of it: a rule written here is written once.
func TestConfigImportsNoOtherPackageOfTheModule(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(pkg.Imports, "gopkg.in/yaml.v3") {
		t.Fatalf("the check reads no imports at all: %v", pkg.Imports)
	}

	for _, path := range pkg.Imports {
		if strings.Contains(path, "openzot/openzot/") {
			t.Errorf("config imports %s", path)
		}
	}
}
