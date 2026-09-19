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
//	# every run is logged; list them, or export one
//	zot sessions
//	zot sessions export last --out ./trajectories
package main

import (
	"context"
	"encoding/json"
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
	"unicode/utf8"

	"github.com/spf13/pflag"

	"github.com/openzot/openzot"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/tui"
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

	// `zot sessions` lists what previous runs left behind. Its own subcommand
	// rather than a flag because it takes no order and produces no run.
	// `zot sessions export` renders sessions for use outside zot.
	if len(os.Args) > 1 && os.Args[1] == "sessions" {
		if len(os.Args) > 2 && os.Args[2] == "export" {
			return exportSessions(os.Args[3:], os.Stdout, os.Stderr)
		}

		return listSessions(os.Args[2:])
	}

	// `zot new` scaffolds a work order. The two-step shape is deliberate: the
	// pause between writing the order and running it is where acceptance
	// criteria get written, and it is what keeps zot from feeling like a
	// prompt box.
	if len(os.Args) > 1 && os.Args[1] == "new" {
		return newOrder(os.Args[2:], os.Stdout)
	}

	configPath := pflag.String("config", "", "path to zot config (default: "+config.DefaultConfigPath()+", optional)")
	provider := pflag.String("provider", "", "provider to run against, by the name it is given under providers: in the config (default: default_provider)")
	model := pflag.String("model", "", "override the model name (default: agent.model from the config)")
	dir := pflag.String("dir", ".", "working directory the agent reads, writes and runs commands in")
	maxIter := pflag.Int("max-iterations", 0, "override the safety cap on agent iterations")
	diffFlag := pflag.Bool("diff", false, "show a syntax-highlighted diff panel under each edit/write")
	plainFlag := pflag.Bool("plain", false, "stream unstyled output instead of the full-screen UI (auto-enabled when not a TTY)")
	colorFlag := pflag.String("color", "", "colorize non-interactive output: auto, always, or never")
	sessionDir := pflag.String("session-dir", "", "where session logs are written (default: "+config.DefaultSessionDir()+")")
	noSession := pflag.Bool("no-session", false, "do not record a session log for this run")
	ordersFlag := pflag.String("orders-dir", "", "where this project's orders live, run by a bare `zot` (default: <dir>/"+order.BookDir+"/orders)")
	watchFlag := pflag.Bool("watch", false, "stay up and run work orders as they arrive, instead of running once and exiting: bare --watch watches this project's orders directory, or name a folder or glob to watch instead")
	pflag.Usage = usage
	pflag.Parse()

	sessions := *sessionDir
	if sessions == "" {
		sessions = config.DefaultSessionDir()
	}

	// Resolved to an absolute path while the original working directory is still
	// current, so a relative --session-dir means what the user typed rather than
	// what it happens to mean after the chdir below.
	if abs, err := filepath.Abs(sessions); err == nil {
		sessions = abs
	}

	// The other half of the book: where this project's own orders live. It is
	// what a bare `zot` runs and what a bare `--watch` watches, so it is
	// resolved here too - before the chdir, because a relative --orders-dir
	// means what was typed.
	ordersRoot := *ordersFlag
	if ordersRoot == "" {
		ordersRoot = order.OrdersDir(*dir)
	}

	if abs, err := filepath.Abs(ordersRoot); err == nil {
		ordersRoot = abs
	}

	// The watch target is resolved like an order path: against the invoking
	// directory, while it is still current, so `zot --dir proj --watch orders`
	// watches the folder that was named rather than one that happens to exist
	// inside proj. Orders themselves are loaded the same way, in the other half.
	var (
		watchTarget string
		orders      []order.Order
	)

	if *watchFlag {
		// A watch has one target, not a batch of them: it is a place work
		// arrives at, and two places would interleave two streams of runs into
		// one screen. Bare --watch watches this project's own orders.
		if len(pflag.Args()) > 1 {
			return fmt.Errorf(
				"--watch takes one folder or glob to watch; %q names several",
				strings.Join(pflag.Args(), " "))
		}

		watchTarget = ordersRoot

		if len(pflag.Args()) == 1 {
			target, err := filepath.Abs(pflag.Args()[0])
			if err != nil {
				return fmt.Errorf("resolve --watch %q: %w", pflag.Args()[0], err)
			}

			watchTarget = target
		}
	} else {
		// Orders are loaded - all of them, so a bad batch fails before any run
		// starts - while the original working directory is still current,
		// because their paths mean what the user typed, not what they happen to
		// mean after the chdir below.
		loaded, err := resolveOrders(pflag.Args(), ordersRoot)
		if err != nil {
			return err
		}

		orders = loaded
	}

	cfg, err := zot.Load(*configPath)
	if err != nil {
		return err
	}

	passed := map[string]bool{}

	pflag.Visit(func(f *pflag.Flag) { passed[f.Name] = true })

	applyOverrides(&cfg, overrides{
		Provider:      *provider,
		Model:         *model,
		MaxIterations: *maxIter,
		Diff:          *diffFlag,
		Plain:         *plainFlag,
		Color:         *colorFlag,
		Passed:        passed,
	})

	if err := cfg.Validate(); err != nil {
		return err
	}

	// Resolve the config directory (source of any global AGENTS.md / skills) while
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

	// Fold in AGENTS.md and skills from the config directory, then the working
	// directory (project-level context wins / appends last).
	workDir, _ := os.Getwd()
	if err := zot.LoadProjectContext(&cfg, configDir, workDir); err != nil {
		return err
	}

	if *noSession {
		sessions = ""
	}

	// A signal cancels the run rather than killing the process outright, so the
	// engine records its aborted outcome - and, mid-failure, dumps the exchange -
	// before exiting. Without this a `kill` (or a supervisor stopping the
	// process) left the session with no ending at all. SIGKILL is uncatchable;
	// the incrementally-written conversation is the answer there.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Watch mode: stay up and run every order the target yields, as it arrives.
	// It shares the batch's per-order treatment (oneRun), so an order runs the
	// same whether it was named on the command line or dropped into the folder
	// after startup - and one failed order is one order's story, not the
	// watch's.

	if *watchFlag {
		return startWatch(ctx, watchTarget, newWatchRunner(ctx, cfg, sessions))
	}

	// Each order is its own run: a fresh conversation and its own session log,
	// whatever ran before. The batch stops at the first order that does not
	// end in success, because later orders usually assume the earlier ones
	// landed - running order three against the wreckage of order two produces
	// confident garbage.
	runs := oneRun{
		ctx:      ctx,
		cfg:      cfg,
		sessions: sessions,
		run:      zot.RunWith,
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

// oneRun is everything a single order's run needs. The batch loop and watch
// mode both go through execute, so an order runs identically however it was
// named: a fresh conversation and its own session log.
type oneRun struct {
	ctx      context.Context
	cfg      zot.Config
	sessions string

	// run is the engine entry point - zot.RunWith everywhere in production,
	// replaced by tests so no provider is ever reached.
	run func(context.Context, zot.Config, string, zot.RunOptions) error
}

// execute runs one order as its own run. Every run starts from zero: nothing of
// an earlier run of the same order is read, continued or skipped.
func (r oneRun) execute(o order.Order, quitOnDone bool) error {
	return r.executeAt(o, quitOnDone, 0, 0)
}

// executeAt is execute with the order's position in a batch, which the viewer
// shows as "order 2/5" so a long queue reports how much of itself is left.
func (r oneRun) executeAt(o order.Order, quitOnDone bool, index, size int) error {
	options := zot.RunOptions{
		SessionDir: r.sessions,

		// what a person calls this order: its own title, or its file name
		Title: o.DisplayTitle(),

		BatchIndex: index,
		BatchSize:  size,

		// intermediate orders auto-advance: a held final screen would stall the
		// rest of the batch until a keypress nobody unattended will make. The
		// last order holds for review as usual - watch mode holds none.
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
// folder of briefs, a drop box a watcher is pointed at.
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

// listSessions prints previous runs, newest first.
func listSessions(args []string) error {
	set := pflag.NewFlagSet("sessions", pflag.ContinueOnError)

	dir := set.String("session-dir", config.DefaultSessionDir(), "directory to list")

	if err := set.Parse(args); err != nil {
		return err
	}

	entries, err := session.List(*dir)
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Printf("no sessions in %s\n", *dir)

		return nil
	}

	for _, entry := range entries {
		status := "running/interrupted"
		if entry.Complete {
			status = entry.Reason
		}

		fmt.Printf("%-17s  %-20s  %s\n", entry.ID, status, oneLine(entry.Task, 60))
	}

	return nil
}

// exportSessions renders sessions as trajectories: the conversation in the
// chat shape the rest of the ecosystem reads, with the run's outcome beside it.
//
// To stdout it is JSON Lines, one trajectory per line. With --out it is a
// directory of `<id>.jsonl` files - the shape a dataset is built from, and one
// `cp -r` away from wherever it is going.
func exportSessions(args []string, stdout, stderr io.Writer) error {
	set := pflag.NewFlagSet("sessions export", pflag.ContinueOnError)
	set.SetOutput(stderr)

	dir := set.String("session-dir", config.DefaultSessionDir(), "directory the sessions are read from")
	out := set.String("out", "", "directory to write <id>.jsonl into (default: JSON Lines on stdout)")
	all := set.Bool("all", false, "export every session, instead of the ones named")

	set.Usage = func() {
		fmt.Fprintln(stderr, "usage: zot sessions export [flags] [session ...]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "A session is an id, a path, or \"last\" (the default).")
		fmt.Fprintln(stderr)
		set.PrintDefaults()
	}

	if err := set.Parse(args); err != nil {
		return err
	}

	entries, err := session.List(*dir)
	if err != nil {
		return err
	}

	var paths []string

	switch {
	case *all:
		// oldest first, so a directory of exports reads in the order it happened
		for i := len(entries) - 1; i >= 0; i-- {
			paths = append(paths, entries[i].Path)
		}

		if len(paths) == 0 {
			return fmt.Errorf("sessions export: no sessions in %s", *dir)
		}

	default:
		references := set.Args()
		if len(references) == 0 {
			references = []string{"last"}
		}

		for _, reference := range references {
			path, err := session.Resolve(*dir, reference)
			if err != nil {
				return err
			}

			paths = append(paths, path)
		}
	}

	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return fmt.Errorf("sessions export: %w", err)
		}
	}

	for _, path := range paths {
		last, err := session.Load(path)
		if err != nil {
			return fmt.Errorf("sessions export: read %s: %w", path, err)
		}

		trajectory, err := session.Export(last)
		if err != nil {
			return fmt.Errorf("sessions export: %s: %w", last.Meta.ID, err)
		}

		encoded, err := json.Marshal(trajectory)
		if err != nil {
			return fmt.Errorf("sessions export: encode %s: %w", last.Meta.ID, err)
		}

		if *out == "" {
			fmt.Fprintln(stdout, string(encoded))

			continue
		}

		target := filepath.Join(*out, trajectory.ID+".jsonl")

		if err := os.WriteFile(target, append(encoded, '\n'), 0o644); err != nil {
			return fmt.Errorf("sessions export: %w", err)
		}

		fmt.Fprintf(stderr, "%s  %d messages -> %s\n",
			trajectory.ID, len(trajectory.Messages), target)
	}

	return nil
}

// oneLine flattens a task to a single truncated line, so a multi-line brief does
// not turn the listing into a wall of text.
//
// The cap counts characters, not bytes: a brief written in CJK or carrying an
// emoji would otherwise be cut inside a rune and print as a replacement glyph,
// and would be truncated far earlier than the column it is given.
func oneLine(text string, width int) string {
	text = strings.Join(strings.Fields(text), " ")

	if utf8.RuneCountInString(text) > width {
		return string([]rune(text)[:width-1]) + "\u2026"
	}

	return text
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
		if err := os.WriteFile(path, zot.ExampleConfigYAML, 0o600); err != nil {
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

// overrides are the command-line values that take precedence over the config
// file and the environment.
type overrides struct {
	Provider      string
	Model         string
	MaxIterations int
	Diff          bool
	Plain         bool
	Color         string

	// Passed names the flags actually given, so a boolean can tell "false
	// because it was passed" from "false because it was never set". Without it
	// an unset --diff would silently turn off a diff the config had enabled.
	Passed map[string]bool
}

// applyOverrides layers command-line values over a loaded configuration.
func applyOverrides(cfg *zot.Config, o overrides) {
	if o.Provider != "" {
		cfg.DefaultProvider = o.Provider
	}

	if o.Model != "" {
		cfg.Agent.Model = o.Model
	}

	if o.MaxIterations > 0 {
		cfg.Agent.MaxIterations = o.MaxIterations
	}

	// A per-model max_iterations is applied when the run resolves, after this,
	// and would otherwise leave the config file beating the command line: the
	// engine would stop at the model's cap while the viewer counted up to the
	// flag's. An explicitly passed --max-iterations is the operator's last word,
	// so the model's own cap goes.
	if o.Passed["max-iterations"] {
		clearModelIterations(cfg)
	}

	if o.Passed["diff"] {
		cfg.UI.Diff = o.Diff
	}

	if o.Passed["plain"] {
		cfg.UI.Plain = o.Plain
	}
	if o.Color != "" {
		cfg.UI.Color = o.Color
	}
}

// clearModelIterations drops the per-model iteration cap for the model the run
// will actually use, so nothing is left to override the command line later. The
// provider and model have already been overridden by the time this is called, so
// it looks up the pair the run resolves to.
func clearModelIterations(cfg *zot.Config) {
	provider, ok := cfg.Providers[cfg.DefaultProvider]
	if !ok {
		return
	}

	model, ok := provider.Models[cfg.Agent.Model]
	if !ok {
		return
	}
	model.MaxIterations = 0

	provider.Models[cfg.Agent.Model] = model
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
  zot sessions
  zot --watch [<folder-or-glob>]

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

A batch runs each order as its own run, in sequence, and stops at the first
order that does not end in success.

Watch mode keeps zot up and runs work as it arrives instead of exiting when the
batch ends. Bare zot --watch watches the orders directory; name a folder
(zot --watch ~/inbox) or a glob (zot --watch "~/inbox/*.yaml") to watch that
instead. Every *.yaml that shows up - including orders already sitting there -
runs as its own run, one at a time; a failed order is reported and the watch
goes on; Ctrl-C stops watching.

Commands:
  new        create a blank work order under ./.zot/orders - under
             <dir>/.zot/orders with --dir, or anywhere with --orders-dir - and
             open it in $EDITOR, the way zot config does. It takes no prose:
             write the objective in the file
  config     edit the config file in $EDITOR (creates it on first run)
  sessions   list previous runs, newest first

Flags:`)
	pflag.PrintDefaults()
}
