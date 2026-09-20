// Command zot is an automated software factory you watch, not drive.
//
// zot takes work orders, not prompts. A work order is one file: a front matter
// block with the durable objective, the acceptance criteria that define "done"
// and the constraints the work must hold to, then the system prompt itself, a
// Go template that reads that block. Each order becomes one autonomous run: the
// agent reads files, edits them, and runs shell commands on its own while the
// terminal streams a live, read-only view of everything it does.
//
// Usage:
//
//	# declare a provider (base_url, api_key), a default_provider and agent.model
//	zot config
//
//	# write an order in your editor, then run it
//	zot new
//
//	# run the order you wrote
//	zot .zot/orders/1758300000.md
//
//	# every run is logged, appended to .zot/orders/1758300000.jsonl
//	jq . .zot/orders/1758300000.jsonl
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/tools"
	"github.com/openzot/openzot/internal/tui"
)

// Seams for tests, which have no terminal: production always uses the real
// terminal check and the real full-screen viewer.
var (
	isTerminal = tui.IsInteractive
	runViewer  = tui.Run

	// execute is the engine entry point - runTask everywhere in production,
	// replaced by tests so no provider is ever reached.
	execute = runTask
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zot: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	// `zot config` opens the config file in $EDITOR, seeding it from the embedded
	// template on first run. `zot config path` prints its location.
	if len(os.Args) > 1 && os.Args[1] == "config" {
		if len(os.Args) > 2 && os.Args[2] == "path" {
			fmt.Println(config.DefaultConfigPath())
			return nil
		}
		return editConfig()
	}

	// `zot new` scaffolds a work order. The two-step shape is deliberate: the
	// pause between writing the order and running it is where acceptance
	// criteria get written, and it is what keeps zot from feeling like a
	// prompt box.
	if len(os.Args) > 1 && os.Args[1] == "new" {
		return newOrder(os.Args[2:], os.Stdout)
	}

	configPath := pflag.String("config", "", "path to zot config (default: "+config.DefaultConfigPath()+", optional)")
	dir := pflag.String("dir", ".", "working directory the agent reads, writes and runs commands in")
	pflag.Usage = usage
	pflag.Parse()

	// The run is shown in the full-screen viewer and nowhere else, so with no
	// terminal there is nothing to show it in: say so before any order is read
	// or any provider is touched.
	if !isTerminal() {
		return errors.New("zot runs in a terminal: stdout is not one")
	}

	// Every run leaves a log in the project it works on, beside its orders in
	// .zot/. Resolved to an absolute path while the original working directory is
	// still current, so a relative --dir means what the user typed rather than
	// what it happens to mean after the chdir below.
	logs := order.OrdersDir(*dir)
	if abs, err := filepath.Abs(logs); err == nil {
		logs = abs
	}

	// The order is loaded while the original working directory is still current,
	// because its path means what the user typed, not what it happens to mean
	// after the chdir below. A bad order fails the run here, before any provider
	// is touched.
	o, err := loadOrder(pflag.Args())
	if err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	// Resolve the config directory (source of any global AGENTS.md) while
	// the original working directory is still current, so a relative --config
	// resolves correctly before the chdir below.
	configDir := config.ConfigDir(*configPath)
	if abs, err := filepath.Abs(configDir); err == nil {
		configDir = abs
	}

	// Set the default working directory before the agent starts. This is not a
	// filesystem sandbox: absolute paths and shell commands retain the process's
	// host permissions.
	if err := os.Chdir(*dir); err != nil {
		return fmt.Errorf("cannot enter --dir %q: %w", *dir, err)
	}

	// Fold in AGENTS.md from the config directory, then the working directory
	// (project-level context wins / appends last).
	workDir, _ := os.Getwd()
	loadProjectContext(&cfg, configDir, workDir)

	// Skills are read once, here, into memory: the run offers them through the
	// skills tool and never touches the folder again. Loaded after the chdir so a
	// relative skills_dir means the project being worked on.
	if err := loadSkills(&cfg); err != nil {
		return err
	}

	// A signal cancels the run rather than killing the process outright, so the
	// engine records its aborted outcome - and, mid-failure, dumps the exchange -
	// before exiting. Without this a `kill` (or a supervisor stopping the
	// process) left the session with no ending at all. SIGKILL is uncatchable;
	// the incrementally-written conversation is the answer there.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The run is a fresh conversation with its own session log, whatever ran
	// before: nothing of an earlier run of the same order is read, continued or
	// skipped.
	return execute(ctx, cfg, o, orderOptions(logs, o))
}

// orderOptions is how an order is run: its log goes in logs, named after it, and
// the viewer calls it by its title, or by its file name.
func orderOptions(logs string, o order.Order) runOptions {
	return runOptions{
		SessionPath: filepath.Join(logs, sessionFile(o.Path)),
		Title:       o.DisplayTitle(),
	}
}

// sessionFile names an order's log: the order's own name with .jsonl for its
// extension, so the record of a task is the file beside the task. One file per
// task, whatever the number of runs - each appends to it.
func sessionFile(orderPath string) string {
	base := filepath.Base(orderPath)

	return strings.TrimSuffix(base, filepath.Ext(base)) + ".jsonl"
}

// loadOrder loads the one order this invocation is about. It explains itself
// rather than failing silently when there is none or more than one: zot runs a
// single order per invocation, and running several is a shell loop away.
func loadOrder(args []string) (order.Order, error) {
	switch len(args) {
	case 0:
		usage()

		return order.Order{}, errors.New("no order given (write one with `zot new`)")
	case 1:
	default:
		return order.Order{}, fmt.Errorf("zot runs one order per invocation; %d were named - run them one at a time", len(args))
	}

	path := args[0]

	loaded, err := order.Load(path)
	if err != nil {
		// The retraining moment: someone typed prose where an order file goes. The
		// error has to teach the new shape, not just report a missing file.
		if _, statErr := os.Stat(path); statErr != nil && strings.ContainsAny(path, " \t") {
			return order.Order{}, fmt.Errorf("work orders are files, not prose - write the order first:\n\n  zot new")
		}

		return order.Order{}, err
	}

	// the run chdirs into --dir, so the order's path must survive the move
	if abs, absErr := filepath.Abs(loaded.Path); absErr == nil {
		loaded.Path = abs
	}

	return loaded, nil
}

// newOrder creates a blank work order under ./.zot/orders - or under
// <dir>/.zot/orders when --dir names another working directory - and opens it
// in the editor, the way `zot config` opens the config.
//
// It takes no prose. The objective, the acceptance criteria and the constraints
// are written where they can be reviewed, in the file, not squeezed onto a
// command line. The file is named for the moment it was made, so there is
// nothing to invent and the orders sort in the order they were written.
//
// --dir exists because the order is written for a project the invoker may not
// be standing in.
func newOrder(args []string, out io.Writer) error {
	set := pflag.NewFlagSet("new", pflag.ContinueOnError)

	dir := set.String("dir", ".", "project the order is for: it is created under <dir>/"+order.BookDir+"/orders")

	if err := set.Parse(args); err != nil {
		return err
	}

	if set.NArg() > 0 {
		return fmt.Errorf("zot new takes no arguments: it opens a blank order in your editor - write the objective there")
	}

	path, err := order.Create(order.OrdersDir(*dir), time.Now())
	if err != nil {
		return err
	}

	// The file stays if the editor fails, so an editor that is missing does not
	// cost the operator the order they meant to write.
	if err := openInEditor(path); err != nil {
		return err
	}

	// An order left exactly as it was made is not an order, and a blank one lying
	// in .zot/orders would only fail when someone ran it. Nothing was written, so
	// nothing is kept.
	if written, err := os.ReadFile(path); err == nil && string(written) == order.Blank() {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the unedited order: %w", err)
		}

		fmt.Fprintln(out, "nothing written, so no order was created")

		return nil
	}

	fmt.Fprintf(out, "wrote %s\n\nrun it with:\n\n  zot %s\n", path, path)

	return nil
}

// editConfig ensures the config file exists - seeding it from the embedded
// template on first run - and opens it in the user's editor. This is the setup
// path: configure the provider, model and key by editing the file.
func editConfig() error {
	path := config.DefaultConfigPath()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, configs.ExampleConfigYAML, 0o600); err != nil {
			return fmt.Errorf("write config template: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Created %s from the template.\n", path)
	}

	return openInEditor(path)
}

// openInEditor opens a file in the user's editor and waits for it to close:
// $VISUAL, then $EDITOR, then the first of nano, vi and vim that is installed.
// With none of them it prints the path and says so, since the file itself is
// already in place.
func openInEditor(path string) error {
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		for _, candidate := range []string{"nano", "vi", "vim"} {
			if _, err := exec.LookPath(candidate); err == nil {
				editor = candidate

				break
			}
		}
	}

	if editor == "" {
		fmt.Println(path)

		return fmt.Errorf("no editor found; set $EDITOR (the file is at the path above)")
	}

	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd.Run()
}

func firstNonEmpty(values ...string) string {

	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func usage() {
	fmt.Fprintln(os.Stderr, `zot - an automated software factory powered by an autonomous coding harness

zot takes work orders, not prompts. A work order is one file: a front matter
block with the durable objective, the acceptance criteria that define "done",
and the constraints the work must hold to, then the system prompt itself - a Go
template that reads that block, so the order says what to do and how the agent
works. Each order is one autonomous run.

Usage:
  zot [flags] <order.md>
  zot new [--dir <dir>]
  zot config

Examples:
  zot new
  zot .zot/orders/1758300000.md
  zot --dir ./scratch .zot/orders/1758300000.md

zot new files an order under .zot/orders in the project, named for the moment it
was made, and opens it in your editor; you then run it by naming it. An order
can live anywhere - running one needs no .zot directory at all - and zot runs
one order per invocation: to run several, run zot once for each.

Every run starts from zero. Nothing of an earlier run of the same order is
continued or skipped, so running an order again is running it fresh.

Every run is recorded. The log is one file per order, .zot/orders/<name>.jsonl
in the project being worked on (--dir), appended to line by line as the run
goes: a meta line, a line for each message and event, and the outcome. Running
the order again adds a new run to the same file. Nothing in zot reads it back;
it is a record for you, with cat and jq.

Commands:
  new        create a work order under ./.zot/orders - under <dir>/.zot/orders
             with --dir - and open it in $EDITOR, the way zot config does. The file holds the full default prompt
             and a blank objective: write the objective, and change the prompt
             if you want the agent to work differently. It takes no prose
  config     edit the config file in $EDITOR (creates it on first run)

Flags:`)
	pflag.PrintDefaults()
}

// The file zot looks for under each context directory.
const agentFile = "AGENTS.md"

// taskKickoff is the user message that starts a run. The objective is in the
// instructions; this only has to get the agent moving.
const taskKickoff = "Begin working on your task. Start by calling the tasks tool to list the work, then carry it through to completion."

// loadProjectContext reads the instructions found on disk under the given
// directories, searched in order (typically the config directory first, then the
// working directory), into cfg.ProjectContext:
//
//   - <dir>/AGENTS.md
//
// Missing files are ignored, and duplicate directories are searched once. An
// order's prompt decides whether and where to use them, as .Project.
func loadProjectContext(cfg *config.Config, dirs ...string) {
	seen := map[string]bool{}

	var found []string

	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}

		seen[dir] = true

		if data, err := os.ReadFile(filepath.Join(dir, agentFile)); err == nil {
			if text := strings.TrimSpace(string(data)); text != "" {
				found = append(found, text)
			}
		}
	}

	cfg.ProjectContext = strings.Join(found, "\n\n---\n\n")
}

// loadSkills reads the skills folder named by skills_dir into cfg.Skills. An
// unset skills_dir means no skills; a set one that cannot be read is an error,
// since the config asked for skills the run would otherwise silently lack.
func loadSkills(cfg *config.Config) error {
	dir := strings.TrimSpace(cfg.SkillsDir)
	if dir == "" {
		return nil
	}

	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("skills_dir %q: %w", cfg.SkillsDir, err)
		}

		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}

	skills, err := tools.LoadSkills(dir)
	if err != nil {
		return fmt.Errorf("skills_dir: %w", err)
	}

	cfg.Skills = skills

	return nil
}

// runOptions configures a run beyond the configuration itself.
type runOptions struct {
	// SessionPath is the log this run is appended to: one file per task, so a
	// run of the same task again adds to it. Empty disables recording.
	SessionPath string

	// Title is a short label for the work, shown in the viewer instead of the
	// task text. A work order's title, or one derived from its file name;
	// empty falls back to the task. It is presentation only and never reaches
	// the model - the objective is the contract, a title is a label.
	Title string
}

// orderEnv is what an order's prompt can know about the run beyond the order: the
// tools it really has, where it is working, and what it is talking to.
func orderEnv(cfg config.Config, client *loop.Client, opts loop.Options, workdir string) order.Env {
	env := order.Env{
		Workdir:  workdir,
		Date:     time.Now().Format("2006-01-02"),
		Model:    client.Config().Model,
		Provider: cfg.DefaultProvider,
		Project:  cfg.ProjectContext,
	}

	for _, tool := range opts.Tools {
		info := tool.Info()

		env.Tools = append(env.Tools, order.Tool{Name: info.Name, Description: info.Description})
	}

	return env
}

// runTask executes one autonomous coding task, rendering the agent's activity in
// the read-only TUI. The agent's file and shell tools operate on the current
// working directory, so the caller chdirs into the target project first. It
// blocks until the user quits the viewer or the run errors.
func runTask(ctx context.Context, cfg config.Config, o order.Order, options runOptions) error {
	config.ScrubProviderSecrets(cfg)

	client, opts, err := resolve(cfg)
	if err != nil {
		return err
	}

	workdir, _ := os.Getwd()

	// The order is the system prompt: its objective, criteria and constraints go
	// in it, where they survive trimming however long the run grows, and the
	// opening user message only has to get the agent moving. It is rendered here,
	// once the provider secrets are out of the environment, so nothing it reads
	// can be one of them.
	//
	// @note there is deliberately no way to open a run with a prompt of the
	// caller's own. zot takes a work order, not a conversation; anything worth
	// saying to the agent belongs in the order, where it is durable.
	prompt, err := o.Render(orderEnv(cfg, client, opts, workdir))
	if err != nil {
		return fmt.Errorf("order %s: %w", firstNonEmpty(o.Path, "(unsaved)"), err)
	}

	opts.Client = client
	opts.Instructions = prompt
	opts.Messages = []loop.Message{{Type: loop.TypeUser, Text: taskKickoff}}

	task := o.Objective

	var (
		sessionPath string
		recorder    *session.Recorder
	)

	if options.SessionPath != "" {
		meta := session.Meta{
			Task:     task,
			Model:    client.Config().Model,
			Provider: cfg.DefaultProvider,
			Workdir:  workdir,
		}

		writer, err := session.Open(options.SessionPath, meta)

		// @note a log that cannot be opened is reported but not fatal: the run
		// is the point, and refusing to work because a directory is read-only
		// would be a worse failure than losing the record of it.
		if err != nil {
			fmt.Fprintf(os.Stderr, "zot: session log unavailable: %v\n", err)
		} else {
			defer writer.Close()

			sessionPath = writer.Path()

			// The seed is recorded before the run starts so a session that dies in
			// its first turn still says what it was asked to do.
			recorder = session.NewRecorder(writer)
			recorder.Conversation(opts.Messages)

			opts.OnConversation = recorder.Conversation
			opts.OnEvent = recorder.Event
		}
	}

	meta := viewerMeta(cfg, task, workdir, opts)
	meta.Title = options.Title

	result, err := runViewer(ctx, meta, opts)

	// A run that never began, or was abandoned still going, has no ending to write
	// down or to report.
	if result.Reason != "" {
		recorder.Result(result)

		printDigest(os.Stderr, sessionPath, result)
	}

	return err
}

// printDigest writes the end-of-run digest: the outcome, what the run spent,
// and - when the run was recorded - the session log it was appended to.
func printDigest(w io.Writer, sessionPath string, result loop.Result) {
	digest := tui.Digest{
		Status:       tui.DigestStatus(string(result.Reason), result.ExitCode()),
		Session:      sessionPath,
		Iterations:   result.Budget.Iterations,
		Calls:        result.Budget.Calls,
		InputTokens:  result.Budget.InputTokens,
		OutputTokens: result.Budget.OutputTokens,
		Message:      result.Message,
	}

	fmt.Fprintf(w, "\n%s", tui.RenderDigest(digest))
}

// viewerMeta describes the run to the viewer.
//
// The budgets it carries are the ones the run was resolved with, not the raw
// configuration: a per-model max_iterations lowers the limit the engine
// enforces, and a meta bar counting up to a number the run will never reach is
// worse than no number at all.
func viewerMeta(cfg config.Config, task, workdir string, opts loop.Options) tui.Meta {
	// Show the iteration progress denominator only for a real user-set limit -
	// the default is a 1,000,000 backstop, which is not a budget worth displaying.
	iterLimit := 0
	if opts.MaxIterations != config.Defaults().Agent.MaxIterations {
		iterLimit = opts.MaxIterations
	}

	return tui.Meta{
		Task:          task,
		Model:         cfg.Agent.Model,
		Provider:      cfg.DefaultProvider,
		Workdir:       workdir,
		MaxScrollback: cfg.UI.Scrollback,
		MaxIterations: iterLimit,
		MaxDuration:   opts.MaxDuration,
	}
}

// resolve turns a configuration into a provider client and the agent options a
// run uses. The returned options carry no messages; callers supply those.
func resolve(cfg config.Config) (*loop.Client, loop.Options, error) {
	var empty loop.Options

	if cfg.DefaultProvider == "" {
		return nil, empty, errors.New(
			"no provider selected: declare one under providers: in the config and name it with default_provider")
	}

	providerConfig, ok := cfg.Providers[cfg.DefaultProvider]
	if !ok {
		return nil, empty, fmt.Errorf(
			"provider %q is not configured (declare it under providers: with a base_url and api_key)", cfg.DefaultProvider)
	}

	// Resolve the model against the provider's custom model definitions. A custom
	// entry's settings take priority over the run defaults.
	model := cfg.Agent.Model
	maxIterations := cfg.Agent.MaxIterations
	credential := config.ProviderCredential(providerConfig)

	// Every model is declared, with its own context window. Validate says so at
	// load; the same rule holds here because a run with no window has nothing to
	// decide how much of a conversation to keep.
	mc, ok := providerConfig.Models[model]
	if !ok || mc.Context <= 0 {
		return nil, empty, fmt.Errorf(
			"model %q needs a context window: list it under providers.%s.models with context set",
			model, cfg.DefaultProvider)
	}

	if mc.Model != "" {
		model = mc.Model
	}

	if mc.MaxIterations > 0 {
		maxIterations = mc.MaxIterations
	}

	if mc.APIKey != "" {
		credential = mc.APIKey
	}

	contextWindow := mc.Context
	contentArray := mc.ContentArray

	client, err := loop.NewClient(loop.ClientConfig{
		Provider: cfg.DefaultProvider,
		Model:    model,
		APIKey:   credential,
		BaseURL:  providerConfig.BaseURL,

		ContentArray:    contentArray,
		ReasoningEffort: mc.ReasoningEffort,
		ExtraBody:       mc.ExtraBody,
	})
	if err != nil {
		return nil, empty, fmt.Errorf("provider %q: %w", cfg.DefaultProvider, err)
	}

	// max_time was validated at load, so a parse error here would be a bug; treat
	// it as unbounded rather than failing a run that already passed validation.
	maxDuration, _ := cfg.Agent.MaxDuration()

	opts := loop.Options{
		Tools: tools.DefaultToolsWith(cfg.Agent.MaxToolOutput, cfg.Skills),

		// shell acts on the machine, so a command the model did not finish
		// writing is refused rather than repaired into one that runs
		Unrepaired: []string{tools.ShellTool},

		MaxIterations:    maxIterations,
		MaxSettles:       cfg.Agent.MaxSettles,
		MaxCalls:         cfg.Agent.MaxCalls,
		MaxContinuations: cfg.Agent.MaxContinuations,
		MaxRecoveries:    cfg.Agent.MaxRecoveries,
		MaxCycles:        cfg.Agent.MaxCycles,
		MaxEmpties:       cfg.Agent.MaxEmpties,
		MaxDuration:      maxDuration,
		ContextWindow:    contextWindow,
	}

	// MaxTokens is a pointer so that "unset" (provider decides) is distinct from
	// a deliberate zero; the config uses a positive value to mean "cap here".
	if cfg.Agent.MaxTokens > 0 {
		limit := cfg.Agent.MaxTokens
		opts.MaxTokens = &limit
	}

	return client, opts, nil
}
