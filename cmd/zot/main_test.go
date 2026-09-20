package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/tools"
	"github.com/openzot/openzot/internal/tui"
)

// TestMain gives every test a stand-in for the terminal and the full-screen
// viewer, which need a real TTY: the stand-in runs the agent to its ending and
// prints what it said, so a test can assert on the run without a screen.
func TestMain(m *testing.M) {
	isTerminal = func() bool { return true }
	runViewer = headlessViewer

	os.Exit(m.Run())
}

// headlessViewer is tui.Run without the screen. It reports endings the way the
// viewer does: an error behind the run as itself, otherwise an agent-declared
// failure as an AgentExitError.
func headlessViewer(ctx context.Context, meta tui.Meta, opts loop.Options) (loop.Result, error) {
	engine, err := loop.New(opts)
	if err != nil {
		return loop.Result{}, err
	}

	fmt.Println(meta.Task)

	result := engine.Run(ctx, func(event loop.Event) {
		if event.Kind == loop.EventToken {
			fmt.Print(event.Text)
		}
	})

	fmt.Println(result.Message)

	switch {
	case result.Err != nil:
		return result, result.Err
	case result.ExitCode() != 0:
		return result, &tui.AgentExitError{Code: result.ExitCode(), Message: result.Message}
	}

	return result, nil
}

// With no terminal there is nothing to show a run in, so zot refuses before it
// reads an order or touches a provider.
func TestRunNeedsATerminal(t *testing.T) {
	original := isTerminal
	isTerminal = func() bool { return false }

	t.Cleanup(func() { isTerminal = original })

	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))

	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
agent:
  model: test-model
default_provider: local
providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, "--dir", t.TempDir(), orderFile(t, "a task"))

	err := run()
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("run = %v, want it to say zot needs a terminal", err)
	}

	if requests.Load() != 0 {
		t.Error("a run with no terminal must not reach the provider")
	}
}

func TestLoadOrderLoadsTheFile(t *testing.T) {
	path := orderFile(t, "build the parser")

	o, err := loadOrder([]string{path})
	if err != nil {
		t.Fatalf("loadOrder: %v", err)
	}

	if o.Objective != "build the parser" || o.Path != path {
		t.Errorf("order = %+v", o)
	}
}

// A broken order fails the run before a provider is touched.
func TestLoadOrderFailsUpFront(t *testing.T) {
	if _, err := loadOrder([]string{filepath.Join(t.TempDir(), "nope.md")}); err == nil {
		t.Error("a missing order must not load")
	}

	broken := filepath.Join(t.TempDir(), "broken.md")

	mustWrite(t, broken, "---\nobjective: x\n---\n{{ .Objectve }}")

	if _, err := loadOrder([]string{broken}); err == nil {
		t.Error("an order whose prompt names a field that does not exist must not load")
	}
}

// Someone typing prose where an order file goes is the retraining moment: the
// error has to teach the new shape, not just report a missing file.
func TestLoadOrderTeachesProseTypers(t *testing.T) {
	_, err := loadOrder([]string{"add a health endpoint"})
	if err == nil {
		t.Fatal("prose must not load")
	}

	if !strings.Contains(err.Error(), "zot new") {
		t.Errorf("the error should point at `zot new`: %v", err)
	}
}

// One order per invocation: none is told how to make one, several are told to
// run them one at a time.
func TestLoadOrderNeedsExactlyOne(t *testing.T) {
	quietStderr(t)

	_, err := loadOrder(nil)
	if err == nil || !strings.Contains(err.Error(), "zot new") {
		t.Errorf("no order: err = %v, want it to say how to write one", err)
	}

	_, err = loadOrder([]string{orderFile(t, "a"), orderFile(t, "b")})
	if err == nil || !strings.Contains(err.Error(), "one order per invocation") {
		t.Errorf("two orders: err = %v, want it to say zot runs one at a time", err)
	}
}

// withEditor makes $VISUAL a script that runs the given shell body against the
// file it is handed, so a test can play the part of someone writing an order.
func withEditor(t *testing.T, body string) {
	t.Helper()

	script := filepath.Join(t.TempDir(), "editor.sh")

	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("VISUAL", script)
	t.Setenv("EDITOR", "")
}

// `zot new` opens a blank order in the editor, named for the moment it was
// made. What the operator writes there is the order.
func TestNewOrderOpensABlankOrderInTheEditor(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: fix the typo\n---\nbody\n' > "$1"`)

	var out strings.Builder

	if err := newOrder(nil, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("orders written = %v, want the one", matches)
	}

	if name := filepath.Base(matches[0]); !regexp.MustCompile(`^\d+\.md$`).MatchString(name) {
		t.Errorf("name = %q, want a unix timestamp", name)
	}

	if !strings.Contains(out.String(), matches[0]) {
		t.Errorf("the output should say where the order went and how to run it:\n%s", out.String())
	}

	o, err := loadOrder([]string{matches[0]})
	if err != nil {
		t.Fatalf("the written order does not resolve: %v", err)
	}

	if o.Objective != "fix the typo" {
		t.Errorf("objective = %q", o.Objective)
	}

	// the book is one dotted directory: zot does not claim the generic
	// top-level names in the root of somebody else's project
	if _, err := os.Stat("orders"); err == nil {
		t.Errorf("a top-level orders/ was created; the book lives under %s", order.BookDir)
	}
}

// Prose has no place on the command line. Someone typing it out of habit is told
// where it goes, and nothing is created.
func TestNewOrderTakesNoProse(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: never\n---\nbody\n' > "$1"`)

	err := newOrder([]string{"fix", "the", "typo"}, io.Discard)
	if err == nil {
		t.Fatal("prose must be refused")
	}

	if !strings.Contains(err.Error(), "no arguments") {
		t.Errorf("the error should say zot new takes none: %v", err)
	}

	if _, statErr := os.Stat(order.BookDir); !os.IsNotExist(statErr) {
		t.Errorf("a refused invocation must create nothing: %v", statErr)
	}
}

// `zot new --dir` creates the order in another working directory, not the one
// the command was invoked from - the order belongs to the project it is for.
func TestNewOrderWithDirCreatesItInThatDirectory(t *testing.T) {
	invocation := t.TempDir()
	target := t.TempDir()

	t.Chdir(invocation)

	withEditor(t, `printf -- '---\nobjective: fix the typo\n---\nbody\n' > "$1"`)

	if err := newOrder([]string{"--dir", target}, io.Discard); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	if _, err := os.Stat(filepath.Join(invocation, order.BookDir)); !os.IsNotExist(err) {
		t.Errorf("the invoking directory must stay untouched: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(target, order.BookDir, "orders", "*.md"))
	if len(matches) != 1 {
		t.Errorf("orders in the target project = %v, want the one", matches)
	}
}

// An order closed without a word written is not an order, and a blank one left
// in the book would fail every bare `zot` after it. Nothing was written, so
// nothing is kept.
func TestNewOrderLeftUnchangedIsNotKept(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `true`)

	var out strings.Builder

	if err := newOrder(nil, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	if matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md")); len(matches) != 0 {
		t.Errorf("an unedited order was kept: %v", matches)
	}

	if !strings.Contains(out.String(), "no order was created") {
		t.Errorf("the output should say nothing was created:\n%s", out.String())
	}
}

// An editor that fails must not cost the operator the file: they may have
// written the order before it went wrong.
func TestNewOrderKeepsTheFileWhenTheEditorFails(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: half written\n---\nbody\n' > "$1"; exit 3`)

	if err := newOrder(nil, io.Discard); err == nil {
		t.Fatal("an editor that fails must be reported")
	}

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("orders = %v, want the file kept", matches)
	}
}

// With no editor to be found the order is still created, and the operator is
// told where it is, the way `zot config` does.
func TestNewOrderWithoutAnEditorSaysWhereTheFileIs(t *testing.T) {
	t.Chdir(t.TempDir())

	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", t.TempDir())

	err := newOrder(nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no editor found") {
		t.Fatalf("err = %v, want it to say no editor was found", err)
	}

	if matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md")); len(matches) != 1 {
		t.Errorf("orders = %v, want the blank order left to be edited", matches)
	}
}

func orderFile(t *testing.T, objective string) string {
	t.Helper()

	return orderFileIn(t, t.TempDir(), "order.md", objective)
}

// orderText is an order file with the given objective and the smallest prompt
// that uses it: the contract is not in it, because the run supplies that.
func orderText(objective string) string {
	return "---\nobjective: " + fmt.Sprintf("%q", objective) + "\n---\n{{ .Objective }}\n"
}

// testOrder is an order for a run that never was a file.
func testOrder(objective string) order.Order {
	return order.Order{Objective: objective, Body: "{{ .Objective }}"}
}

// orderFileIn writes an order with the given objective to dir/name.
func orderFileIn(t *testing.T, dir, name, objective string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(orderText(objective)), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// readLog decodes every line of a session log, failing on any line that is not
// a JSON record.
func readLog(t *testing.T, path string) []session.Record {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	var records []session.Record

	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var record session.Record

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not a JSON record: %v\n%s", i+1, err, line)
		}

		records = append(records, record)
	}

	return records
}

// quietStderr silences stderr for a test that deliberately triggers the usage
// block.
func quietStderr(t *testing.T) {
	t.Helper()

	original := os.Stderr

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	os.Stderr = devNull

	t.Cleanup(func() {
		os.Stderr = original

		devNull.Close()
	})
}

func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		values []string
		want   string
	}{
		{[]string{"", "second"}, "second"},
		{[]string{"first", "second"}, "first"},
		// whitespace is not a value - it is how an unset variable usually looks
		{[]string{"  ", "\t", "real"}, "real"},
		{[]string{"", ""}, ""},
		{nil, ""},
	}

	for _, test := range tests {
		if got := firstNonEmpty(test.values...); got != test.want {
			t.Errorf("firstNonEmpty(%q) = %q, want %q", test.values, got, test.want)
		}
	}
}

// The usage text is what a user sees when they get it wrong, so it has to name
// the things they can actually do - and nothing they cannot.
func TestUsageDescribesTheRealCommands(t *testing.T) {
	original := os.Stderr

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stderr = write

	usage()

	write.Close()

	os.Stderr = original

	var builder strings.Builder

	buffer := make([]byte, 4096)

	for {
		n, err := read.Read(buffer)

		builder.Write(buffer[:n])

		if err != nil {
			break
		}
	}

	text := builder.String()

	for _, want := range []string{"zot [flags] <order.md>", "zot new", "zot config", "--dir", ".jsonl"} {
		if !strings.Contains(text, want) {
			t.Errorf("usage does not mention %q:\n%s", want, text)
		}
	}

	// --dir belongs to both shapes: where a run works, and where `zot new`
	// scaffolds - someone standing outside the project needs it either way
	if n := strings.Count(text, "--dir"); n < 2 {
		t.Errorf("usage should document --dir for both running an order and `zot new` (%d mentions):\n%s", n, text)
	}

	// The book is a convention, so --help is where someone finds out where
	// their orders went.
	if !strings.Contains(text, order.BookDir+"/orders") {
		t.Errorf("usage does not say where zot new files an order:\n%s", text)
	}

	if strings.Contains(text, "--orders-dir") {
		t.Errorf("usage still mentions --orders-dir:\n%s", text)
	}

	// ACP is gone: zot runs unattended and has no protocol server
	if strings.Contains(strings.ToLower(text), "acp") {
		t.Errorf("usage still mentions acp:\n%s", text)
	}

	// nothing is resumed, skipped or recorded between runs, and the help must
	// not promise it
	for _, gone := range []string{"--resume", "--fresh", "--rerun", "--records-dir", "--draft", "ledger"} {
		if strings.Contains(text, gone) {
			t.Errorf("usage still mentions %q:\n%s", gone, text)
		}
	}
}

// The CLI uses pflag (GNU-style), so a flag may appear AFTER the positional
// order paths: `zot orders/a.md --dir proj` parses --dir as a flag and keeps
// the paths intact. The stdlib flag package stopped at the first non-flag,
// folding the flag into the positionals - this locks the behaviour that
// motivated the switch.
func TestFlagsAfterThePositionalOrdersAreParsed(t *testing.T) {
	set := pflag.NewFlagSet("zot", pflag.ContinueOnError)
	dir := set.String("dir", ".", "")

	if err := set.Parse([]string{"a.md", "b.md", "--dir", "proj"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if *dir != "proj" {
		t.Errorf("--dir given after the orders = %q, want it parsed as a flag", *dir)
	}

	if got := strings.Join(set.Args(), " "); got != "a.md b.md" {
		t.Errorf("positional orders = %q, want the paths before the flag", got)
	}
}

// Everything the config can say, the config alone says: a flag that duplicated a
// key would be a second place to look for what a run was told.
func TestConfigKeysAreNotFlags(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "missing.yaml"), orderFile(t, "a task"))

	_, _ = captureStderr(t, func() error { return run() })

	for _, name := range []string{"provider", "model", "max-iterations", "plain", "color", "orders-dir"} {
		if pflag.CommandLine.Lookup(name) != nil {
			t.Errorf("--%s is a flag, but the config already says it", name)
		}
	}

	for _, name := range []string{"config", "dir"} {
		if pflag.CommandLine.Lookup(name) == nil {
			t.Errorf("--%s should stay a flag: the config cannot say it", name)
		}
	}
}

// editConfig is the setup path: it must create the config from the template on
// first run, and say something useful when there is no editor to open it with.
func TestEditConfigSeedsTheTemplate(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ZOT_CONFIG", filepath.Join(dir, "nested", "config.yaml"))
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", dir) // no nano/vi/vim reachable

	err := editConfig()

	// with no editor available it must fail loudly rather than silently doing
	// nothing - but the file it would have opened must exist by then
	if err == nil {
		t.Fatal("expected an error when no editor is available")
	}

	if !strings.Contains(err.Error(), "editor") {
		t.Errorf("the error should mention the missing editor: %v", err)
	}

	if _, statErr := os.Stat(config.DefaultConfigPath()); statErr != nil {
		t.Errorf("the config should have been seeded from the template: %v", statErr)
	}
}

func TestEditConfigOpensTheConfiguredEditor(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")

	t.Setenv("ZOT_CONFIG", path)

	// a no-op "editor" that just succeeds
	t.Setenv("VISUAL", "true")

	if err := editConfig(); err != nil {
		t.Fatalf("editConfig: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}

	if len(content) == 0 {
		t.Error("the seeded config is empty")
	}
}

// withArgs runs a function with a fresh flag set and the given argv, so run()
// can be exercised the way the shell invokes it. run() chdirs into --dir, so the
// working directory is put back afterwards: a later test must not inherit a
// temp directory that is already gone.
func withArgs(t *testing.T, args ...string) {
	t.Helper()

	originalArgs := os.Args
	originalFlags := pflag.CommandLine
	originalDir, _ := os.Getwd()

	os.Args = append([]string{"zot"}, args...)
	pflag.CommandLine = pflag.NewFlagSet("zot", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)

	t.Cleanup(func() {
		os.Args = originalArgs
		pflag.CommandLine = originalFlags

		if originalDir != "" {
			_ = os.Chdir(originalDir)
		}
	})
}

func TestRunConfigPath(t *testing.T) {
	t.Setenv("ZOT_CONFIG", "/some/where/config.yaml")

	withArgs(t, "config", "path")

	output, err := captureStdout(t, run)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(output, "/some/where/config.yaml") {
		t.Errorf("output = %q, want the config path", output)
	}
}

func TestRunRequiresAnOrder(t *testing.T) {
	quietStderr(t)
	withArgs(t)

	if err := run(); err == nil {
		t.Error("running with no task must be an error")
	}
}

// The whole path: argv in, config resolved, provider called, transcript out.
func TestRunEndToEnd(t *testing.T) {
	// the run's log lands under --dir, which defaults to here: keep it out of the
	// source tree
	t.Chdir(t.TempDir())

	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		frames := [][]string{
			{`{"choices":[{"delta":{"content":"on it"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`},
			{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"complete\"}"}}]},"finish_reason":"tool_calls"}]}`},
		}

		index := turn
		if index >= len(frames) {
			index = len(frames) - 1
		}

		turn++

		for _, frame := range frames[index] {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	workdir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	configYAML := fmt.Sprintf(`
agent:
  model: test-model
  max_iterations: 5

default_provider: local

providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)

	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, "--dir", workdir, orderFile(t, "do the thing"))

	output, err := captureStdout(t, run)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, output)
	}

	for _, want := range []string{"do the thing", "on it", "complete"} {
		if !strings.Contains(output, want) {
			t.Errorf("transcript is missing %q:\n%s", want, output)
		}
	}
}

// The whole loop of the new order: zot new scaffolds the file with the full
// prompt in it, the operator writes the objective, and what the model is sent is
// that prompt rendered - the objective, the tools the run really has, where it is
// working, and the project's AGENTS.md, with the contract once.
func TestAScaffoldedOrderRunsWithItsFullPrompt(t *testing.T) {
	project := t.TempDir()

	mustWrite(t, filepath.Join(project, "AGENTS.md"), "Always mention PINECONE.")

	// the operator fills in the objective and a criterion and leaves the prompt as
	// zot wrote it
	withEditor(t, `sed -i 's/^objective:$/objective: build the parser\nacceptance:\n  - it parses/' "$1"`)

	var out strings.Builder

	if err := newOrder([]string{"--dir", project}, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	written, err := filepath.Glob(filepath.Join(project, order.BookDir, "orders", "*.md"))
	if err != nil || len(written) != 1 {
		t.Fatalf("orders written = %v, %v", written, err)
	}

	var system string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}

		_ = json.NewDecoder(r.Body).Decode(&body)

		if system == "" && len(body.Messages) > 0 && body.Messages[0].Role == "system" {
			system, _ = body.Messages[0].Content.(string)
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	mustWrite(t, configPath, fmt.Sprintf(`
agent:
  model: test-model
default_provider: local
providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL))

	withArgs(t, "--config", configPath, "--dir", project, written[0])

	if _, err := captureStdout(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, want := range []string{
		"## Your task\n\nbuild the parser",
		"1. it parses",
		`- "shell":`,
		`- "tasks":`,
		"# Project context\n\nAlways mention PINECONE.",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("the system prompt is missing %q:\n%s", want, system)
		}
	}

	if n := strings.Count(system, contractHeading); n != 1 {
		t.Errorf("the contract appears %d times, want once", n)
	}
}

// A run pointed at another directory works end to end: every relative path on
// the command line - --config, the order itself - resolves from
// the invoking directory before zot chdirs into --dir, the session records the
// real working directory, and project context comes from --dir.
func TestRunFromADifferentDirectoryEndToEnd(t *testing.T) {
	invocation := t.TempDir()

	t.Chdir(invocation)

	target := filepath.Join(invocation, "project")

	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// project context that only exists inside --dir: if either reaches the
	// provider, it was loaded from the right tree
	if err := os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte("# Project context\n\nAlways mention PINECONE.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// a relative skills_dir means the project: the skills tool only exists if
	// this folder, inside --dir, was found
	mustWrite(t, filepath.Join(target, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: ship it\n---\nDeploy carefully.\n")

	// every path on the command line is relative to the invoking directory -
	// none of them exist inside --dir, so they must resolve before the chdir
	if err := os.WriteFile("order.md", []byte(orderText("do the thing")+"{{ .Project }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	var sawContext, sawSkill atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		body, _ := io.ReadAll(r.Body)

		if strings.Contains(string(body), "PINECONE") {
			sawContext.Store(true)
		}

		if strings.Contains(string(body), `"name":"skills"`) {
			sawSkill.Store(true)
		}

		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"on it\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"complete\"}"}}]},"finish_reason":"tool_calls"}]}`)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	if err := os.WriteFile("config.yaml", []byte(fmt.Sprintf(`
agent:
  model: test-model
skills_dir: skills
default_provider: local
providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", "config.yaml", "--dir", target, "order.md")

	output, err := captureStdout(t, run)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, output)
	}

	for _, want := range []string{"do the thing", "on it", "complete"} {
		if !strings.Contains(output, want) {
			t.Errorf("transcript is missing %q:\n%s", want, output)
		}
	}

	if !sawContext.Load() || !sawSkill.Load() {
		t.Errorf("project context did not come from --dir (AGENTS.md seen: %v, skills tool seen: %v)",
			sawContext.Load(), sawSkill.Load())
	}

	// the log lands in the project being worked on, named after the order
	records := readLog(t, filepath.Join(target, ".zot", "orders", "order.jsonl"))

	if records[0].Meta == nil || records[0].Meta.Workdir != target {
		t.Errorf("meta = %+v, want the absolute --dir %q as workdir", records[0].Meta, target)
	}
}

// A run gets its own log, named after its order, with its own recorded outcome;
// an order that does not end in success fails the run.
func TestRunAnOrder(t *testing.T) {
	settle := func(name, args string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")

			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`,
				name, args))

			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
	}

	configFor := func(t *testing.T, url string) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), "config.yaml")

		if err := os.WriteFile(path, []byte(fmt.Sprintf(`
agent:
  model: test-model
default_provider: local
providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, url)), 0o644); err != nil {
			t.Fatal(err)
		}

		return path
	}

	t.Run("the run gets its own log", func(t *testing.T) {
		project := t.TempDir()

		server := settle("success", `{"summary":"complete"}`)
		defer server.Close()

		withArgs(t, "--config", configFor(t, server.URL), "--dir", project,
			orderFileIn(t, t.TempDir(), "first.md", "the first order"))

		if _, err := captureStdout(t, run); err != nil {
			t.Fatalf("run: %v", err)
		}

		records := readLog(t, filepath.Join(project, ".zot", "orders", "first.jsonl"))

		if records[0].Meta == nil || records[0].Meta.Task != "the first order" {
			t.Errorf("the log opens with %+v, want the order's objective as the task", records[0])
		}
	})

	t.Run("a failed order fails the run", func(t *testing.T) {
		project := t.TempDir()

		server := settle("failure", `{"reason":"cannot"}`)
		defer server.Close()

		withArgs(t, "--config", configFor(t, server.URL), "--dir", project,
			orderFileIn(t, t.TempDir(), "doomed.md", "the doomed order"))

		quietStderr(t)

		if _, err := captureStdout(t, run); err == nil {
			t.Fatal("a failed order must fail the run")
		}
	})
}

// A model with no context window is refused before any request, and the error
// names the model and the key to set: there is no table to guess from.
func TestRunRefusesAModelWithNoContextWindow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(configPath, []byte(`
agent:
  model: my-model
default_provider: local
providers:
  local:
    base_url: http://127.0.0.1:1
    models:
      my-model:
        model: some-real-id
`), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, orderFile(t, "a task"))

	err := run()
	if err == nil {
		t.Fatal("a model with no context window must not run")
	}

	for _, want := range []string{"providers.local.models.my-model", "context is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestRunRejectsAnInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	// a provider that has no endpoint
	if err := os.WriteFile(configPath, []byte(`
default_provider: nowhere
providers:
  nowhere: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, orderFile(t, "a task"))

	if err := run(); err == nil {
		t.Error("an unreachable provider must fail before any request")
	}
}

func TestRunRejectsAMissingConfigFile(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "nope.yaml"), orderFile(t, "a task"))

	if err := run(); err == nil {
		t.Error("an explicit but missing --config must be an error")
	}
}

// captureStdout collects what a function prints to stdout.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	return capture(t, &os.Stdout, fn)
}

// captureStderr collects what a function prints to stderr. Stdout and stderr are
// worth telling apart: stdout is the transcript, stderr is where zot talks about
// itself, and something that belongs on one must not leak onto the other.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	return capture(t, &os.Stderr, fn)
}

// capture redirects one of the process's standard streams for the duration of a
// call and returns what was written to it.
func capture(t *testing.T, stream **os.File, fn func() error) (string, error) {
	t.Helper()

	original := *stream

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	*stream = write

	done := make(chan string)

	go func() {
		var builder strings.Builder

		buffer := make([]byte, 4096)

		for {
			n, err := read.Read(buffer)

			builder.Write(buffer[:n])

			if err != nil {
				break
			}
		}

		done <- builder.String()
	}()

	runErr := fn()

	write.Close()

	*stream = original

	return <-done, runErr
}

// A run leaves a record: one log per order, in .zot/orders of the project,
// with the task and the outcome. Running the order again appends a new run to
// the same log rather than starting another file.
func TestRunRecordsASession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"complete\"}"}}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	workdir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	configYAML := fmt.Sprintf(`
agent:
  model: test-model
  max_iterations: 5

default_provider: local

providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)

	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	orderPath := orderFileIn(t, t.TempDir(), "1758300000.md", "the first task")

	withArgs(t, "--config", configPath, "--dir", workdir, orderPath)

	if _, err := captureStdout(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}

	logPath := filepath.Join(workdir, ".zot", "orders", "1758300000.jsonl")

	first := readLog(t, logPath)

	// the task is the durable objective, recorded in the meta (and placed in the
	// instructions), not as the opening user message
	meta := first[0]

	if meta.Kind != session.KindMeta || meta.Meta.Task != "the first task" {
		t.Errorf("the log must open with the objective: %+v", meta)
	}

	if meta.Meta.Model != "test-model" || meta.Meta.Workdir == "" {
		t.Errorf("meta = %+v", meta.Meta)
	}

	if last := first[len(first)-1]; last.Kind != session.KindResult || last.Result.Reason == "" {
		t.Errorf("the log must end with the outcome: %+v", last)
	}

	// running the order again adds a run to the same log, after the first
	withArgs(t, "--config", configPath, "--dir", workdir, orderPath)

	if _, err := captureStdout(t, run); err != nil {
		t.Fatalf("second run: %v", err)
	}

	second := readLog(t, logPath)

	if len(second) != 2*len(first) {
		t.Fatalf("the log holds %d records after two runs, want the first run's %d twice", len(second), len(first))
	}

	for i, record := range first {
		if second[i].Kind != record.Kind {
			t.Errorf("record %d changed from %s to %s: the first run must be left as it was", i, record.Kind, second[i].Kind)
		}
	}

	if again := second[len(first)]; again.Kind != session.KindMeta {
		t.Errorf("the second run must open with its own meta, got %+v", again)
	}
}

func settleOnce(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"complete\"}"}}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
agent:
  model: test-model
default_provider: local
providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	return configPath
}

// The tasks tool end to end: a model lists its work, keeps going, and settles.
// The real tool handler answers the call, so the run carries on to a second turn
// instead of ending on the list.
func TestARunsTaskListDoesNotEndTheRun(t *testing.T) {
	t.Chdir(t.TempDir())

	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		call := `{"name":"success","arguments":"{\"summary\":\"complete\"}"}`

		if requests.Add(1) == 1 {
			call = `{"name":"tasks","arguments":"{\"tasks\":[` +
				`{\"title\":\"read the parser\",\"status\":\"done\"},` +
				`{\"title\":\"fix the lexer\",\"status\":\"in_progress\",\"note\":\"off by one\"},` +
				`{\"title\":\"add a test\",\"status\":\"pending\"}]}"}`
		}

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":`+call+`}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
agent:
  model: test-model
default_provider: local
providers:
  local:
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, "--dir", t.TempDir(), orderFile(t, "fix the lexer"))

	if _, err := captureStdout(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}

	if requests.Load() != 2 {
		t.Errorf("the model was called %d times, want the tasks turn and the settling turn", requests.Load())
	}
}

func TestAnOrdersTitleReachesTheViewer(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a declared title",
			body: "---\ntitle: Rate limiting\nobjective: add rate limiting to the api\n---\nbody\n",
			want: "Rate limiting",
		},
		{
			name: "otherwise the file name",
			body: "---\nobjective: add rate limiting to the api\n---\nbody\n",
			want: "Fix the flaky test", // from fix-the-flaky-test.md
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orderPath := filepath.Join(t.TempDir(), "fix-the-flaky-test.md")

			if err := os.WriteFile(orderPath, []byte(test.body), 0o644); err != nil {
				t.Fatal(err)
			}

			loaded, err := order.Load(orderPath)
			if err != nil {
				t.Fatal(err)
			}

			got := orderOptions(t.TempDir(), loaded)

			if got.Title != test.want {
				t.Errorf("viewer title = %q, want %q", got.Title, test.want)
			}
		})
	}
}

func TestLoadProjectContext(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	// A global AGENTS.md in the config dir and a project one in the work dir.
	mustWrite(t, filepath.Join(configDir, "AGENTS.md"), "GLOBAL CONVENTIONS")
	mustWrite(t, filepath.Join(workDir, "AGENTS.md"), "PROJECT CONVENTIONS")

	cfg := config.Config{}
	loadProjectContext(&cfg, configDir, workDir, workDir)

	// both files are there, the config directory's first, each once
	for _, want := range []string{"GLOBAL CONVENTIONS", "PROJECT CONVENTIONS"} {
		if strings.Count(cfg.ProjectContext, want) != 1 {
			t.Errorf("project context should hold %q once:\n%s", want, cfg.ProjectContext)
		}
	}

	if i, j := strings.Index(cfg.ProjectContext, "GLOBAL"), strings.Index(cfg.ProjectContext, "PROJECT"); i > j {
		t.Error("expected config-dir AGENTS.md to appear before work-dir AGENTS.md")
	}
}

func TestLoadProjectContextNoFiles(t *testing.T) {
	cfg := config.Config{}
	loadProjectContext(&cfg, t.TempDir())

	if cfg.ProjectContext != "" {
		t.Error("expected no project context when no AGENTS.md is present")
	}
}

func TestLoadSkillsFromTheConfiguredFolder(t *testing.T) {
	t.Run("unset means no skills", func(t *testing.T) {
		cfg := config.Config{}

		if err := loadSkills(&cfg); err != nil || cfg.Skills != nil {
			t.Errorf("skills = %v, err = %v, want none and no error", cfg.Skills, err)
		}
	})

	t.Run("a relative folder is taken against the working directory", func(t *testing.T) {
		project := t.TempDir()
		mustWrite(t, filepath.Join(project, "my-skills", "greet", "SKILL.md"), "---\nname: greet\ndescription: say hello\n---\nbody")
		t.Chdir(project)

		cfg := config.Config{SkillsDir: "my-skills"}

		if err := loadSkills(&cfg); err != nil {
			t.Fatalf("loadSkills: %v", err)
		}

		if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "greet" || cfg.Skills[0].Content == "" {
			t.Errorf("skills = %+v, want greet loaded with its content", cfg.Skills)
		}
	})

	t.Run("~ is the home directory", func(t *testing.T) {
		home := t.TempDir()
		mustWrite(t, filepath.Join(home, "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\nbody")
		t.Setenv("HOME", home)

		cfg := config.Config{SkillsDir: "~/skills"}

		if err := loadSkills(&cfg); err != nil {
			t.Fatalf("loadSkills: %v", err)
		}

		if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "deploy" {
			t.Errorf("skills = %+v, want deploy from ~/skills", cfg.Skills)
		}
	})

	t.Run("a folder that cannot be read stops the run", func(t *testing.T) {
		cfg := config.Config{SkillsDir: filepath.Join(t.TempDir(), "missing")}

		err := loadSkills(&cfg)
		if err == nil || !strings.Contains(err.Error(), "skills_dir") {
			t.Errorf("err = %v, want it to name skills_dir", err)
		}
	})
}

// The whole path a skill takes: the model lists the skills, reads one by name,
// and each answer reaches its next request - from memory, with the folder gone.
func TestTheModelListsAndReadsASkill(t *testing.T) {
	project := t.TempDir()
	skillsDir := filepath.Join(project, "skills")

	mustWrite(t, filepath.Join(skillsDir, "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: LISTING-MARKER\n---\n# Deploy\n\nINSTRUCTIONS-MARKER\n")

	var (
		requests atomic.Int32
		bodies   = make(chan string, 8)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)

		var call string

		switch requests.Add(1) {
		case 1:
			call = `{"name":"skills","arguments":"{}"}`
		case 2:
			call = `{"name":"skills","arguments":"{\"name\":\"deploy\"}"}`
		default:
			call = `{"name":"success","arguments":"{\"summary\":\"complete\"}"}`
		}

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":`+call+`}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	cfg := stubProvider(t)
	cfg.DefaultProvider = "local"
	cfg.Providers = map[string]config.ProviderConfig{
		"local": {BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")},
	}
	cfg.SkillsDir = skillsDir

	if err := loadSkills(&cfg); err != nil {
		t.Fatal(err)
	}

	// loaded at startup: the folder is not read again during the run
	if err := os.RemoveAll(skillsDir); err != nil {
		t.Fatal(err)
	}

	if _, err := quietly(t, func() error {
		return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{})
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	close(bodies)

	var all []string
	for body := range bodies {
		all = append(all, body)
	}

	if len(all) != 3 {
		t.Fatalf("the model was called %d times, want list, read, settle", len(all))
	}

	if strings.Contains(all[0], "LISTING-MARKER") || strings.Contains(all[0], "INSTRUCTIONS-MARKER") {
		t.Error("nothing of a skill may reach the model before it asks")
	}

	if !strings.Contains(all[1], "LISTING-MARKER") || strings.Contains(all[1], "INSTRUCTIONS-MARKER") {
		t.Error("the listing must carry the description and not the instructions")
	}

	if !strings.Contains(all[2], "INSTRUCTIONS-MARKER") {
		t.Error("reading a skill by name must return its full instructions")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeCfg writes a config file and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Credential resolution, which is the part of the configuration that fails
// silently. A key that does not arrive presents as a 401 from the provider,
// which reads like a bad key rather than a config that never picked it up.
//
// These assert on the Authorization header the provider actually receives,
// because that is the only thing that proves a credential was resolved rather
// than merely accepted by the parser. The layering - provider key, per-model
// override, key inlined into the model name - is what the README documents and
// what an older config relies on.
func TestCredentialResolutionLayers(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		config string
		want   string
		model  string
	}{
		{
			name:   "a provider api_key",
			config: "    api_key: sk-provider\n    models:\n      gpt-4:\n        context: 100000\n",
			want:   "Bearer sk-provider",
			model:  "gpt-4",
		},
		{
			name:   "a $VAR reference, so no secret is on disk",
			env:    map[string]string{"MY_PROVIDER_KEY": "sk-from-env"},
			config: "    api_key: $MY_PROVIDER_KEY\n    models:\n      gpt-4:\n        context: 100000\n",
			want:   "Bearer sk-from-env",
			model:  "gpt-4",
		},
		{
			name: "a per-model key overrides the provider's",
			config: "    api_key: sk-provider\n" +
				"    models:\n      gpt-4:\n        api_key: sk-for-gpt4\n        context: 100000\n",
			want:  "Bearer sk-for-gpt4",
			model: "gpt-4",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for key, value := range test.env {
				t.Setenv(key, value)
			}

			seen := make(chan string, 1)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case seen <- r.Header.Get("Authorization"):
				default:
				}

				w.Header().Set("Content-Type", "text/event-stream")

				fmt.Fprintf(w, "data: %s\n\n",
					`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"done\"}"}}]},"finish_reason":"tool_calls"}]}`)

				fmt.Fprint(w, "data: [DONE]\n\n")
			}))

			defer server.Close()

			path := writeCfg(t, fmt.Sprintf(`
agent:
  model: %q
default_provider: myprovider
providers:
  myprovider:
    base_url: %s
%s`, test.model, server.URL, test.config))

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			client, _, err := resolve(cfg)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			if got := client.Config().Model; got != "gpt-4" {
				t.Errorf("model = %q", got)
			}

			if _, err := quietly(t, func() error {
				return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{})
			}); err != nil {
				t.Fatalf("run: %v", err)
			}

			select {
			case got := <-seen:
				if got != test.want {
					t.Errorf("the provider received %q, want %q", got, test.want)
				}
			default:
				t.Fatal("the provider was never called")
			}
		})
	}
}

// testDefaults is the built-in configuration with the one thing it deliberately
// lacks: a model to run.
// declared is the model list a provider needs to run the named models: each
// with a context window, since a model without one cannot run.
func declared(names ...string) map[string]config.ModelConfig {
	models := make(map[string]config.ModelConfig, len(names))

	for _, name := range names {
		models[name] = config.ModelConfig{Context: 100_000}
	}

	return models
}

func testDefaults() config.Config {
	cfg := config.Defaults()
	cfg.Agent.Model = "glm-5.2"

	return cfg
}

// content_array on a model has to reach the wire, so every message the run
// sends carries array content, while other models keep the plain string.
func TestContentArrayReachesTheWire(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra string
		want  string
	}{
		{name: "asked for", extra: "        content_array: true\n", want: "["},
		{name: "not asked for", want: `"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("ZOT_CONFIG", "")

			seen := make(chan []json.RawMessage, 1)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}

				json.NewDecoder(r.Body).Decode(&body)

				contents := make([]json.RawMessage, 0, len(body.Messages))

				for _, message := range body.Messages {
					contents = append(contents, message.Content)
				}

				select {
				case seen <- contents:
				default:
				}

				w.Header().Set("Content-Type", "text/event-stream")

				fmt.Fprintf(w, "data: %s\n\n",
					`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"done\"}"}}]},"finish_reason":"tool_calls"}]}`)

				fmt.Fprint(w, "data: [DONE]\n\n")
			}))

			defer server.Close()

			path := writeCfg(t, fmt.Sprintf(`
agent:
  model: default
default_provider: selfhosted
providers:
  selfhosted:
    base_url: %s
    api_key: x
    models:
      default:
        model: Qwen3.8-27B
        context: 100000
%s`, server.URL, test.extra))

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if _, err := quietly(t, func() error {
				return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{})
			}); err != nil {
				t.Fatalf("run: %v", err)
			}

			select {
			case contents := <-seen:
				if len(contents) < 2 {
					t.Fatalf("saw %d messages, want the system prompt and the task", len(contents))
				}

				for i, content := range contents {
					if !strings.HasPrefix(string(content), test.want) {
						t.Errorf("message %d content = %.40s, want it to start with %s", i, content, test.want)
					}
				}
			default:
				t.Fatal("the provider was never called")
			}
		})
	}
}

// A provider that names no endpoint cannot resolve, and says so rather than
// sending a request to nowhere.
func TestAProviderWithoutAnEndpointIsRejected(t *testing.T) {
	cfg := testDefaults()
	cfg.DefaultProvider = "myprovider"
	cfg.Providers = map[string]config.ProviderConfig{"myprovider": {APIKey: "sk-test", Models: declared("glm-5.2")}}

	_, _, err := resolve(cfg)
	if err == nil {
		t.Fatal("a provider naming no endpoint must be rejected")
	}

	// the error has to be actionable: it names the field to set
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error = %q, want it to name what is missing", err)
	}
}

// shell acts on the machine, so a call the model did not finish writing is
// refused, never mended into one that runs.
func TestResolveNeverRepairsAShellCall(t *testing.T) {
	cfg := testDefaults()
	cfg.DefaultProvider = "p"
	cfg.Providers = map[string]config.ProviderConfig{
		"p": {BaseURL: "http://127.0.0.1:1", Models: declared("glm-5.2")},
	}

	_, opts, err := resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(opts.Unrepaired) != 1 || opts.Unrepaired[0] != tools.ShellTool {
		t.Errorf("Unrepaired = %v, want just the shell tool", opts.Unrepaired)
	}
}

// The window is the operator's to state and zot keeps no table of what models
// can take, so a model with none cannot run. Load-time validation says so first;
// resolve holds the same rule for a config that skipped it.
func TestResolveRefusesAModelWithoutAContextWindow(t *testing.T) {
	cases := map[string]map[string]config.ModelConfig{
		"the model is not declared":     declared("some-other-model"),
		"no models are declared at all": nil,
		"the window is zero":            {"glm-5.2": {Model: "glm-5.2"}},
		"the window is negative":        {"glm-5.2": {Context: -1}},
	}

	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testDefaults()
			cfg.DefaultProvider = "local"
			cfg.Providers = map[string]config.ProviderConfig{
				"local": {BaseURL: "http://127.0.0.1:1", Models: models},
			}

			_, _, err := resolve(cfg)
			if err == nil {
				t.Fatal("a model with no context window resolved")
			}

			for _, want := range []string{"glm-5.2", "context window", "providers.local.models"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should mention %q", err, want)
				}
			}
		})
	}
}

// A declared provider resolves to its own endpoint and credential, with the
// model name passed through untouched, and its own name is what the client
// reports.
func TestResolveDeclaredProviders(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("ALPHA_KEY", "sk-alpha")

	cfg, err := config.Load(writeCfg(t, `
agent:
  model: some-model
providers:
  alpha:
    base_url: https://alpha.example.com/v1
    api_key: $ALPHA_KEY
    models:
      some-model:
        context: 100000
  beta:
    base_url: https://beta.example.com/v1
    api_key: sk-beta
    models:
      some-model:
        context: 100000
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for name, wantURL := range map[string]string{
		"alpha": "https://alpha.example.com/v1",
		"beta":  "https://beta.example.com/v1",
	} {
		cfg.DefaultProvider = name

		client, _, err := resolve(cfg)
		if err != nil {
			t.Fatalf("resolve(%s): %v", name, err)
		}

		if got := client.Config().Model; got != "some-model" {
			t.Errorf("%s model = %q, want it unchanged", name, got)
		}

		if got := client.Config().Provider; got != name {
			t.Errorf("%s provider = %q, want %q", name, got, name)
		}

		if got := client.Config().BaseURL; got != wantURL {
			t.Errorf("%s endpoint = %q, want %q", name, got, wantURL)
		}
	}
}

// Nothing is built in: naming a provider that was never declared fails, whatever
// the name and whatever the environment holds.
func TestNoProviderIsBuiltIn(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai")

	cfg := testDefaults()

	for _, name := range []string{"openai", "anthropic", "zai", "ollama", "openrouter"} {
		cfg.DefaultProvider = name

		if _, _, err := resolve(cfg); err == nil {
			t.Errorf("%q resolved with nothing declared", name)
		}
	}
}

// A custom model entry aliases a real id, caps iterations, and carries its own
// credential, all of which take priority over the run defaults.
func TestResolveCustomModelAlias(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	path := writeCfg(t, `
agent:
  model: fast
default_provider: mygateway
providers:
  mygateway:
    base_url: https://gw.example.com/v1
    models:
      fast:
        model: gpt-5
        max_iterations: 50
        api_key: $OPENAI_API_KEY
        context: 32000
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	client, opts, err := resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := client.Config().Model; got != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", got)
	}
	if opts.MaxIterations != 50 {
		t.Errorf("max iterations = %d, want 50 (from custom model)", opts.MaxIterations)
	}

	// the operator declared the endpoint's real window; the run must budget to it
	if opts.ContextWindow != 32000 {
		t.Errorf("context window = %d, want the per-model override", opts.ContextWindow)
	}
}

// The iteration denominator in the meta bar has to be the limit the run will
// actually stop at. A per-model max_iterations lowers that limit, and a bar
// counting towards a number the run never reaches - "iter 12/100" on a run the
// engine ends at 40 - misreports the run to the only person watching it.
func TestTheViewerShowsTheIterationLimitTheRunEnforces(t *testing.T) {
	cfg := testDefaults()
	cfg.DefaultProvider = "openai"
	cfg.Agent.Model = "capped"
	cfg.Agent.MaxIterations = 100
	cfg.Providers = map[string]config.ProviderConfig{
		"openai": {
			BaseURL: "https://gw.example.com/v1",
			APIKey:  "sk-test",
			Models: map[string]config.ModelConfig{
				"capped": {Model: "gpt-5", MaxIterations: 40, Context: 100_000},
			},
		},
	}

	_, opts, err := resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if opts.MaxIterations != 40 {
		t.Fatalf("the run resolved to %d iterations, want the model's cap", opts.MaxIterations)
	}

	meta := viewerMeta(cfg, "a task", "/somewhere", opts)

	if meta.MaxIterations != opts.MaxIterations {
		t.Errorf("the viewer shows a limit of %d while the engine stops at %d",
			meta.MaxIterations, opts.MaxIterations)
	}

	// the default is a 1,000,000 backstop rather than a budget, so there is
	// nothing worth counting towards and the denominator stays hidden
	cfg.Agent.MaxIterations = config.Defaults().Agent.MaxIterations
	cfg.Providers["openai"].Models["capped"] = config.ModelConfig{Model: "gpt-5", Context: 100_000}

	_, opts, err = resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := viewerMeta(cfg, "a task", "/somewhere", opts).MaxIterations; got != 0 {
		t.Errorf("the viewer shows a limit of %d, want the backstop hidden", got)
	}
}

// runTask is the whole thing end to end: config in, a provider call out, a
// transcript back. The tests' stand-in viewer prints what the run said, which is
// what can be asserted on without a terminal.
func TestRunTaskEndToEnd(t *testing.T) {
	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		frames := [][]string{
			{`{"choices":[{"delta":{"content":"working on it"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`},
			{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`},
		}

		index := turn
		if index >= len(frames) {
			index = len(frames) - 1
		}

		turn++

		for _, frame := range frames[index] {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	cfg := testDefaults()
	cfg.DefaultProvider = "local"
	cfg.Providers = map[string]config.ProviderConfig{
		"local": {BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")},
	}

	original := os.Stdout

	read, write, _ := os.Pipe()

	os.Stdout = write

	done := make(chan string)

	go func() {
		var builder strings.Builder

		buffer := make([]byte, 4096)

		for {
			n, err := read.Read(buffer)

			builder.Write(buffer[:n])

			if err != nil {
				break
			}
		}

		done <- builder.String()
	}()

	err := runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{})

	write.Close()

	os.Stdout = original

	output := <-done

	if err != nil {
		t.Fatalf("Run: %v\n%s", err, output)
	}

	for _, want := range []string{"do the thing", "working on it", "all done"} {
		if !strings.Contains(output, want) {
			t.Errorf("transcript is missing %q:\n%s", want, output)
		}
	}
}

// A misconfigured provider fails before any request is made, with a message that
// says what to fix.
func TestRunRejectsAnUnconfiguredProvider(t *testing.T) {
	cfg := testDefaults()
	cfg.DefaultProvider = "nowhere"
	cfg.Providers = map[string]config.ProviderConfig{}

	err := runTask(context.Background(), cfg, testOrder("task"), runOptions{})

	if err == nil {
		t.Fatal("an unconfigured provider must fail")
	}

	if !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("the error should name the provider: %v", err)
	}
}

func stubProvider(t *testing.T) config.Config {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	cfg := testDefaults()
	cfg.DefaultProvider = "local"
	cfg.Providers = map[string]config.ProviderConfig{
		"local": {BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")},
	}

	return cfg
}

// quietly runs a function with stdout discarded, returning what it printed.
func quietly(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	original := os.Stdout

	read, write, _ := os.Pipe()

	os.Stdout = write

	done := make(chan string)

	go func() {
		var builder strings.Builder

		buffer := make([]byte, 4096)

		for {
			n, err := read.Read(buffer)

			builder.Write(buffer[:n])

			if err != nil {
				break
			}
		}

		done <- builder.String()
	}()

	err := fn()

	write.Close()

	os.Stdout = original

	return <-done, err
}

// readSession decodes every line of a session log, failing on any line that is
// not a JSON record.
func readSession(t *testing.T, path string) []session.Record {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	var records []session.Record

	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var record session.Record

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not a JSON record: %v\n%s", i+1, err, line)
		}

		records = append(records, record)
	}

	return records
}

// A run leaves a record of itself: what it was asked, which model answered, and
// how it ended.
func TestRunWithRecordsASession(t *testing.T) {
	cfg := stubProvider(t)

	path := filepath.Join(t.TempDir(), ".zot", "orders", "task.jsonl")

	if _, err := quietly(t, func() error {
		return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{SessionPath: path})
	}); err != nil {
		t.Fatalf("RunWith: %v", err)
	}

	records := readSession(t, path)

	first, last := records[0], records[len(records)-1]

	if first.Kind != session.KindMeta || first.Meta == nil {
		t.Fatalf("the log must open with the meta: %+v", first)
	}

	if first.Meta.Task != "do the thing" || first.Meta.Provider != "local" {
		t.Errorf("meta = %+v", first.Meta)
	}

	if first.Meta.Model == "" || first.Meta.Workdir == "" {
		t.Errorf("the log must record what it ran against: %+v", first.Meta)
	}

	if last.Kind != session.KindResult || last.Result == nil || last.Result.Reason == "" {
		t.Errorf("the log must end with the outcome: %+v", last)
	}

	// the objective is the durable task, recorded in the meta and placed in the
	// instructions; the opening message is the kickoff, not the task
	var opening bool

	for _, record := range records {
		if record.Kind == session.KindMessage && record.Message.Text == taskKickoff {
			opening = true
		}
	}

	if !opening {
		t.Errorf("the log must record the opening message: %v", records)
	}
}

// Running the same order again adds a run to its log rather than replacing it,
// and starts from zero: the second run opens with the kickoff and carries
// nothing of the first run's conversation.
func TestRunningTheSameTaskAgainAppendsAFreshRun(t *testing.T) {
	cfg := stubProvider(t)

	path := filepath.Join(t.TempDir(), "task.jsonl")

	for i := 0; i < 2; i++ {
		if output, err := quietly(t, func() error {
			return runTask(context.Background(), cfg, testOrder("the same brief"), runOptions{SessionPath: path})
		}); err != nil {
			t.Fatalf("run %d: %v\n%s", i+1, err, output)
		}
	}

	var runs [][]session.Record

	for _, record := range readSession(t, path) {
		if record.Kind == session.KindMeta {
			runs = append(runs, nil)
		}

		if len(runs) == 0 {
			t.Fatalf("a record precedes the first meta: %+v", record)
		}

		runs[len(runs)-1] = append(runs[len(runs)-1], record)
	}

	if len(runs) != 2 {
		t.Fatalf("got %d runs in the log, want both", len(runs))
	}

	if len(runs[0]) != len(runs[1]) {
		t.Errorf("the runs differ in length (%d, %d), so one carried the other", len(runs[0]), len(runs[1]))
	}

	for i, run := range runs {
		if last := run[len(run)-1]; last.Kind != session.KindResult {
			t.Errorf("run %d does not end with its outcome: %+v", i+1, last)
		}
	}
}

// The log holds what the model thought, and holds it while a tool is still
// running: a snapshot of the log taken by the command itself already carries the
// turn's reasoning and the request being run, so a run killed inside a long
// command loses nothing of the turn that started it.
func TestTheLogHoldsReasoningBeforeItsToolFinishes(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "task.jsonl")
	snapshot := filepath.Join(dir, "snapshot.jsonl")

	command, err := json.Marshal(map[string]string{"command": "cp " + path + " " + snapshot})
	if err != nil {
		t.Fatal(err)
	}

	call, err := json.Marshal(string(command))
	if err != nil {
		t.Fatal(err)
	}

	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		turn++

		if turn == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"copy the log while the shell runs"}}]}`+"\n\n")
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"shell","arguments":%s}}]},"finish_reason":"tool_calls"}]}`+"\n\n", call)
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	cfg := stubProvider(t)
	cfg.Providers["local"] = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")}

	if output, err := quietly(t, func() error {
		return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{SessionPath: path})
	}); err != nil {
		t.Fatalf("RunWith: %v\n%s", err, output)
	}

	var reasoning, request bool

	for _, record := range readSession(t, snapshot) {
		if record.Kind != session.KindMessage {
			continue
		}

		if record.Message.Type == "reasoning" && record.Message.Text == "copy the log while the shell runs" {
			reasoning = true
		}

		if record.Message.Activity != nil && record.Message.Activity.Kind == "request" && record.Message.Activity.Name == "shell" {
			request = true
		}
	}

	if !reasoning || !request {
		t.Errorf("the log was missing the turn while its tool ran (reasoning %v, request %v)", reasoning, request)
	}

	// and the finished log keeps it too
	var final bool

	for _, record := range readSession(t, path) {
		if record.Kind == session.KindMessage && record.Message.Type == "reasoning" {
			final = true
		}
	}

	if !final {
		t.Error("the finished log lost the model's reasoning")
	}
}

// The digest names the log the run was appended to, and says nothing of one
// when the run was not recorded.
func TestPrintDigestNamesTheSessionLog(t *testing.T) {
	result := loop.Result{Reason: loop.StopSettled, Budget: loop.Budget{Iterations: 1}}

	var recorded, unrecorded strings.Builder

	printDigest(&recorded, "/w/.zot/orders/1758300000.jsonl", result)
	printDigest(&unrecorded, "", result)

	if !strings.Contains(recorded.String(), "/w/.zot/orders/1758300000.jsonl") {
		t.Errorf("the digest must say where the log is:\n%s", recorded.String())
	}

	if strings.Contains(unrecorded.String(), "session") {
		t.Errorf("no log was written, so the digest must not mention one:\n%s", unrecorded.String())
	}
}

// The run is the point. A log that cannot be opened is reported and the work
// goes ahead - refusing to work because a directory is read-only would be a
// worse failure than losing the record of it.
func TestRunWithSurvivesAnUnwritableSessionPath(t *testing.T) {
	cfg := stubProvider(t)

	blocked := filepath.Join(t.TempDir(), "a-file")

	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := quietly(t, func() error {
		return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{
			SessionPath: filepath.Join(blocked, "task.jsonl"),
		})
	})
	if err != nil {
		t.Fatalf("RunWith: %v\n%s", err, output)
	}

	if !strings.Contains(output, "all done") {
		t.Errorf("the run should have finished regardless:\n%s", output)
	}
}

// No session path means no log, and that has to be silent rather than an
// error a caller has to handle.
func TestRunWithoutASessionPathWritesNothing(t *testing.T) {
	cfg := stubProvider(t)

	dir := t.TempDir()

	t.Chdir(dir)

	if _, err := quietly(t, func() error {
		return runTask(context.Background(), cfg, testOrder("do the thing"), runOptions{})
	}); err != nil {
		t.Fatalf("RunWith: %v", err)
	}

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a run with no session path wrote %d entries", len(entries))
	}
}

// The example config is what `zot config` writes on first run, so it is the
// first thing most people ever edit. Its knobs drifting from the code's own
// defaults is not cosmetic: someone copies it, changes nothing, and gets
// different behaviour from someone who has no config file at all. The provider
// and model are the exception - there are no defaults for those, and the
// example shows the shape of declaring them.
func TestTheExampleConfigMatchesTheDefaults(t *testing.T) {
	var example config.Config

	if err := yaml.Unmarshal(configs.ExampleConfigYAML, &example); err != nil {
		t.Fatalf("the embedded example config does not parse: %v", err)
	}

	defaults := config.Defaults()

	if example.Agent.MaxIterations != defaults.Agent.MaxIterations {
		t.Errorf("example max_iterations = %d, defaults = %d",
			example.Agent.MaxIterations, defaults.Agent.MaxIterations)
	}
}

// A config file is only useful if it survives being loaded, and the example is
// the one file guaranteed to be in front of a new user.
func TestTheExampleConfigLoadsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(path, configs.ExampleConfigYAML, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}

	// a key so validation is judging the shape rather than the environment
	provider := cfg.Providers[cfg.DefaultProvider]
	provider.APIKey = "test-key"
	cfg.Providers[cfg.DefaultProvider] = provider

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the example config does not validate: %v", err)
	}
}

// A run with nothing configured fails before any request, and says what to
// declare - there is no default provider or model to fall back on.
func TestARunWithNothingConfiguredSaysWhatIsMissing(t *testing.T) {
	cfg := config.Defaults()

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "agent.model") {
		t.Errorf("Validate = %v, want it to name the missing model", err)
	}

	cfg.Agent.Model = "m"

	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "providers:") {
		t.Errorf("Validate = %v, want it to say to declare a provider", err)
	}

	// and the library entry point, which does not validate, says the same
	err = runTask(context.Background(), cfg, testOrder("task"), runOptions{})
	if err == nil || !strings.Contains(err.Error(), "providers:") {
		t.Errorf("Run = %v, want it to say to declare a provider", err)
	}
}

// promptOf renders an order the way a run does, with the tools a run really has.
func promptOf(t *testing.T, o order.Order) string {
	t.Helper()

	client, opts, err := resolve(stubProviderConfig(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	prompt, err := o.Render(orderEnv(stubProviderConfig(t), client, opts, "/work"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	return prompt
}

// stubProviderConfig is a config that resolves without a network.
func stubProviderConfig(t *testing.T) config.Config {
	t.Helper()

	cfg := testDefaults()
	cfg.DefaultProvider = "local"
	cfg.Providers = map[string]config.ProviderConfig{
		"local": {BaseURL: "http://127.0.0.1:1", APIKey: "k", Models: declared("glm-5.2")},
	}

	return cfg
}

// newOrderNamed is the order zot new scaffolds, with its objective written in.
func newOrderNamed(t *testing.T, objective string) order.Order {
	t.Helper()

	o, err := order.Parse([]byte(strings.Replace(order.Blank(), "objective:\n", "objective: "+objective+"\n", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	return o
}

// defaultPrompt is what a freshly scaffolded order sends the model.
func defaultPrompt(t *testing.T) string {
	t.Helper()

	return promptOf(t, newOrderNamed(t, "build a parser"))
}

// The task is the durable objective, so it must land in the system prompt, which
// trimming never drops and always orders first - not as a user message, which a
// long run can trim away. An agent that forgets its own objective is the worst
// way for a run to fail.
func TestTheObjectiveGoesIntoTheSystemPrompt(t *testing.T) {
	o, err := order.Parse([]byte(strings.Replace(order.Blank(), "objective:\n", "objective: \"  build a parser  \"\n", 1)))
	if err != nil {
		t.Fatal(err)
	}

	got := promptOf(t, o)

	if !strings.Contains(got, "You are zot") {
		t.Error("the order's own prompt must be what is sent")
	}

	if !strings.Contains(got, "## Your task\n\nbuild a parser") {
		t.Errorf("the objective must be in the prompt, trimmed:\n%s", got)
	}
}

// The tools the prompt names come from the tool set the run really has, so it
// cannot describe tools that are not offered. The prompt drifted once already -
// it told the agent to call "edit", "exec", "exit" and "progress" when those
// tools did not exist - which is what generating the list prevents; this pins it.
func TestTheDefaultPromptNamesOnlyRealTools(t *testing.T) {
	prompt := defaultPrompt(t)

	real := map[string]bool{
		// the terminal tools the loop injects
		"success": true,
		"failure": true,
	}

	for _, tool := range tools.New(1000, nil) {
		real[tool.Info().Name] = true
	}

	// pull every "quoted" token out of the prompt and check the tool-looking ones
	// are real
	for _, quoted := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(prompt, -1) {
		name := quoted[1]

		// only check things that look like tool names (a real tool, or the
		// phantom ones we are guarding against)
		phantom := map[string]bool{"edit": true, "exec": true, "exit": true, "abort": true, "read": true, "write": true, "list": true, "plan": true, "progress": true}

		if !real[name] && phantom[name] {
			t.Errorf("the prompt names %q, which is not a real tool", name)
		}
	}

	// and positively assert the tools the prompt promises are all present
	for _, want := range []string{"tasks", "shell", "success", "failure"} {
		if !real[want] {
			t.Errorf("the prompt relies on %q but it is not a real tool", want)
		}

		if !strings.Contains(prompt, `"`+want+`"`) {
			t.Errorf("the prompt should name the %q tool so the model knows to use it", want)
		}
	}
}

// A tool the run does not have is not in its prompt: no skills, no skills tool.
func TestThePromptListsTheToolsTheRunHas(t *testing.T) {
	without := defaultPrompt(t)

	if strings.Contains(without, `- "skills":`) {
		t.Error("the prompt lists a skills tool the run does not have")
	}

	cfg := stubProviderConfig(t)
	cfg.Skills = []tools.Skill{{Name: "deploy", Description: "ship it"}}

	client, opts, err := resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}

	with, err := newOrderNamed(t, "x").Render(orderEnv(cfg, client, opts, "/work"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(with, `- "skills":`) {
		t.Errorf("the prompt must list the skills tool when the run has one:\n%s", with)
	}
}

// With shell the only tool that touches the machine, the model has to be told so
// and shown how to read, list and write with it. A prompt that only said "shell"
// would leave a model reaching for file tools it does not have.
func TestTheDefaultPromptTeachesShellAsTheOnlyWayToTouchTheMachine(t *testing.T) {
	prompt := defaultPrompt(t)

	for _, want := range []string{
		"only way to act on the machine",
		"cat", "sed -n", "grep -n", "ls", "heredoc",

		// writing a file through the shell is where an unquoted heredoc mangles
		// what was written, so the rule that prevents it is part of the prompt
		"quoted heredoc",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt should mention %q so the model knows how to work through shell", want)
		}
	}
}

// The tasks tool only helps if the model keeps it current, and the prompt is the
// only thing that says how: each status it may use, and that a blocker or an
// assumption belongs in a note.
func TestTheDefaultPromptTeachesHowToKeepTheTasksCurrent(t *testing.T) {
	prompt := defaultPrompt(t)

	for _, want := range []string{"in_progress", "done", "blocked", "note", "whole list"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt should mention %q so the model keeps its tasks current", want)
		}
	}
}

// The prompt knows where the run is: the project's AGENTS.md, and the facts of the
// run itself.
func TestThePromptCarriesTheProjectAndTheRun(t *testing.T) {
	cfg := stubProviderConfig(t)
	cfg.ProjectContext = "Always mention PINECONE."

	client, opts, err := resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}

	o, err := order.Parse([]byte("---\nobjective: go\n---\n{{ .Workdir }}|{{ .Model }}|{{ .Provider }}|{{ .Date }}|{{ .Project }}|{{ range .Tools }}{{ .Name }},{{ end }}"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := o.Render(orderEnv(cfg, client, opts, "/work/project"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(got, "/work/project|glm-5.2|local|"+time.Now().Format("2006-01-02")+"|Always mention PINECONE.|shell,tasks,") {
		t.Errorf("rendered = %q", got)
	}
}

// zot has no input channel: no stdin, no chat turn, no approval prompt - a run
// is a work order, a provider and a read-only viewer. An agent that does not
// know that asks a question and waits, and waiting is fatal in a way no other
// prompt mistake is: nothing answers, the run burns its budget until a guard
// kills it, and the work it never wrote is lost. These pin the directives that
// prevent it. A prompt cannot be tested against a model here, so the patterns
// are deliberately loose - they assert the directive survives a rewrite of the
// wording, not the wording itself.
var nonInteractiveDirectives = []struct {
	need    string
	pattern *regexp.Regexp
}{
	{"say the run is non-interactive", regexp.MustCompile(`(?i)non-interactive`)},
	{"say nothing reaches the user", regexp.MustCompile(`(?i)nothing you address to the user is delivered|no reader|will never be seen|no one is watching`)},
	{"forbid waiting for input", regexp.MustCompile(`(?i)never stop to wait|do not (stop and )?wait|NO further input`)},
	{"name approval and confirmation as things not to wait for", regexp.MustCompile(`(?i)approval, permission or confirmation|approval|confirmation`)},
	{"forbid ending a turn with a question", regexp.MustCompile(`(?i)never end your turn with a question|do not ask`)},
	{"require deciding and recording the assumption instead", regexp.MustCompile(`(?i)assumption`)},
	{"require a terminal tool call to end the task", regexp.MustCompile(`(?i)"success".*\n?.*"failure"|"failure"`)},
	{"forbid simply stopping", regexp.MustCompile(`(?i)do not simply stop`)},
}

// assertNonInteractive checks that every directive above is present in what the
// engine would send.
func assertNonInteractive(t *testing.T, where, instructions string) {
	t.Helper()

	for _, directive := range nonInteractiveDirectives {
		if !directive.pattern.MatchString(instructions) {
			t.Errorf("%s does not %s:\n%s", where, directive.need, instructions)
		}
	}
}

// The prompt zot scaffolds carries the contract.
func TestTheDefaultPromptForbidsWaitingForTheUser(t *testing.T) {
	prompt := defaultPrompt(t)

	assertNonInteractive(t, "the default prompt", prompt)

	if n := strings.Count(prompt, contractHeading); n != 1 {
		t.Errorf("the contract appears %d times in the default prompt, want once", n)
	}
}

// contractHeading is how the contract is spotted in an assembled prompt.
const contractHeading = "## Non-interactive contract"

// The order's prompt is the operator's to rewrite - that is what having it in the
// file is for - but it cannot hand the agent an interactivity the run does not
// have. A prompt that forgot to say "never wait" would otherwise produce runs
// that hang on a question nobody can answer, and the operator would have no way
// to tell that from a slow model.
func TestACustomPromptKeepsTheNonInteractiveContract(t *testing.T) {
	o, err := order.Parse([]byte("---\nobjective: write a haiku\n---\nYou are a haiku bot. Write only haiku about {{ .Objective }}.\n"))
	if err != nil {
		t.Fatal(err)
	}

	got := promptOf(t, o)

	// the custom prompt really is what is sent...
	if !strings.HasPrefix(got, "You are a haiku bot. Write only haiku about write a haiku.") {
		t.Errorf("the order's own prompt was not used:\n%s", got)
	}

	if strings.Contains(got, "Your tools:") {
		t.Error("a custom prompt replaces zot's, it is not appended to it")
	}

	// ...and the contract came along anyway
	assertNonInteractive(t, "a custom prompt", got)
}

// The contract must not pile up: a prompt that carries it - the default one does,
// or one that places it with {{ .Contract }} - must not get it again.
func TestThePromptCarriesTheContractExactlyOnce(t *testing.T) {
	custom := func(body string) order.Order {
		o, err := order.Parse([]byte("---\nobjective: x\n---\n" + body))
		if err != nil {
			t.Fatal(err)
		}

		return o
	}

	for _, test := range []struct {
		name  string
		order order.Order
	}{
		{"the scaffolded prompt", newOrderNamed(t, "x")},
		{"a custom prompt", custom("Do the thing.")},
		{"a custom prompt that places the contract itself", custom("Do the thing.\n\n{{ .Contract }}\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := promptOf(t, test.order)

			assertNonInteractive(t, test.name, got)

			if n := strings.Count(got, contractHeading); n != 1 {
				t.Errorf("the contract appears %d times, want once:\n%s", n, got)
			}
		})
	}
}

// The settle and call budgets are configurable, and the config values have to
// actually reach the run - otherwise the knob in the example config is a lie.
// max_settles is the one the operator most wants: how hard zot pushes the model
// to record an outcome before giving up.
func TestRunBudgetsComeFromConfig(t *testing.T) {
	cfg := testDefaults()
	cfg.DefaultProvider = "openai"
	cfg.Providers = map[string]config.ProviderConfig{"openai": {BaseURL: "https://gw.example.com/v1", APIKey: "sk-test", Models: declared("glm-5.2")}}
	cfg.Agent.MaxSettles = 5
	cfg.Agent.MaxCalls = 33

	_, opts, err := resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if opts.MaxSettles != 5 {
		t.Errorf("MaxSettles = %d, want the configured 5", opts.MaxSettles)
	}

	if opts.MaxCalls != 33 {
		t.Errorf("MaxCalls = %d, want the configured 33", opts.MaxCalls)
	}

	// max_time is a duration string on the config, a time.Duration on the run
	cfg.Agent.MaxTime = "30m"

	_, timed, err := resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if timed.MaxDuration != 30*time.Minute {
		t.Errorf("MaxDuration = %v, want 30m", timed.MaxDuration)
	}

	// zero passes through as zero: the engine, not the config, owns the default,
	// and it never means "no settling"
	cfg.Agent.MaxSettles = 0

	_, opts, err = resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if opts.MaxSettles != 0 {
		t.Errorf("MaxSettles = %d, want the unset value left for the engine to default", opts.MaxSettles)
	}
}

// A tool result is bounded by a share of the model's own context window, so a
// model with a small window is held tighter without being told to be.
func TestToolOutputIsCappedAtAShareOfTheWindow(t *testing.T) {
	shellOutput := func(window, percent int) int {
		cfg := testDefaults()
		cfg.DefaultProvider = "openai"
		cfg.Agent.MaxToolOutputPercent = percent
		cfg.Providers = map[string]config.ProviderConfig{"openai": {
			BaseURL: "https://gw.example.com/v1", APIKey: "sk-test",
			Models: map[string]config.ModelConfig{"glm-5.2": {Context: window}},
		}}

		_, opts, err := resolve(cfg)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}

		for _, tool := range opts.Tools {
			if tool.Info().Name != tools.ShellTool {
				continue
			}

			response, err := tool.Run(context.Background(), fantasy.ToolCall{
				ID: "c", Name: tools.ShellTool, Input: `{"command":"head -c 600000 /dev/zero | tr '\\0' x"}`,
			})
			if err != nil {
				t.Fatalf("shell: %v", err)
			}

			return len(response.Content)
		}

		t.Fatal("no shell tool")

		return 0
	}

	small, large := shellOutput(8_000, 0), shellOutput(64_000, 0)

	// 25% of the window in tokens, three bytes to a token, and a marker
	if want := 8_000 / 4 * 3; small < want || small > want+100 {
		t.Errorf("a small window let through %d bytes, want about %d", small, want)
	}

	if large <= small*4 {
		t.Errorf("an eight times larger window let through %d bytes against %d: the cap does not follow the window", large, small)
	}

	if tight := shellOutput(64_000, 5); tight >= large/3 {
		t.Errorf("max_tool_output_percent 5 let through %d bytes against %d at the default", tight, large)
	}
}
