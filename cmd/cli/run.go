package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/outcome"
	"github.com/openzot/openzot/internal/plan"
	"github.com/openzot/openzot/internal/provider"
	"github.com/openzot/openzot/internal/render"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/skills"
	"github.com/openzot/openzot/internal/tools"
	"github.com/openzot/openzot/internal/tui"
)

// The file agent looks for under each context directory.
const agentFile = "AGENTS.md"

// taskKickoff is the user message that starts a run. The goal is in the
// instructions. This only has to get the agent moving.
const taskKickoff = "Begin working on your task. Start by calling the tasks tool to list the work, then carry it through to completion."

// loadProjectContext reads the AGENTS.md found under the given directories, searched in order (typically the config directory
// then the working directory). Missing files are ignored and duplicate directories are searched once. The config's prompt
// decides whether and where to use them, as .Project.
func loadProjectContext(dirs ...string) string {
	seen := map[string]bool{}

	var found []string

	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}

		seen[dir] = true

		if data, err := os.ReadFile(filepath.Join(dir, agentFile)); err == nil { //nolint:gosec // G304: AGENTS.md in the config and project directories
			if text := strings.TrimSpace(string(data)); text != "" {
				found = append(found, text)
			}
		}
	}

	return strings.Join(found, "\n\n---\n\n")
}

// loadSkills reads the skills folder named by skills_dir. An unset skills_dir
// means no skills. A set one that cannot be read is an error, since the config
// asked for skills the run would otherwise silently lack.
func loadSkills(skillsDir string) ([]skills.Skill, error) {
	dir := strings.TrimSpace(skillsDir)
	if dir == "" {
		return nil, nil
	}

	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("skills_dir %q: %w", skillsDir, err)
		}

		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}

	loaded, err := skills.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("skills_dir: %w", err)
	}

	return loaded, nil
}

// runOptions configures a run beyond the configuration itself.
type runOptions struct {
	// SessionPath is the log this run is appended to. One file per task, so a
	// run of the same task again adds to it. Empty disables recording.
	SessionPath string

	// A short label for the work, shown in the viewer instead of the task text. A work order's title or its
	// file name. Presentation only, and never sent to the model. The goal is the contract.
	Title string

	// Project is the instructions found in the AGENTS.md files of the config
	// directory and the project, for the config's prompt to use as .Project.
	Project string

	// Skills are the skills loaded at startup, offered to the model through the
	// skills tool.
	Skills []skills.Skill

	// Viewer shows the run. Nil is the full-screen viewer. It is a field so that
	// what needs a terminal can be replaced by what does not.
	Viewer func(context.Context, tui.Meta, *loop.Options) (loop.Result, error)
}

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

// digestStatus maps a run's stop reason and exit code to the one human word a
// digest shows. "done", "failed", or "canceled".
func digestStatus(reason string, code int) string {
	switch reason {
	case string(outcome.StopAborted):
		return "canceled"
	case string(outcome.StopFailed):
		return "failed"
	}

	if code != 0 {
		return "failed"
	}

	return "done"
}

// printDigest writes the end-of-run digest. The outcome, what the run spent,
// and - when the run was recorded - the session log it was appended to.
func printDigest(w io.Writer, sessionPath string, result *loop.Result) {
	digest := render.Digest{
		Status:       digestStatus(string(result.Reason), result.ExitCode()),
		Session:      sessionPath,
		Iterations:   result.Budget.Iterations,
		Calls:        result.Budget.Calls,
		InputTokens:  result.Budget.InputTokens,
		OutputTokens: result.Budget.OutputTokens,
		Message:      result.Message,
	}

	fmt.Fprintf(w, "\n%s", render.RenderDigest(digest))
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
	toolOutput := conversation.BytesForTokens(contextWindow * cmp.Or(cfg.Agent.MaxToolOutputPercent, tools.DefaultOutputPercent) / 100)

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

// runOrder executes one autonomous coding task, rendering the agent's activity in the read-only TUI. The tools operate on the
// current working directory, so the caller chdirs into the project first. It blocks until the user quits the viewer or the
// run errors.
func runOrder(ctx context.Context, cfg *config.Config, o order.Order, options runOptions) error {
	config.ScrubProviderSecrets(cfg)

	client, opts, err := resolve(ctx, cfg, options.Skills)
	if err != nil {
		return err
	}

	workdir, _ := os.Getwd()

	// The config's prompt, filled in from the order, is the system prompt. The goal, criteria and constraints survive
	// trimming there, and the opening user message only gets the agent moving. Rendered after the secrets leave the environment.

	// There is no way to open a run with a prompt of the caller's own. Agent takes a work order, not a
	// conversation, and anything worth saying to the agent belongs in the order, where it is durable.
	prompt, err := o.Render(cfg.Prompt, orderEnv(cfg, client, &opts, workdir, options.SessionPath, options.Project))
	if err != nil {
		return fmt.Errorf("order %s: %w", cmp.Or(o.Path, "(unsaved)"), err)
	}

	opts.Instructions = prompt
	opts.Messages = []conversation.Message{{Type: conversation.TypeUser, Text: taskKickoff}}

	task := o.Objective

	// The session log is not optional. It is the run's record and the agent's long-term memory, so a run
	// that cannot be recorded is rejected rather than run without either.
	if options.SessionPath == "" {
		return errors.New("no session log: a run is always recorded")
	}

	writer, err := session.Open(options.SessionPath, session.Meta{
		Task:     task,
		Model:    client.Config().Model,
		Provider: cfg.Provider.Label(),
		Workdir:  workdir,
	})
	if err != nil {
		return fmt.Errorf("session log: %w", err)
	}

	defer writer.Close()

	// A log that stops being writable ends the run. What it cannot record it
	// should not go on doing.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	recorder := newRecorder(writer, func(error) { stop() })

	// The seed is recorded before the run starts so a session that dies in its
	// first turn still says what it was asked to do.
	recorder.Conversation(opts.Messages)

	opts.OnConversation = recorder.Conversation
	opts.OnEvent = recorder.Event

	meta := viewerMeta(cfg, task, workdir, &opts)
	meta.Title = options.Title

	viewer := options.Viewer
	if viewer == nil {
		viewer = tui.Run
	}

	result, err := viewer(ctx, meta, &opts)

	// A run that never began, or was abandoned still going, has no ending to write
	// down or to report.
	if result.Reason != "" {
		recorder.Result(&result)

		printDigest(os.Stderr, writer.Path(), &result)
	}

	if failed := recorder.Err(); failed != nil {
		return fmt.Errorf("session log: %w", failed)
	}

	return err
}
