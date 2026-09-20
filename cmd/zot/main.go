// Command zot is an automated software factory you watch, not drive.
//
// Zot takes work orders, not prompts. A work order is one file: a front matter
// block with the durable objective, the acceptance criteria that define "done"
// and the constraints the work must hold to, then the system prompt itself, a
// Go template that reads that block. Each order becomes one autonomous run: the
// agent reads files, edits them, and runs shell commands on its own while the
// terminal streams a live, read-only view of everything it does.
//
// Usage:
//
//	# declare the provider (base_url, api_key, models) and pick agent.model
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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/run"
	"github.com/openzot/openzot/internal/tui"
)

// Seams for tests, which have no terminal: production always uses the real
// terminal check and the real full-screen viewer.
var (
	isTerminal = tui.IsInteractive
	runViewer  = tui.Run

	// Execute is the engine entry point - run.Run everywhere in production,
	// replaced by tests so no provider is ever reached.
	execute = run.Run
)

func main() {
	if err := command(); err != nil {
		fmt.Fprintln(os.Stderr, "zot: "+err.Error())
		os.Exit(1)
	}
}

func command() error {
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
	project := run.LoadProjectContext(configDir, workDir)

	// Skills are read once, here, into memory: the run offers them through the
	// skills tool and never touches the folder again. Loaded after the chdir so a
	// relative skills_dir means the project being worked on.
	offered, err := run.LoadSkills(cfg.SkillsDir)
	if err != nil {
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
	options := orderOptions(logs, o)
	options.Project = project
	options.Skills = offered
	options.Viewer = runViewer

	return execute(ctx, &cfg, o, options)
}

// orderOptions is how an order is run: its log goes in logs, named after it, and
// the viewer calls it by its title, or by its file name.
func orderOptions(logs string, o order.Order) run.Options {
	// the log is the order's own name with .jsonl for its extension, so the record
	// of a task is the file beside the task: one file per task, whatever the number
	// of runs - each appends to it
	base := filepath.Base(o.Path)

	return run.Options{
		SessionPath: filepath.Join(logs, strings.TrimSuffix(base, filepath.Ext(base))+".jsonl"),
		Title:       o.DisplayTitle(),
	}
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
			return order.Order{}, errors.New("work orders are files, not prose - write the order first:\n\n  zot new")
		}

		return order.Order{}, err
	}

	// the run chdirs into --dir, so the order's path must survive the move
	if abs, absErr := filepath.Abs(loaded.Path); absErr == nil {
		loaded.Path = abs
	}

	return loaded, nil
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
