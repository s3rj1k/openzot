// Command zot is an automated software factory you watch, not drive.
//
// zot takes work orders, not prompts. A work order is a small YAML file - the
// durable objective, the acceptance criteria that define "done", the
// constraints the work must hold to - and each order becomes one autonomous
// run: the agent reads files, edits them, and runs shell commands on its own
// while the terminal streams a live, read-only view of everything it does.
//
// Usage:
//
//	# declare a provider (base_url, api_key), a default_provider and agent.model
//	zot config
//
//	# write an order in your editor, then run it - orders live under .zot/
//	zot new
//	zot
//
//	# a bare zot runs the whole book, in filename order; naming orders runs
//	# exactly those
//	zot .zot/orders/1758300000.yaml
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
	"github.com/openzot/openzot/internal/agent"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/tui"
)

// Seams for tests, which have no terminal: production always uses the real
// terminal check and the real full-screen viewer.
var (
	isTerminal = tui.IsInteractive
	runViewer  = tui.Run
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
	ordersFlag := pflag.String("orders-dir", "", "where this project's orders live, run by a bare `zot` (default: <dir>/"+order.BookDir+"/orders)")
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

	// The other half of the book: where this project's own orders live. It is
	// what a bare `zot` runs, so it is resolved here too - before the chdir,
	// because a relative --orders-dir means what was typed.
	ordersRoot := *ordersFlag
	if ordersRoot == "" {
		ordersRoot = order.OrdersDir(*dir)
	}

	if abs, err := filepath.Abs(ordersRoot); err == nil {
		ordersRoot = abs
	}

	// Orders are loaded - all of them, so a bad batch fails before any run
	// starts - while the original working directory is still current, because
	// their paths mean what the user typed, not what they happen to mean after
	// the chdir below.
	orders, err := resolveOrders(pflag.Args(), ordersRoot)
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

	// Each order is its own run: a fresh conversation and its own session log,
	// whatever ran before. The batch stops at the first order that does not
	// end in success, because later orders usually assume the earlier ones
	// landed - running order three against the wreckage of order two produces
	// confident garbage.
	runs := oneRun{
		ctx:  ctx,
		cfg:  cfg,
		logs: logs,
		run:  runTask,
	}

	for i, o := range orders {
		if len(orders) > 1 {
			fmt.Fprintf(os.Stderr, "zot: order %d/%d: %s\n", i+1, len(orders), o.Path)
		}

		if err := runs.executeAt(o, i < len(orders)-1, i+1, len(orders)); err != nil {
			// a deliberate stop - q or Ctrl-C - is the operator's decision,
			// not an order failing; report it as what it is
			if errors.Is(err, tui.ErrCancelled) {
				return fmt.Errorf("batch stopped: %w", err)
			}

			if len(orders) > 1 {
				return fmt.Errorf("order %s stopped the batch: %w", o.Path, err)
			}

			return err
		}
	}

	return nil
}

// oneRun is everything a single order's run needs: a fresh conversation,
// recorded in the task's session log.
type oneRun struct {
	ctx context.Context
	cfg config.Config

	// logs is the folder session logs go in: one file per order, named after it.
	logs string

	// run is the engine entry point - runTask everywhere in production,
	// replaced by tests so no provider is ever reached.
	run func(context.Context, config.Config, string, runOptions) error
}

// sessionFile names an order's log: the order's own name with .jsonl for its
// extension, so the record of a task is the file beside the task. One file per
// task, whatever the number of runs - each appends to it.
func sessionFile(orderPath string) string {
	base := filepath.Base(orderPath)

	return strings.TrimSuffix(base, filepath.Ext(base)) + ".jsonl"
}

// executeAt runs one order as its own run. Every run starts from zero: nothing
// of an earlier run of the same order is read, continued or skipped. The
// order's position in a batch is shown by the viewer as "order 2/5", so a long
// queue reports how much of itself is left.
func (r oneRun) executeAt(o order.Order, quitOnDone bool, index, size int) error {
	options := runOptions{
		SessionPath: filepath.Join(r.logs, sessionFile(o.Path)),

		// what a person calls this order: its own title, or its file name
		Title: o.DisplayTitle(),

		BatchIndex: index,
		BatchSize:  size,

		// intermediate orders auto-advance: a held final screen would stall the
		// rest of the batch until a keypress nobody unattended will make. The
		// last order holds for review as usual.
		QuitOnDone: quitOnDone,
	}

	return r.run(r.ctx, r.cfg, o.Task(), options)
}

// resolveOrders loads the orders this invocation is about: the ones named on
// the command line, or - when none are - everything in the project's own orders
// directory. It explains itself rather than failing silently either way.
func resolveOrders(args []string, ordersRoot string) ([]order.Order, error) {
	// Nothing named: run the book, every order in it, in filename order. Naming
	// orders explicitly runs exactly those, and reads them from anywhere.
	if len(args) == 0 {
		found, err := listOrdersRoot(ordersRoot)
		if err != nil {
			return nil, err
		}

		fmt.Fprintf(os.Stderr, "zot: running %d order(s) from %s\n", len(found), ordersRoot)

		args = found
	}

	orders := make([]order.Order, 0, len(args))

	for _, path := range args {
		loaded, err := order.Load(path)
		if err != nil {
			// The retraining moment: someone typed prose where an order file
			// goes. The error has to teach the new shape, not just report a
			// missing file.
			if _, statErr := os.Stat(path); statErr != nil && strings.ContainsAny(path, " \t") {
				return nil, fmt.Errorf("work orders are files, not prose - write the order first:\n\n  zot new")
			}

			return nil, err
		}

		// the run chdirs into --dir, so the order's path must survive the move
		if abs, absErr := filepath.Abs(loaded.Path); absErr == nil {
			loaded.Path = abs
		}

		orders = append(orders, loaded)
	}

	return orders, nil
}

// listOrdersRoot lists the orders directory for a bare invocation, or explains
// what to do instead. An empty book is not an error state to decode - it is
// someone who has not written an order yet.
func listOrdersRoot(ordersRoot string) ([]string, error) {
	var found []string

	if ordersRoot != "" {
		var err error

		if found, err = order.List(ordersRoot); err != nil {
			return nil, err
		}
	}

	if len(found) == 0 {
		usage()

		where := "no orders directory is configured"
		if ordersRoot != "" {
			where = ordersRoot + " holds none"
		}

		return nil, fmt.Errorf("no order given, and %s (write one with `zot new`)", where)
	}

	return found, nil
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
// be standing in. --orders-dir files the order somewhere else again - a shared
// folder of briefs.
func newOrder(args []string, out io.Writer) error {
	set := pflag.NewFlagSet("new", pflag.ContinueOnError)

	dir := set.String("dir", ".", "project the order is for: it is created under <dir>/"+order.BookDir+"/orders")
	ordersFlag := set.String("orders-dir", "", "where to create the order (default: <dir>/"+order.BookDir+"/orders)")

	if err := set.Parse(args); err != nil {
		return err
	}

	if set.NArg() > 0 {
		return fmt.Errorf("zot new takes no arguments: it opens a blank order in your editor - write the objective there")
	}

	ordersDir := *ordersFlag
	if ordersDir == "" {
		ordersDir = order.OrdersDir(*dir)
	}

	path, err := order.Create(ordersDir, time.Now())
	if err != nil {
		return err
	}

	// The file stays if the editor fails, so an editor that is missing does not
	// cost the operator the order they meant to write.
	if err := openInEditor(path); err != nil {
		return err
	}

	// An order left exactly as it was made is not an order, and a blank one in
	// the book would fail every bare `zot` after it. Nothing was written, so
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

zot takes work orders, not prompts. A work order is a small YAML file: the
durable objective, the acceptance criteria that define "done", and the
constraints the work must hold to. Each order is one autonomous run.

Usage:
  zot [flags] [<order.yaml> ...]
  zot new [--dir <dir>] [--orders-dir <dir>]
  zot config

Examples:
  zot new
  zot
  zot .zot/orders/1758300000.yaml
  zot --dir ./scratch .zot/orders/*.yaml

The book: a project keeps its orders under .zot/orders in its root - written by
zot new, and named for the moment they were made. Bare zot runs that book:
every order in it, in filename order, so writing an order and typing zot is the
whole loop. Naming order files instead runs exactly those, read from any path
in any tree; running an order needs no book at all. --orders-dir moves the
book: it is where zot new files an order and where a bare zot looks for work.

Every run starts from zero. Nothing of an earlier run of the same order is
continued or skipped, so running an order again is running it fresh.

Every run is recorded. The log is one file per order, .zot/orders/<name>.jsonl
in the project being worked on (--dir), appended to line by line as the run
goes: a meta line, a line for each message and event, and the outcome. Running
the order again adds a new run to the same file. Nothing in zot reads it back;
it is a record for you, with cat and jq.

A batch runs each order as its own run, in sequence, and stops at the first
order that does not end in success.

Commands:
  new        create a blank work order under ./.zot/orders - under
             <dir>/.zot/orders with --dir, or anywhere with --orders-dir - and
             open it in $EDITOR, the way zot config does. It takes no prose:
             write the objective in the file
  config     edit the config file in $EDITOR (creates it on first run)

Flags:`)
	pflag.PrintDefaults()
}

// Names zot looks for under each context directory.
const (
	agentFile      = "AGENTS.md"
	projectContext = "# Project context"
)

// defaultInstructions is the system prompt handed to the agent when the
// configuration does not override it. It establishes the fully-autonomous,
// no-questions-asked contract: zot has no input channel, so the agent must
// never wait for the user.
//
// The opening and the "act, don't narrate" rule imitate the batch-mode prompt of
// the engine zot was derived from: a run is a non-interactive background session
// whose deliverable is the changed working tree, not prose, and which ends only
// by recording an outcome with a terminal tool.
//
// It is assembled rather than written out because its closing half is not the
// caller's to drop - see nonInteractiveContract.
const defaultInstructions = baseInstructions + "\n\n" + nonInteractiveContract

// baseInstructions is the overridable half: who the agent is, what it can call,
// and how it is expected to work. A configuration that sets its own
// instructions replaces this and nothing else.
const baseInstructions = `You are zot, a fully autonomous software engineering agent operating inside a real working directory on the user's machine.

This is a non-interactive session running in the background. No one is watching, and no questions or further guidance can be answered - you will receive NO further input. Complete the assigned task end to end on your own, using your tools.

Your tools:
- "tasks": list the tasks the work needs and keep each one's status current. Every call carries the whole list, so use it to lay the work out before you start and to revise it whenever your approach changes.
- "shell": your only way to act on the machine, so use it for everything. Read files with cat, head, tail, sed -n 'START,ENDp' and grep -n; list directories with ls and find; create and change files with heredocs, tee, sed -i, patch or a small script; run builds, tests, linters and any other non-interactive command. Never run interactive or long-lived commands.

Operating rules:
- Begin by calling "tasks" to list the concrete tasks the work needs, in the order you will do them.
- Look before you change. Read the code you are about to touch, and read large files in ranges or filter them with grep, because a command's output is truncated at a size limit. After you change anything, build and run the tests, and fix what you broke.
- Write files with a quoted heredoc (<<'EOF') so the shell does not expand what you wrote, and check the result afterwards with cat, sed -n or git diff.
- Keep "tasks" current: mark a task in_progress when you begin it and done when it is finished, and mark it blocked, with a note saying why, when it cannot go on.
- Act, do not narrate. The deliverable is the changed working tree, not an explanation of it; there is no reader to address. Do not pause to summarise, interpret, or analyse tool output - keep working, and use "tasks" for status.`

// nonInteractiveContract is the half no configuration may leave out. Every
// other prompt rule is a preference; this one is a fact about the machine the
// agent is running on. zot has no input channel at all - a run is a work order,
// a provider and a read-only viewer - so an agent that asks a question is not
// answered tersely, it is not answered at all: it waits until a guard kills the
// run, and everything it had not yet written is lost. That failure is silent
// and expensive, and it costs a whole run to discover, so the contract is
// re-attached to whatever instructions a run resolves to rather than left to
// whoever wrote them.
//
// It is written to stand alone, naming the terminal tools itself, because the
// custom instructions it may be appended to need not mention them at all.
const nonInteractiveContract = `## Non-interactive contract

Nothing you address to the user is delivered. There is no reader, no reply, and no approval on its way. A question you ask is discarded unheard, and a run that stops to wait for an answer waits until a guard kills it, losing the work it had not yet finished.

- Never stop to wait for input, approval, permission or confirmation. No one can grant what you asked for, so asking and waiting is the one certain way to fail the task.
- Never end your turn with a question, an offer, or a promise to continue once told to. Continue now instead.
- Where the task is ambiguous or underspecified, decide it the way a careful engineer would, act on the decision, and record the assumption in a task's note and again in your final summary. A stated assumption is reviewable afterwards; an unasked question is not.
- Only a terminal tool call ends the task: "success" with a summary when the objective is met, or "failure" with the reason when it genuinely cannot be. Uncertainty is not a reason to stop - it is a reason to choose, act, and say what you chose. Do not simply stop.`

// taskHeading introduces the task inside the instructions. The task lives in the
// system prompt rather than as a user message so it survives trimming: the
// oldest messages are dropped first to fit the window, so a user message can
// fall out of a long run, and an autonomous agent that forgets its own objective
// is the worst way for a run to fail. The instructions are never dropped and
// always ordered first.
const taskHeading = "\n\n## Your task\n\n"

// taskKickoff is the user message that starts a run. The objective is in the
// instructions; this only has to get the agent moving.
const taskKickoff = "Begin working on your task. Start by calling the tasks tool to list the work, then carry it through to completion."

// withNonInteractiveContract guarantees the no-questions contract reaches the
// model whatever the instructions say. Custom instructions replace the built-in
// prompt wholesale - that is what an override is for - but they cannot opt a run
// into an interactivity zot does not have. Instructions that already carry the
// contract (the defaults, or the defaults plus an AGENTS.md) are left untouched,
// so the common path is unchanged and the text is never repeated.
func withNonInteractiveContract(instructions string) string {
	if strings.Contains(instructions, nonInteractiveContract) {
		return instructions
	}

	return strings.TrimRight(instructions, "\n") + "\n\n" + nonInteractiveContract
}

// withTask appends the task to the instructions, or returns them unchanged when
// there is no task.
func withTask(instructions, task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return instructions
	}

	return instructions + taskHeading + task
}

// loadProjectContext augments cfg with on-disk context discovered under the
// given directories, searched in order (typically the config directory first,
// then the working directory):
//
//   - <dir>/AGENTS.md  - appended to the agent instructions
//
// Missing files are ignored, and duplicate directories are searched once.
// AGENTS.md content augments (never replaces) the base instructions.
func loadProjectContext(cfg *config.Config, dirs ...string) {
	seen := map[string]bool{}
	var search []string
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		search = append(search, d)
	}

	base := cfg.Agent.Instructions
	if base == "" {
		base = defaultInstructions
	}

	var instructions []string
	for _, d := range search {
		if data, err := os.ReadFile(filepath.Join(d, agentFile)); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				instructions = append(instructions, s)
			}
		}
	}

	if len(instructions) > 0 {
		cfg.Agent.Instructions = base + "\n\n" + projectContext + "\n\n" + strings.Join(instructions, "\n\n---\n\n")
	}
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

	skills, err := agent.LoadSkills(dir)
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

	// BatchIndex and BatchSize place this run in a batch - order 2 of 5 - so
	// the viewer can show how much of the queue is left. Zero for a run that is
	// not part of a batch.
	BatchIndex int
	BatchSize  int

	// Title is a short label for the work, shown in the viewer instead of the
	// task text. A work order's title, or one derived from its file name;
	// empty falls back to the task. It is presentation only and never reaches
	// the model - the objective is the contract, a title is a label.
	Title string

	// QuitOnDone closes the viewer as soon as the run ends instead of holding
	// the final screen. Set for the intermediate orders of a batch, where a
	// held screen would stall the orders behind it until a keypress nobody
	// unattended will make; the batch's last order still holds for review.
	QuitOnDone bool
}

// runTask executes one autonomous coding task, rendering the agent's activity in
// the read-only TUI. The agent's file and shell tools operate on the current
// working directory, so the caller chdirs into the target project first. It
// blocks until the user quits the viewer or the run errors.
func runTask(ctx context.Context, cfg config.Config, task string, options runOptions) error {
	config.ScrubProviderSecrets(cfg)

	client, opts, err := resolve(cfg, defaultInstructions)
	if err != nil {
		return err
	}

	// The task is the durable objective and goes into the system prompt; the
	// opening user message only has to get the agent moving. This is what keeps
	// the objective in context however long the run grows - see withTask.
	//
	// @note there is deliberately no way to open a run with a prompt of the
	// caller's own. zot takes a work order, not a conversation; anything worth
	// saying to the agent belongs in the order, where it is durable.
	opts.Instructions = withTask(opts.Instructions, task)

	opts.Text = []string{taskKickoff}

	workdir, _ := os.Getwd()

	// Captures the run's final totals so the end-of-run digest can report them;
	// the viewer's returned Outcome carries only the reason and message.
	summaryRec := &agent.SummaryRecorder{}
	opts.Recorder = summaryRec

	var sessionPath string

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
			opts.Recorder = agent.MultiRecorder(session.NewRecorder(writer), summaryRec)
		}
	}

	meta := viewerMeta(cfg, task, workdir, opts)
	meta.Title = options.Title
	meta.BatchIndex = options.BatchIndex
	meta.BatchSize = options.BatchSize
	meta.QuitOnDone = options.QuitOnDone

	outcome, err := runViewer(ctx, client, meta, opts)

	printDigest(os.Stderr, sessionPath, outcome, summaryRec.Summary)

	return err
}

// printDigest writes the end-of-run digest: the outcome, what the run spent,
// and - when the run was recorded - the session log it was appended to. Skipped
// entirely when there is nothing to say (a run that never produced a summary, e.g. a setup
// failure before the first turn).
func printDigest(w io.Writer, sessionPath string, outcome tui.Outcome, summary *agent.Summary) {
	if summary == nil {
		return
	}

	digest := tui.Digest{
		Status:       tui.DigestStatus(summary.Reason, summary.Code),
		Session:      sessionPath,
		Iterations:   summary.Iterations,
		Calls:        summary.Calls,
		InputTokens:  summary.InputTokens,
		OutputTokens: summary.OutputTokens,
		Message:      outcome.Message,
	}

	fmt.Fprintf(w, "\n%s", tui.RenderDigest(digest))
}

// viewerMeta describes the run to the viewer.
//
// The budgets it carries are the ones the run was resolved with, not the raw
// configuration: a per-model max_iterations lowers the limit the engine
// enforces, and a meta bar counting up to a number the run will never reach is
// worse than no number at all.
func viewerMeta(cfg config.Config, task, workdir string, opts agent.ExecuteWithToolsOptions) tui.Meta {
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
		MaxCalls:      opts.MaxCalls,
		MaxDuration:   opts.MaxDuration,
		Stats:         cfg.UI.Stats,
	}
}

// resolve turns a configuration into a provider client and the agent options a
// run uses. The returned options carry no messages; callers supply those.
func resolve(cfg config.Config, defaultInstructions string) (*loop.Client, agent.ExecuteWithToolsOptions, error) {
	var empty agent.ExecuteWithToolsOptions

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

	instructions := cfg.Agent.Instructions
	if instructions == "" {
		instructions = defaultInstructions
	}

	instructions = withNonInteractiveContract(instructions)

	client, err := loop.NewClient(loop.ClientConfig{
		Provider: cfg.DefaultProvider,
		Model:    model,
		APIKey:   credential,
		BaseURL:  providerConfig.BaseURL,

		ContentArray: contentArray,
	})
	if err != nil {
		return nil, empty, fmt.Errorf("provider %q: %w", cfg.DefaultProvider, err)
	}

	// max_time was validated at load, so a parse error here would be a bug; treat
	// it as unbounded rather than failing a run that already passed validation.
	maxDuration, _ := cfg.Agent.MaxDuration()

	opts := agent.ExecuteWithToolsOptions{
		Instructions: instructions,
		Tools:        agent.DefaultToolsWith(cfg.Agent.MaxToolOutput, cfg.Skills),

		// shell acts on the machine, so a command the model did not finish
		// writing is refused rather than repaired into one that runs
		Unrepaired: []string{agent.ShellTool},

		MaxIterations:    maxIterations,
		MaxSettles:       cfg.Agent.MaxSettles,
		MaxCalls:         cfg.Agent.MaxCalls,
		MaxContinuations: cfg.Agent.MaxContinuations,
		MaxRecoveries:    cfg.Agent.MaxRecoveries,
		MaxCycles:        cfg.Agent.MaxCycles,
		MaxEmpties:       cfg.Agent.MaxEmpties,
		MaxDuration:      maxDuration,
		LimitCheckpoints: cfg.Agent.LimitCheckpoints,
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
