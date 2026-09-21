package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/plan"
	"github.com/openzot/openzot/internal/provider"
	"github.com/openzot/openzot/internal/skills"
	"github.com/openzot/openzot/internal/tools"
	"github.com/openzot/openzot/internal/tui"
	"github.com/openzot/openzot/internal/window"
)

// orderEnv is what the prompt can know about the run beyond the order. The
// tools it really has, where it is working, and what it is talking to.
func orderEnv(cfg *config.Config, client *provider.Client, opts *loop.Options, workdir, sessionPath, project string) order.Env {
	env := order.Env{
		Workdir:  workdir,
		Date:     time.Now().Format("2006-01-02"),
		Model:    client.Config().Model,
		Provider: cfg.Provider.Label(),
		Project:  project,
		Session:  sessionPath,
	}

	for _, tool := range opts.Tools {
		info := tool.Info()

		env.Tools = append(env.Tools, order.Tool{Name: info.Name, Description: info.Description})
	}

	return env
}

// viewerMeta describes the run to the viewer. Its budgets are the ones the run was resolved with, not the raw config, since
// a per-model max_iterations lowers the engine's limit and a bar counting to a number the run never reaches misreports it.
func viewerMeta(cfg *config.Config, task, workdir string, opts *loop.Options) tui.Meta {
	// Show the iteration progress denominator only for a real user-set limit -
	// the default is a 1,000,000 fallback, which is not a budget worth displaying.
	iterLimit := 0
	if opts.MaxIterations != config.Defaults().Agent.MaxIterations {
		iterLimit = opts.MaxIterations
	}

	return tui.Meta{
		Task:          task,
		Model:         cfg.Agent.Model,
		Provider:      cfg.Provider.Label(),
		Workdir:       workdir,
		MaxScrollback: cfg.UI.Scrollback,
		MaxIterations: iterLimit,
		MaxDuration:   opts.MaxDuration,
	}
}

// resolve turns a configuration into a provider client and the agent options a
// run uses. The returned options carry no messages. Callers supply those.
func resolve(ctx context.Context, cfg *config.Config, offered []skills.Skill) (*provider.Client, loop.Options, error) {
	var empty loop.Options

	providerConfig := cfg.Provider

	if providerConfig.BaseURL == "" {
		return nil, empty, errors.New(
			"no provider: declare one under provider: in the config, with a base_url, an api_key and its models")
	}

	// resolve the model against the provider's model definitions. An entry's
	// settings take priority over the run defaults.
	model := cfg.Agent.Model
	maxIterations := cfg.Agent.MaxIterations

	// Every model is declared with its own context window, and Validate says so at load. The same rule holds
	// here, since a run with no window cannot decide how much of a conversation to keep.
	mc, ok := providerConfig.Models[model]
	if !ok || mc.Context <= 0 {
		return nil, empty, fmt.Errorf(
			"model %q needs a context window: list it under provider.models with context set", model)
	}

	if mc.Model != "" {
		model = mc.Model
	}

	if mc.MaxIterations > 0 {
		maxIterations = mc.MaxIterations
	}

	contextWindow := mc.Context
	contentArray := mc.ContentArray

	client, err := provider.NewClient(ctx, provider.ClientConfig{
		Provider: cfg.Provider.Label(),
		Model:    model,
		APIKey:   providerConfig.APIKey,
		BaseURL:  providerConfig.BaseURL,

		ContentArray: contentArray,
	})
	if err != nil {
		return nil, empty, fmt.Errorf("provider %s: %w", cfg.Provider.Label(), err)
	}

	// max_time was validated at load, so a parse error here would be a bug. Treat
	// it as unbounded rather than failing a run that already passed validation.
	maxDuration, _ := cfg.Agent.MaxDuration()

	// A single tool result may take a share of the context window, so a
	// small-window model is bounded tighter without being told to be.
	toolOutput := window.BytesForTokens(contextWindow * cmp.Or(cfg.Agent.MaxToolOutputPercent, tools.DefaultOutputPercent) / 100)

	opts := loop.Options{
		Model:           client.Model(),
		ReasoningEffort: strings.ToLower(strings.TrimSpace(mc.ReasoningEffort)),
		ExtraBody:       mc.ExtraBody,

		Tools: tools.New(toolOutput, offered),

		// shell acts on the machine, so a command the model did not finish
		// writing is rejected rather than repaired into one that runs
		Unrepaired: []string{tools.ShellTool},

		MaxIterations:    maxIterations,
		MaxSettles:       cfg.Agent.MaxSettles,
		MaxCalls:         cfg.Agent.MaxCalls,
		MaxContinuations: cfg.Agent.MaxContinuations,
		MaxRecoveries:    cfg.Agent.MaxRecoveries,
		MaxCycles:        cfg.Agent.MaxCycles,
		MaxEmpties:       cfg.Agent.MaxEmpties,
		MaxDuration:      maxDuration,
		PlanTool:         plan.Tool,
		PlanNudgeEvery:   cfg.Agent.PlanNudgeEvery,
		PlanMinTurns:     cfg.Agent.PlanMinTurns,
		ContextSoft:      cfg.Agent.ContextSoft,
		ContextHard:      cfg.Agent.ContextHard,
		ContextWindow:    contextWindow,
	}

	// MaxTokens is a pointer so that "unset" (provider decides) is distinct from
	// an explicit zero. The config uses a positive value to mean "cap here".
	if cfg.Agent.MaxTokens > 0 {
		opts.MaxTokens = new(cfg.Agent.MaxTokens)
	}

	return client, opts, nil
}
