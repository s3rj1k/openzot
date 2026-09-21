package config

import (
	"go/build"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// validConfig returns a minimal config that passes Validate, optionally tweaked.
func validConfig(tweak func(*Config)) *Config {
	c := &Config{
		Agent:  Agent{Model: "m", MaxIterations: 1},
		Prompt: "{{ .Objective }}",
		Provider: ProviderConfig{
			BaseURL: litHTTPSGwExampleCom, APIKey: "x",
			Models: map[string]ModelConfig{"m": {Context: 100_000}},
		},
	}
	if tweak != nil {
		tweak(c)
	}

	return c
}

// There is no default provider, model or prompt. They name something the operator runs
// against, so the defaults carry none and Validate says what is missing.
func TestDefaultsCarryNoProviderModelOrPrompt(t *testing.T) {
	c := Defaults()
	assert.Empty(t, c.Agent.Model)
	assert.Empty(t, c.Prompt)

	assert.Empty(t, c.Provider.BaseURL)
	assert.Empty(t, c.Provider.Models)

	assert.Positive(t, c.Agent.MaxIterations, "expected a positive default max_iterations")
}

// Nothing is seeded, and no conventional credential variable is read. A config
// that declares no provider has none, whatever the environment holds.
func TestLoadSeedsNoProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZAI_API_KEY", "sk-zai")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Empty(t, cfg.Provider.BaseURL)
	assert.Empty(t, cfg.Provider.APIKey)

	err = cfg.Validate()
	require.Error(t, err, "a config with no provider or model must not validate")
}

// A provider that names no endpoint gets none, and no ambient key. Nothing is
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
	require.NoError(t, err)

	provider := cfg.Provider
	assert.Empty(t, provider.BaseURL, "want no endpoint and no key filled in")
	assert.Empty(t, provider.APIKey, "want no endpoint and no key filled in")

	require.Error(t, cfg.Validate(), "a provider with no base_url must not validate")
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
			_, err := Load(writeConfig(t, body))
			require.Error(t, err, "removed provider configuration keys must be rejected")
		})
	}
}

func TestLoadExplicitMissingIsError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err, "expected an error for a missing explicit --config file")
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
	require.NoError(t, err)

	assert.Equal(t, "sk-from-env", cfg.Provider.APIKey, "want sk-from-env")
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
	require.NoError(t, err)

	assert.Equal(t, "sk-gateway-default", cfg.Provider.APIKey, "want sk-gateway-default")
}

func TestValidate(t *testing.T) {
	require.NoError(t, validConfig(nil).Validate(), "unexpected error for a valid config")

	require.Error(t, validConfig(func(c *Config) { c.Agent.Model = "" }).Validate())

	require.Error(t, validConfig(func(c *Config) { c.Agent.MaxIterations = 0 }).Validate(), "expected an error for non-positive max_iterations")

	require.Error(t, validConfig(func(c *Config) { c.Provider = ProviderConfig{} }).Validate(), "expected an error when no provider is declared")

	require.Error(t, validConfig(func(c *Config) { c.Provider.BaseURL = "" }).Validate(), "expected an error for a provider with no base_url")

	require.Error(t, validConfig(func(c *Config) {
		c.Provider = ProviderConfig{BaseURL: litHTTPSGwExampleCom, Models: map[string]ModelConfig{litAllowed: {Model: litGpt54, Context: 100_000}}}
	}).Validate(), "expected the model list to reject an unlisted model")

	require.NoError(t, validConfig(func(c *Config) {
		c.Agent.Model = litAllowed
		c.Provider = ProviderConfig{BaseURL: litHTTPSGwExampleCom, Models: map[string]ModelConfig{litAllowed: {Model: litGpt54, Context: 100_000}}}
	}).Validate(), "a declared model was rejected")
}

// Several models on the one provider are the point. Agent.model picks which runs,
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
		require.NoError(t, err, "model %q is listed and was refused", name)
	}

	err := validConfig(func(c *Config) {
		c.Agent.Model = "huge"
		c.Provider.Models = models
	}).Validate()
	require.Error(t, err, "a model the provider does not list was accepted")

	for _, want := range []string{`"huge"`, "provider.models", "fast, smart"} {
		assert.Contains(t, err.Error(), want)
	}
}

// The context window is the only source of what a model can take. There is no
// table to fall back on, so a model without one cannot run, and the error says
// which model and what to set.
func TestValidateRequiresEveryModelToStateItsContextWindow(t *testing.T) {
	for _, window := range []int{0, -1} {
		err := validConfig(func(c *Config) {
			c.Provider.Models = map[string]ModelConfig{"m": {Context: window}}
		}).Validate()
		require.Error(t, err, "a context of %d was accepted", window)

		for _, want := range []string{"provider.models.m", "context is required"} {
			assert.Contains(t, err.Error(), want)
		}
	}

	// not only the selected model. A listed model with no window is a mistake
	// whether this run uses it
	err := validConfig(func(c *Config) {
		c.Provider.Models = map[string]ModelConfig{"m": {Context: 100_000}, "spare": {Model: litGpt54}}
	}).Validate()
	require.Error(t, err, "an unused model with no context should still be refused, got %v", err)
	assert.Contains(t, err.Error(), "provider.models.spare", "an unused model with no context should still be refused, got %v", err)
}

// With no model list there is nowhere to state a window, so the model cannot run
// at all. Silently accepting any model name is what a built-in table allowed.
func TestValidateRefusesAProviderThatDeclaresNoModels(t *testing.T) {
	err := validConfig(func(c *Config) {
		c.Provider = ProviderConfig{BaseURL: litHTTPSGwExampleCom, APIKey: "x"}
	}).Validate()
	require.Error(t, err, "a provider with no models was accepted")

	for _, want := range []string{"models is empty", "context window"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestModelNamesAreSorted(t *testing.T) {
	custom := ProviderConfig{Models: map[string]ModelConfig{
		"small": {Model: "gpt-5.4-mini"},
		"large": {Model: litGpt54},
	}}
	got, want := custom.ModelNames(), []string{"large", "small"}
	require.Equal(t, want, got)

	got = (ProviderConfig{}).ModelNames()
	require.Empty(t, got, "no models should mean no names, got %v", got)
}

// The viewer and the log call the provider by the host of its base_url.
func TestTheProviderIsLabelledByItsHost(t *testing.T) {
	for base, want := range map[string]string{
		"https://gateway.example.com/v1": "gateway.example.com",
		"http://127.0.0.1:8080/v1":       "127.0.0.1:8080",
		"not a url":                      "not a url",
		"":                               "",
	} {
		assert.Equal(t, want, (ProviderConfig{BaseURL: base}).Label(), "label of %q", base)
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
	require.NoError(t, err)

	ScrubProviderSecrets(&cfg)

	_, ok := os.LookupEnv("ZAI_API_KEY")
	assert.False(t, ok, "ZAI_API_KEY should be removed after scrub")

	got := os.Getenv("OPENAI_API_KEY")
	assert.Equal(t, "sk-openai", got, "a variable the config never named was touched: OPENAI_API_KEY = %q", got)

	assert.Equal(t, "keep-me", os.Getenv("ZOT_TEST_UNRELATED"), "want keep-me")
}

// The config path resolves through an explicit override, then XDG, then the
// home fallback - so a container that sets neither still lands somewhere real.
func TestConfigPathResolution(t *testing.T) {
	t.Run("an explicit ZOT_CONFIG wins", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "/custom/zot.yaml")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")

		assert.Equal(t, "/custom/zot.yaml", DefaultConfigPath())
	})

	t.Run("then XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")

		assert.Equal(t, "/xdg/zot/config.yaml", DefaultConfigPath())
	})

	t.Run("then the home directory", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/someone")

		assert.Equal(t, "/home/someone/.config/zot/config.yaml", DefaultConfigPath())
	})

	t.Run("whitespace counts as unset", func(t *testing.T) {
		t.Setenv("ZOT_CONFIG", "   ")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")

		assert.Equal(t, "/xdg/zot/config.yaml", DefaultConfigPath(), "want the blank override ignored")
	})
}

// A container with no HOME still needs somewhere to look, or the binary cannot
// start at all.
func TestHomeDirHasAFallback(t *testing.T) {
	t.Setenv("HOME", "")

	assert.NotEmpty(t, homeDir(), "homeDir must always return something")

	t.Setenv("HOME", "/home/real")

	assert.Equal(t, "/home/real", homeDir())
}

// ConfigDir is where a global AGENTS.md and skills live, so it has to track
// whichever config path is in play.
func TestConfigDir(t *testing.T) {
	assert.Equal(t, "/somewhere", ConfigDir("/somewhere/zot.yaml"))

	t.Setenv("ZOT_CONFIG", "/fallback/zot.yaml")

	assert.Equal(t, "/fallback", ConfigDir("   "), "want the default's directory")
}

func TestValidateRejectsAnUnreachableProvider(t *testing.T) {
	cfg := Defaults()
	cfg.Agent.Model = "m"
	cfg.Prompt = "x"

	require.Error(t, cfg.Validate(), "a provider with no base_url must be rejected")

	cfg.Provider = ProviderConfig{
		BaseURL: litHTTPSGwExampleCom,
		Models:  map[string]ModelConfig{"m": {Context: 100_000}},
	}

	require.NoError(t, cfg.Validate(), "a provider with its own endpoint is valid")
}

// A `$VAR` reference is the documented way to keep a key out of the config file. It once did not expand, sending
// the literal text "$MY_KEY" to the provider and a 401 that reads like a bad key rather than a config that never resolved.
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

			assert.Equal(t, "sk-resolved", cfg.Provider.APIKey, "want the expanded value")
		})
	}
}

// An unset variable must resolve to nothing, so the run fails with "no API key
// configured" rather than sending the literal reference to the provider.
func TestAnUnsetEnvReferenceResolvesToNothing(t *testing.T) {
	t.Setenv("ZOT_TEST_UNSET_KEY", "")

	cfg := Config{Provider: ProviderConfig{APIKey: "$ZOT_TEST_UNSET_KEY"}}

	resolveProvider(&cfg)

	assert.Empty(t, cfg.Provider.APIKey)
}

// A literal key is left exactly as written - a provider key that happens to
// contain a dollar sign is not a reference.
func TestALiteralCredentialIsUntouched(t *testing.T) {
	cfg := Config{Provider: ProviderConfig{APIKey: "sk-literal-with-$-inside"}}

	resolveProvider(&cfg)

	assert.Equal(t, "sk-literal-with-$-inside", cfg.Provider.APIKey)
}

// The credential is only what the config says. A conventional variable named
// after the provider is never consulted, however well it matches.
func TestNoConventionalVariableIsRead(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	cfg := Config{}

	resolveProvider(&cfg)

	assert.Empty(t, cfg.Provider.APIKey, "want none: nothing is read on the provider's behalf")
}

// max_time is validated at load, so a typo is caught immediately rather than
// silently ignored into an unbounded run.
func TestMaxTimeIsValidated(t *testing.T) {
	base := func() Config {
		return Config{
			Agent:  Agent{Model: "m", MaxIterations: 10},
			Prompt: "x",
			Provider: ProviderConfig{
				BaseURL: litHTTPSGwExampleCom, APIKey: "k",
				Models: map[string]ModelConfig{"m": {Context: 100_000}},
			},
		}
	}

	good := base()

	good.Agent.MaxTime = "45m"
	require.NoError(t, good.Validate(), "a valid duration must pass")

	empty := base() // unbounded is valid
	require.NoError(t, empty.Validate(), "an empty max_time must pass (unbounded)")

	bad := base()

	bad.Agent.MaxTime = "half an hour"
	require.Error(t, bad.Validate(), "a malformed max_time must fail validation")

	negative := base()

	negative.Agent.MaxTime = "-5m"
	require.Error(t, negative.Validate(), "a negative max_time must fail validation")
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
	require.NoError(t, err)

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
		assert.NotEqual(t, 0, got, "%s was not read from the file", name)
	}

	// and the string parses through to a real duration
	d, err := a.MaxDuration()
	require.NoError(t, err)
	assert.Equal(t, "45m0s", d.String())
}

// The thresholds are validated at load, where the operator is looking, not when
// the run starts.
func TestContextThresholdsAreReadAndValidated(t *testing.T) {
	cfg, err := Load(writeConfig(t, "agent:\n  context_soft: 30\n  context_hard: 70\n"))
	require.NoError(t, err)

	assert.Equal(t, 30, cfg.Agent.ContextSoft)
	assert.Equal(t, 70, cfg.Agent.ContextHard)

	for _, bad := range [][2]int{{95, 0}, {60, 60}, {0, 100}, {-1, 0}} {
		err := validConfig(func(c *Config) { c.Agent.ContextSoft, c.Agent.ContextHard = bad[0], bad[1] }).Validate()

		require.Error(t, err, "want them refused and named")
		assert.Contains(t, err.Error(), "context", "want them refused and named")
	}
}

func TestPlanKnobsAreReadAndValidated(t *testing.T) {
	cfg, err := Load(writeConfig(t, "agent:\n  plan_nudge_every: -1\n  plan_min_turns: 8\n"))
	require.NoError(t, err)

	assert.Equal(t, -1, cfg.Agent.PlanNudgeEvery)
	assert.Equal(t, 8, cfg.Agent.PlanMinTurns)

	err = validConfig(func(c *Config) { c.Agent.PlanNudgeEvery = -1 }).Validate()
	require.NoError(t, err, "a negative plan_nudge_every turns the reminders off, got %v", err)

	require.Error(t, validConfig(func(c *Config) { c.Agent.PlanMinTurns = -1 }).Validate(), "a negative plan_min_turns was accepted")
}

func TestToolOutputPercentIsReadAndValidated(t *testing.T) {
	cfg, err := Load(writeConfig(t, "agent:\n  max_tool_output_percent: 10\n"))
	require.NoError(t, err)

	assert.Equal(t, 10, cfg.Agent.MaxToolOutputPercent, "max_tool_output_percent = %d, want 10", cfg.Agent.MaxToolOutputPercent)

	for _, bad := range []int{-1, 101} {
		require.Error(t, validConfig(func(c *Config) { c.Agent.MaxToolOutputPercent = bad }).Validate(), "max_tool_output_percent %d was accepted", bad)
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

			_, err := Load(path)
			require.Error(t, err, "agent.%s must be rejected", key)
		})
	}
}

// skills_dir names the folder skills are loaded from. It is read as written and
// expanded by the caller, which knows the working directory.
// The system prompt is the config's, so a config without one does not validate, and the file's own text is what is read.
func TestThePromptIsRequiredAndReadAsWritten(t *testing.T) {
	require.Error(t, validConfig(func(c *Config) { c.Prompt = "" }).Validate(), "a config with no prompt validated")
	require.Error(t, validConfig(func(c *Config) { c.Prompt = " \n " }).Validate(), "a blank prompt validated")

	cfg, err := Load(writeConfig(t, "prompt: |\n  Hello {{ .Objective }}.\n\n  Bye.\n"))
	require.NoError(t, err)

	assert.Equal(t, "Hello {{ .Objective }}.\n\nBye.\n", cfg.Prompt)
}

func TestSkillsDirIsRead(t *testing.T) {
	cfg, err := Load(writeConfig(t, "skills_dir: ~/skills\n"))
	require.NoError(t, err)

	assert.Equal(t, "~/skills", cfg.SkillsDir, "skills_dir = %q, want it as written", cfg.SkillsDir)
}

// reasoning_effort and extra_body are per-model request settings. Read as
// written, and an effort the provider would reject is rejected at load.
func TestAModelCarriesItsRequestSettings(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
prompt: x
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
	require.NoError(t, err)

	model := cfg.Provider.Models["local"]

	assert.Equal(t, "Low", model.ReasoningEffort, "reasoning_effort = %q, want it as written", model.ReasoningEffort)

	kwargs, _ := model.ExtraBody["chat_template_kwargs"].(map[string]any)
	thinking, ok := kwargs["enable_thinking"].(bool)
	assert.True(t, ok, "want the nested settings kept")
	assert.False(t, thinking, "want the nested settings kept")

	require.NoError(t, cfg.Validate(), "a known effort (any case) must validate")

	cfg.Provider = ProviderConfig{BaseURL: "http://127.0.0.1:8080/v1", Models: map[string]ModelConfig{
		"local": {Context: 8000, ReasoningEffort: "extreme"},
	}}

	err = cfg.Validate()
	require.Error(t, err, "want an unknown effort refused and named")
	assert.Contains(t, err.Error(), "reasoning_effort", "want an unknown effort refused and named")
}

// The viewer scrollback is a scalar UI field read from the file, and an
// out-of-range value is rejected at load.
func TestUIScrollbackIsReadAndValidated(t *testing.T) {
	path := writeConfig(t, `
ui:
  scrollback: 20000
`)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 20000, cfg.UI.Scrollback)

	require.Error(t, validConfig(func(c *Config) { c.UI.Scrollback = -1 }).Validate(), "a negative ui.scrollback must fail validation")
}

// The header is fixed, so ui.stats is gone rather than ignored. A config that
// still sets it must fail at load and name the key, not show a header
// the operator did not ask for.
func TestUIStatsIsNoLongerAKey(t *testing.T) {
	path := writeConfig(t, `
ui:
  stats: [model, iter]
`)

	_, err := Load(path)
	require.Error(t, err, "want the removed ui.stats key refused and named")
	assert.Contains(t, err.Error(), "stats", "want the removed ui.stats key refused and named")
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
	require.NoError(t, err)

	assert.Equal(t, "sk-proxy", cfg.Provider.APIKey, "want the explicitly configured one")
}

// The config is what everything else reads and validates itself against, so it
// must not depend on any of it. A rule written here is written once.
func TestConfigImportsNoOtherPackageOfTheModule(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	require.NoError(t, err)

	require.Contains(t, pkg.Imports, "gopkg.in/yaml.v3", "the check reads no imports at all")

	for _, path := range pkg.Imports {
		assert.NotContains(t, path, "openzot/openzot/")
	}
}
