package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/openzot/openzot/internal/catalogue"
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
		Agent:           Agent{Model: "m", MaxIterations: 1},
		DefaultProvider: "openai",
		Providers:       map[string]ProviderConfig{"openai": {BaseURL: "https://gw.example.com/v1", APIKey: "x"}},
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
	if c.DefaultProvider != "" {
		t.Errorf("default provider = %q, want none", c.DefaultProvider)
	}
	if len(c.Providers) != 0 {
		t.Errorf("default providers = %v, want none", c.Providers)
	}
	if c.Agent.MaxIterations <= 0 {
		t.Error("expected a positive default max_iterations")
	}
}

// Nothing is seeded, and no conventional credential variable is read: a config
// that declares no providers has none, whatever the environment holds.
func TestLoadSeedsNoProviders(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZAI_API_KEY", "sk-zai")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Providers) != 0 {
		t.Errorf("providers = %v, want none", cfg.Providers)
	}

	if cfg.DefaultProvider != "" {
		t.Errorf("default provider = %q, want none", cfg.DefaultProvider)
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("a config with no provider or model must not validate")
	}
}

// A provider named after a service that used to be built in is no different
// from any other name: it gets no endpoint and no ambient key.
func TestANameThatWasOnceBuiltInGetsNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-openai")

	path := writeConfig(t, `
agent:
  model: gpt-5.4
default_provider: openai
providers:
  openai:
    driver: openai
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	provider := cfg.Providers["openai"]

	if provider.BaseURL != "" || ProviderCredential(provider) != "" {
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
		"driver selector": `
default_provider: corporate
providers:
  corporate:
    provider: openai
	`,
		"attribution": `
attribution:
  name: acme-bot
	`,
		"per-model tools override": `
default_provider: corporate
providers:
  corporate:
    base_url: https://gw.example.com/v1
    models:
      fast:
        tools: false
	`,
		"per-model reasoning override": `
default_provider: corporate
providers:
  corporate:
    base_url: https://gw.example.com/v1
    models:
      fast:
        reasoning: true
	`,
		"per-model driver": `
default_provider: corporate
providers:
  corporate:
    base_url: https://gw.example.com/v1
    models:
      fast:
        driver: openai
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

// Env vars override the file (defaults < file < env). CLI flags override env,
// but that layer lives in main.
func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("GROQ_KEY", "sk-test")
	path := writeConfig(t, `
agent:
  model: from-file
default_provider: openai
providers:
  groq:
    base_url: https://groq.example.com/v1
    api_key: $GROQ_KEY
`)
	t.Setenv("ZOT_AGENT_MODEL", "gpt-4o")
	t.Setenv("ZOT_AGENT_MAX_ITERATIONS", "12")
	t.Setenv("ZOT_DEFAULT_PROVIDER", "groq")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o (env overrides file)", cfg.Agent.Model)
	}
	if cfg.Agent.MaxIterations != 12 {
		t.Errorf("max_iterations = %d, want 12", cfg.Agent.MaxIterations)
	}
	if cfg.DefaultProvider != "groq" {
		t.Errorf("default provider = %q, want groq (env overrides file)", cfg.DefaultProvider)
	}
	if got := ProviderCredential(cfg.Providers["groq"]); got != "sk-test" {
		t.Errorf("groq credential = %q, want it from GROQ_KEY", got)
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
default_provider: openai
providers:
  openai:
    api_key: '$MY_PROVIDER_KEY'
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["openai"].APIKey; got != "sk-from-env" {
		t.Errorf("resolved secret = %q, want sk-from-env", got)
	}
}

// A provider key and a per-model key may each be written as a $VAR, and both are
// resolved.
func TestAuthorizationEnvReference(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GATEWAY_DEFAULT_KEY", "sk-gateway-default")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	path := writeConfig(t, `
default_provider: mygateway
providers:
  mygateway:
    api_key: '$GATEWAY_DEFAULT_KEY'
    models:
      gpt-4:
        api_key: $OPENAI_API_KEY
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["mygateway"].APIKey; got != "sk-gateway-default" {
		t.Errorf("provider key = %q, want sk-gateway-default", got)
	}
	if got := cfg.Providers["mygateway"].Models["gpt-4"].APIKey; got != "sk-openai" {
		t.Errorf("model key = %q, want sk-openai", got)
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
	if err := validConfig(func(c *Config) { c.DefaultProvider = "nope" }).Validate(); err == nil {
		t.Error("expected an error for an unknown default provider")
	}
	if err := validConfig(func(c *Config) { c.DefaultProvider = "" }).Validate(); err == nil {
		t.Error("expected an error when no provider is selected")
	}
	if err := validConfig(func(c *Config) {
		c.Providers["openai"] = ProviderConfig{APIKey: "x"}
	}).Validate(); err == nil {
		t.Error("expected an error for a provider with no base_url")
	}
	if err := validConfig(func(c *Config) {
		c.Providers["openai"] = ProviderConfig{Driver: "anthropic", BaseURL: "https://gw.example.com/v1", APIKey: "x"}
	}).Validate(); err == nil {
		t.Error("expected an error for a driver other than openai")
	}
	if err := validConfig(func(c *Config) {
		c.Providers["openai"] = ProviderConfig{Driver: "openai", BaseURL: "https://gw.example.com/v1", APIKey: "x"}
	}).Validate(); err != nil {
		t.Errorf("the openai driver was rejected: %v", err)
	}
	if err := validConfig(func(c *Config) {
		c.Providers["openai"] = ProviderConfig{BaseURL: "https://gw.example.com/v1", Models: map[string]ModelConfig{"allowed": {Model: "gpt-5.4"}}}
	}).Validate(); err == nil {
		t.Error("expected a custom provider model list to reject an unlisted model")
	}
	if err := validConfig(func(c *Config) {
		c.Agent.Model = "allowed"
		c.Providers["openai"] = ProviderConfig{BaseURL: "https://gw.example.com/v1", Models: map[string]ModelConfig{"allowed": {Model: "gpt-5.4"}}}
	}).Validate(); err != nil {
		t.Errorf("custom provider model was rejected: %v", err)
	}
}

func TestProviderModelsIsTheCustomListOrNothing(t *testing.T) {
	custom := ProviderConfig{Models: map[string]ModelConfig{
		"small": {Model: "gpt-5.4-mini"},
		"large": {Model: "gpt-5.4"},
	}}
	if got, want := ProviderModels(custom), []string{"large", "small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("custom models = %v, want %v", got, want)
	}
	if got := ProviderModels(ProviderConfig{}); len(got) != 0 {
		t.Fatalf("no custom list should mean no restriction, got %v", got)
	}
}

func TestProviderDriverDefaultsToOpenAI(t *testing.T) {
	if got := ProviderDriver(ProviderConfig{}); got != DriverOpenAI {
		t.Errorf("driver = %q, want %q", got, DriverOpenAI)
	}
}

// Scrubbing removes every resolved credential - Bearer secrets and provider
// keys, provider-level and per-model - from the environment, while
// leaving unrelated variables intact.
func TestScrubProviderSecrets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZAI_API_KEY", "sk-zai")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZOT_TEST_UNRELATED", "keep-me")
	path := writeConfig(t, `
default_provider: mygateway
providers:
  mygateway:
    api_key: $ZAI_API_KEY
    models:
      gpt-4:
        api_key: $OPENAI_API_KEY
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ScrubProviderSecrets(cfg)

	if _, ok := os.LookupEnv("ZAI_API_KEY"); ok {
		t.Error("ZAI_API_KEY should be removed after scrub")
	}
	if _, ok := os.LookupEnv("OPENAI_API_KEY"); ok {
		t.Error("OPENAI_API_KEY should be removed after scrub")
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

// Env overrides are typed, and a bad value has to be reported rather than
// silently zeroed - a max_iterations of 0 would end every run immediately.
func TestEnvOverrideTypeErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "k")

	t.Setenv("ZOT_AGENT_MAX_ITERATIONS", "not-a-number")

	if _, err := Load(""); err == nil {
		t.Error("a non-numeric max_iterations must be reported")
	}

	t.Setenv("ZOT_AGENT_MAX_ITERATIONS", "42")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.MaxIterations != 42 {
		t.Errorf("max_iterations = %d, want 42", cfg.Agent.MaxIterations)
	}
}

func TestEnvOverrideBooleans(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("ZOT_UI_PLAIN", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.UI.Plain {
		t.Error("ui.plain should be set from ZOT_UI_PLAIN")
	}

	t.Setenv("ZOT_UI_PLAIN", "maybe")

	if _, err := Load(""); err == nil {
		t.Error("a non-boolean ui.plain must be reported")
	}
}

// A provider brings its own endpoint. There is none to fall back on, and
// finding out mid-run that there is nowhere to send the request is worse than
// at load.
func TestValidateRejectsAnUnreachableProvider(t *testing.T) {
	cfg := Defaults()
	cfg.Agent.Model = "m"
	cfg.DefaultProvider = "mygateway"

	cfg.Providers = map[string]ProviderConfig{"mygateway": {}}

	if err := cfg.Validate(); err == nil {
		t.Error("a provider with no base_url must be rejected")
	}

	// a name that used to be built in gets no special treatment
	cfg.DefaultProvider = "groq"
	cfg.Providers = map[string]ProviderConfig{"groq": {}}

	if err := cfg.Validate(); err == nil {
		t.Error("a provider named after a former built-in still needs a base_url")
	}

	cfg.Providers["groq"] = ProviderConfig{BaseURL: "https://gw.example.com/v1"}

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
			cfg := Config{Providers: map[string]ProviderConfig{"mine": test.provider}}

			resolveProviders(&cfg)

			if got := ProviderCredential(cfg.Providers["mine"]); got != "sk-resolved" {
				t.Errorf("credential = %q, want the expanded value", got)
			}
		})
	}
}

// An unset variable must resolve to nothing, so the run fails with "no API key
// configured" rather than sending the literal reference to the provider.
func TestAnUnsetEnvReferenceResolvesToNothing(t *testing.T) {
	t.Setenv("ZOT_TEST_UNSET_KEY", "")

	cfg := Config{Providers: map[string]ProviderConfig{
		"mine": {Driver: "custom", APIKey: "$ZOT_TEST_UNSET_KEY"},
	}}

	resolveProviders(&cfg)

	if got := ProviderCredential(cfg.Providers["mine"]); got != "" {
		t.Errorf("credential = %q, want nothing", got)
	}
}

// A literal key is left exactly as written - a provider key that happens to
// contain a dollar sign is not a reference.
func TestALiteralCredentialIsUntouched(t *testing.T) {
	cfg := Config{Providers: map[string]ProviderConfig{
		"mine": {APIKey: "sk-literal-with-$-inside"},
	}}

	resolveProviders(&cfg)

	if got := ProviderCredential(cfg.Providers["mine"]); got != "sk-literal-with-$-inside" {
		t.Errorf("credential = %q, want it untouched", got)
	}
}

// The credential is only what the config says. A conventional variable named
// after the provider is never consulted, however well it matches.
func TestNoConventionalVariableIsRead(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	cfg := Config{Providers: map[string]ProviderConfig{"openai": {}}}

	resolveProviders(&cfg)

	if got := ProviderCredential(cfg.Providers["openai"]); got != "" {
		t.Errorf("credential = %q, want none: nothing is read on the provider's behalf", got)
	}
}

// max_time is validated at load, so a typo is caught immediately rather than
// silently ignored into an unbounded run.
func TestMaxTimeIsValidated(t *testing.T) {
	base := func() Config {
		return Config{
			Agent:           Agent{Model: "m", MaxIterations: 10},
			DefaultProvider: "openai",
			Providers:       map[string]ProviderConfig{"openai": {BaseURL: "https://gw.example.com/v1", APIKey: "k"}},
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

// Every run budget is settable from the environment, not just max_iterations -
// the config comment promises a ZOT_AGENT_MAX_* for each, so this pins that the
// reflection-based override actually reaches all of them (including the two-word
// max_continuations, whose env name is the easy one to get wrong).
func TestEveryBudgetHasAnEnvOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")

	t.Setenv("ZOT_AGENT_MAX_SETTLES", "3")
	t.Setenv("ZOT_AGENT_MAX_CALLS", "4")
	t.Setenv("ZOT_AGENT_MAX_TIME", "45m")
	t.Setenv("ZOT_AGENT_MAX_TOKENS", "5")
	t.Setenv("ZOT_AGENT_MAX_CONTINUATIONS", "6")
	t.Setenv("ZOT_AGENT_MAX_RECOVERIES", "9")
	t.Setenv("ZOT_AGENT_MAX_CYCLES", "7")
	t.Setenv("ZOT_AGENT_MAX_EMPTIES", "8")

	cfg, err := Load("")
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
			t.Errorf("%s was not set from its ZOT_AGENT_* variable", name)
		}
	}

	if a.MaxTime != "45m" {
		t.Errorf("max_time = %q, want 45m from the environment", a.MaxTime)
	}

	// and the string parses through to a real duration
	if d, err := a.MaxDuration(); err != nil || d.String() != "45m0s" {
		t.Errorf("MaxDuration = %v (err %v), want 45m", d, err)
	}
}

// limit_checkpoints is validated so a typo (a percentage over 99, or a negative)
// is caught at load rather than silently dropped into a run with no notices.
func TestLimitCheckpointsAreValidated(t *testing.T) {
	base := func(cp []int) Config {
		return Config{
			Agent:           Agent{Model: "m", MaxIterations: 10, LimitCheckpoints: cp},
			DefaultProvider: "openai",
			Providers:       map[string]ProviderConfig{"openai": {BaseURL: "https://gw.example.com/v1", APIKey: "k"}},
		}
	}

	if err := base([]int{50, 80, 90}).Validate(); err != nil {
		t.Errorf("a valid checkpoint list must pass: %v", err)
	}

	if err := base(nil).Validate(); err != nil {
		t.Errorf("unset checkpoints must pass (uses the default): %v", err)
	}

	if err := base([]int{}).Validate(); err != nil {
		t.Errorf("an empty list must pass (disables notices): %v", err)
	}

	for _, bad := range [][]int{{0}, {100}, {150}, {-5}, {50, 200}} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("checkpoints %v must fail validation", bad)
		}
	}
}

// A standard build carries no compiled-in config, so nothing overrides the
// runtime file and Portable reads false. The portable build path is exercised
// by building with -tags portable, which this suite does not; applyPortableOverlay
// below covers the layering that path relies on.
func TestPortableIsOffInAStandardBuild(t *testing.T) {
	if Portable() {
		t.Error("a build without -tags portable must not report a compiled-in config")
	}
	if data := portableConfig(); data != nil {
		t.Errorf("portableConfig must be empty in a standard build, got %q", data)
	}
}

// The overlay is what makes a portable build authoritative: it is applied after
// the file and env, so a field it sets wins. A field it omits must be left
// exactly as the earlier layers resolved it - that is what lets an operator bake
// the model while still taking the key from the environment.
func TestPortableOverlayOverridesOnlyWhatItSets(t *testing.T) {
	// stand in for "the file and env already resolved this"
	cfg := Config{
		Agent:           Agent{Model: "from-env", MaxIterations: 42},
		DefaultProvider: "openai",
		Providers: map[string]ProviderConfig{
			"openai": {APIKey: "sk-from-env"},
		},
	}

	overlay := []byte(`
agent:
  model: baked-model
default_provider: zai
providers:
  zai:
    api_key: sk-baked
`)

	if err := applyPortableOverlay(&cfg, overlay); err != nil {
		t.Fatalf("applyPortableOverlay: %v", err)
	}

	// fields the overlay set are authoritative
	if cfg.Agent.Model != "baked-model" {
		t.Errorf("model = %q, want the baked value to win", cfg.Agent.Model)
	}
	if cfg.DefaultProvider != "zai" {
		t.Errorf("default_provider = %q, want the baked value to win", cfg.DefaultProvider)
	}
	if got := cfg.Providers["zai"].APIKey; got != "sk-baked" {
		t.Errorf("baked provider key = %q, want sk-baked", got)
	}

	// fields it did NOT set fall through untouched
	if cfg.Agent.MaxIterations != 42 {
		t.Errorf("max_iterations = %d, want the earlier layer (42) to survive an overlay that omits it", cfg.Agent.MaxIterations)
	}
	if got := cfg.Providers["openai"].APIKey; got != "sk-from-env" {
		t.Errorf("a provider the overlay did not mention was lost: openai key = %q", got)
	}
}

// An empty overlay - the standard build - must be a no-op, not a wipe.
func TestPortableOverlayEmptyIsANoOp(t *testing.T) {
	cfg := Config{Agent: Agent{Model: "keep", MaxIterations: 7}}

	if err := applyPortableOverlay(&cfg, nil); err != nil {
		t.Fatalf("applyPortableOverlay(nil): %v", err)
	}
	if cfg.Agent.Model != "keep" || cfg.Agent.MaxIterations != 7 {
		t.Errorf("an empty overlay changed the config: %+v", cfg.Agent)
	}
}

// Malformed baked YAML must fail loudly at load, not silently ship a binary that
// ignores its own compiled-in config.
func TestPortableOverlayRejectsBadYAML(t *testing.T) {
	cfg := Config{}
	if err := applyPortableOverlay(&cfg, []byte("agent: [not-a-mapping")); err == nil {
		t.Error("malformed compiled-in config must be an error")
	}
}

// The context-strategy and compact-tuning knobs are gone: the only strategy is
// truncation, so there is nothing to select or tune. A config that still sets
// them fails loudly rather than being silently ignored.
func TestRemovedContextKnobsAreRejected(t *testing.T) {
	for _, key := range []string{
		"context_strategy: truncate",
		"context_strategy: compact",
		"compact_trigger_ratio: 0.8",
		"compact_min_tokens: 1000",
		"compact_min_messages: 10",
	} {
		t.Run(key, func(t *testing.T) {
			path := writeConfig(t, "agent:\n  "+key+"\n")

			if _, err := Load(path); err == nil {
				t.Errorf("agent.%s must be rejected", key)
			}
		})
	}
}

// The viewer scrollback is a scalar UI field, so it takes the ZOT_UI_* env
// override like the others, and a negative value is rejected at load.
func TestScrollbackEnvOverrideAndValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("ZAI_API_KEY", "sk-zai")
	t.Setenv("ZOT_UI_SCROLLBACK", "20000")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.UI.Scrollback != 20000 {
		t.Errorf("ZOT_UI_SCROLLBACK not applied: %d", cfg.UI.Scrollback)
	}

	if err := validConfig(func(c *Config) { c.UI.Scrollback = -1 }).Validate(); err == nil {
		t.Error("a negative ui.scrollback must fail validation")
	}
}

// Stream color is a capability declaration, so containers must be able to set
// it through the same scalar environment layer as the rest of ui.*.
func TestUIColorEnvOverrideAndValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("ZAI_API_KEY", "sk-zai")
	t.Setenv("ZOT_UI_COLOR", "always")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UI.Color != "always" {
		t.Errorf("ZOT_UI_COLOR not applied: %q", cfg.UI.Color)
	}
	if err := validConfig(func(c *Config) { c.UI.Color = "sometimes" }).Validate(); err == nil {
		t.Error("an unknown ui.color mode must fail validation")
	}
}

// ui.stats selects header fields; an unknown field name is a typo worth catching
// at load rather than silently dropping the field.
func TestUIStatsAreValidated(t *testing.T) {
	if err := validConfig(func(c *Config) { c.UI.Stats = []string{"model", "iter"} }).Validate(); err != nil {
		t.Errorf("a valid stat list must pass: %v", err)
	}

	if err := validConfig(func(c *Config) { c.UI.Stats = []string{"model", "bogus"} }).Validate(); err == nil {
		t.Error("an unknown stat field must fail validation")
	}

	if err := validConfig(func(c *Config) { c.UI.Stats = nil }).Validate(); err != nil {
		t.Errorf("an unset stat list must pass (defaults): %v", err)
	}
}

// A key written for the endpoint is exactly what it uses, and the ambient
// variable of the same provider name plays no part.
func TestAConfiguredKeyIsTheOneUsed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("PROXY_KEY", "sk-proxy")
	path := writeConfig(t, `
providers:
  openai:
    base_url: https://proxy.example.com/v1
    api_key: $PROXY_KEY
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["openai"].APIKey; got != "sk-proxy" {
		t.Errorf("openai key = %q, want the explicitly configured one", got)
	}
}

func TestModelCapabilitiesDeferToTheCatalogueWhenUnset(t *testing.T) {
	base := catalogue.Model{SupportsVision: true, ContextWindow: 200_000}

	got := ModelConfig{}.Capabilities(base)

	if got != base {
		t.Errorf("Capabilities changed %+v to %+v with nothing set", base, got)
	}
}

func TestModelCapabilitiesTurnSightOnAndOff(t *testing.T) {
	base := catalogue.Model{}

	on := true
	off := false

	if got := (ModelConfig{Vision: &on}).Capabilities(base); !got.SupportsVision {
		t.Error("vision: true must let a model zot has not catalogued be shown images")
	}

	seeing := catalogue.Model{SupportsVision: true}

	if got := (ModelConfig{Vision: &off}).Capabilities(seeing); got.SupportsVision {
		t.Error("vision: false must be able to turn off what the catalogue believes")
	}

}

func TestModelCapabilitiesApplyTheContextOverrideToo(t *testing.T) {
	base := catalogue.Model{ContextWindow: 1_000_000}

	if got := (ModelConfig{Context: 32_000}).Capabilities(base); got.ContextWindow != 32_000 {
		t.Errorf("context window = %d, want the override", got.ContextWindow)
	}

	if got := (ModelConfig{}).Capabilities(base); got.ContextWindow != 1_000_000 {
		t.Errorf("context window = %d, want the catalogue's when unset", got.ContextWindow)
	}
}

func TestModelCapabilitiesParseFromYAML(t *testing.T) {
	var parsed struct {
		Models map[string]ModelConfig `yaml:"models"`
	}

	source := "models:\n  stealth/ox-alpha:\n    vision: true\n  blinkered:\n    vision: false\n  quiet: {}\n"

	if err := yaml.Unmarshal([]byte(source), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed.Models["stealth/ox-alpha"].Vision == nil || !*parsed.Models["stealth/ox-alpha"].Vision {
		t.Error("vision: true must parse as an explicit yes")
	}

	if parsed.Models["blinkered"].Vision == nil || *parsed.Models["blinkered"].Vision {
		t.Error("vision: false must parse as an explicit no, not as absent")
	}

	if parsed.Models["quiet"].Vision != nil {
		t.Error("an unstated capability must stay unstated, so the catalogue decides")
	}
}

// The release check is the one request a run makes that is not to the
// configured provider, so turning it off has to work from the file and from
// the environment - the air-gapped hosts and locked-down CI jobs that need the
// opt-out are exactly the ones that may never write a config file. And the
// default must be on: an opt-out that defaults to off is not an opt-out.
func TestTheUpdateCheckCanBeDisabledFromFileAndEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")

	def, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if def.UpdateCheck.Disabled {
		t.Error("a bare config must leave the update check enabled")
	}

	path := writeConfig(t, `
update_check:
  disabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.UpdateCheck.Disabled {
		t.Error("update_check.disabled was not read from the file")
	}

	t.Setenv("ZOT_UPDATE_CHECK_DISABLED", "true")

	fromEnv, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !fromEnv.UpdateCheck.Disabled {
		t.Error("ZOT_UPDATE_CHECK_DISABLED did not reach the config")
	}
}
