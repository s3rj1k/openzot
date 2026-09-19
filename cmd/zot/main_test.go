package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/openzot/openzot"
	"github.com/openzot/openzot/internal/config"

	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/spf13/pflag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolveOrdersLoadsEveryFile(t *testing.T) {
	first := orderFile(t, "build the parser")
	second := orderFile(t, "then the lexer")

	orders, err := resolveOrders([]string{first, second}, "")
	if err != nil {
		t.Fatalf("resolveOrders: %v", err)
	}

	if len(orders) != 2 || orders[0].Objective != "build the parser" || orders[1].Objective != "then the lexer" {
		t.Errorf("orders = %+v", orders)
	}
}

// A bad batch must fail before any run starts: discovering order three is
// broken after orders one and two have spent an hour is the expensive way.
func TestResolveOrdersFailsTheWholeBatchUpFront(t *testing.T) {
	good := orderFile(t, "fine")

	if _, err := resolveOrders([]string{good, filepath.Join(t.TempDir(), "nope.yaml")}, ""); err == nil {
		t.Error("a batch with a broken order must not resolve")
	}
}

// Someone typing prose where an order file goes is the retraining moment: the
// error has to teach the new shape, not just report a missing file.
func TestResolveOrdersTeachesProseTypers(t *testing.T) {
	_, err := resolveOrders([]string{"add a health endpoint"}, "")
	if err == nil {
		t.Fatal("prose must not resolve")
	}

	if !strings.Contains(err.Error(), "zot new") {
		t.Errorf("the error should point at `zot new`: %v", err)
	}
}

func TestResolveOrdersRequiresAnOrder(t *testing.T) {
	quietStderr(t)

	if _, err := resolveOrders(nil, ""); err == nil {
		t.Error("no order must be an error")
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

	withEditor(t, `printf 'objective: fix the typo\n' > "$1"`)

	var out strings.Builder

	if err := newOrder(nil, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.yaml"))
	if len(matches) != 1 {
		t.Fatalf("orders written = %v, want the one", matches)
	}

	if name := filepath.Base(matches[0]); !regexp.MustCompile(`^\d+\.yaml$`).MatchString(name) {
		t.Errorf("name = %q, want a unix timestamp", name)
	}

	if !strings.Contains(out.String(), matches[0]) {
		t.Errorf("the output should say where the order went and how to run it:\n%s", out.String())
	}

	orders, err := resolveOrders([]string{matches[0]}, "")
	if err != nil {
		t.Fatalf("the written order does not resolve: %v", err)
	}

	if orders[0].Objective != "fix the typo" {
		t.Errorf("objective = %q", orders[0].Objective)
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

	withEditor(t, `printf 'objective: never\n' > "$1"`)

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

// Where an order is filed and which project it is about are different
// questions: --orders-dir files it in a shared folder of briefs (or a drop box
// a watcher is pointed at) while --dir still says which project it is for.
func TestNewOrderWithOrdersDirFilesItThere(t *testing.T) {
	invocation := t.TempDir()
	project := t.TempDir()
	briefs := filepath.Join(t.TempDir(), "shared-briefs")

	t.Chdir(invocation)

	withEditor(t, `printf 'objective: fix the typo\n' > "$1"`)

	var out strings.Builder

	if err := newOrder([]string{"--dir", project, "--orders-dir", briefs}, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(briefs, "*.yaml"))
	if len(matches) != 1 {
		t.Fatalf("orders in --orders-dir = %v, want the one", matches)
	}

	// neither the project's book nor the invoking directory is touched
	for _, untouched := range []string{project, invocation} {
		if _, err := os.Stat(filepath.Join(untouched, order.BookDir)); !os.IsNotExist(err) {
			t.Errorf("--orders-dir must be the only place written; %s has a book: %v", untouched, err)
		}
	}
}

// `zot new --dir` creates the order in another working directory, not the one
// the command was invoked from - the order belongs to the project it is for.
func TestNewOrderWithDirCreatesItInThatDirectory(t *testing.T) {
	invocation := t.TempDir()
	target := t.TempDir()

	t.Chdir(invocation)

	withEditor(t, `printf 'objective: fix the typo\n' > "$1"`)

	if err := newOrder([]string{"--dir", target}, io.Discard); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	if _, err := os.Stat(filepath.Join(invocation, order.BookDir)); !os.IsNotExist(err) {
		t.Errorf("the invoking directory must stay untouched: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(target, order.BookDir, "orders", "*.yaml"))
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

	if matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.yaml")); len(matches) != 0 {
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

	withEditor(t, `printf 'objective: half written\n' > "$1"; exit 3`)

	if err := newOrder(nil, io.Discard); err == nil {
		t.Fatal("an editor that fails must be reported")
	}

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.yaml"))
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

	if matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.yaml")); len(matches) != 1 {
		t.Errorf("orders = %v, want the blank order left to be edited", matches)
	}
}

func orderFile(t *testing.T, objective string) string {
	t.Helper()

	return orderFileIn(t, t.TempDir(), "order.yaml", objective)
}

// orderFileIn writes an order with the given objective to dir/name.
func orderFileIn(t *testing.T, dir, name, objective string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte("objective: "+fmt.Sprintf("%q", objective)+"\n"), 0o644); err != nil {
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

	for _, want := range []string{"zot [flags] [<order.yaml>", "zot new", "zot config", "--dir", ".jsonl"} {
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
	// their orders went - and that it is theirs to point elsewhere.
	for _, want := range []string{order.BookDir + "/orders", "--orders-dir"} {
		if !strings.Contains(text, want) {
			t.Errorf("usage does not describe %q:\n%s", want, text)
		}
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
// order paths: `zot orders/a.yaml --plain` parses --plain as a flag and keeps
// the paths intact. The stdlib flag package stopped at the first non-flag,
// folding --plain into the positionals - this locks the behaviour that
// motivated the switch.
func TestFlagsAfterThePositionalOrdersAreParsed(t *testing.T) {

	set := pflag.NewFlagSet("zot", pflag.ContinueOnError)
	plain := set.Bool("plain", false, "")

	if err := set.Parse([]string{"do", "the", "thing", "--plain"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !*plain {
		t.Error("--plain given after the task must be parsed as a flag, not swallowed into it")
	}

	if got := strings.Join(set.Args(), " "); got != "do the thing" {
		t.Errorf("positional task = %q, want the words before the flag", got)
	}
}

// Command-line values win over the file and the environment - but a boolean that
// was never passed must not overwrite one the config enabled.
func TestApplyOverrides(t *testing.T) {
	base := func() zot.Config {
		cfg := config.Defaults()
		cfg.UI.Plain = true

		return cfg
	}

	t.Run("scalars override when set", func(t *testing.T) {
		cfg := base()

		applyOverrides(&cfg, overrides{
			Provider:      "groq",
			Model:         "glm-5.2",
			MaxIterations: 12,
			Color:         "always",
		})

		if cfg.DefaultProvider != "groq" {
			t.Errorf("provider = %q", cfg.DefaultProvider)
		}

		if cfg.Agent.Model != "glm-5.2" {
			t.Errorf("model = %q", cfg.Agent.Model)
		}

		if cfg.Agent.MaxIterations != 12 {
			t.Errorf("max iterations = %d", cfg.Agent.MaxIterations)
		}

		if cfg.UI.Color != "always" {
			t.Errorf("color = %q", cfg.UI.Color)
		}
	})

	t.Run("empty scalars leave the config alone", func(t *testing.T) {
		cfg := base()

		before := cfg.Agent.Model

		applyOverrides(&cfg, overrides{})

		if cfg.Agent.Model != before {
			t.Errorf("model = %q, want %q untouched", cfg.Agent.Model, before)
		}

		if cfg.Agent.MaxIterations <= 0 {
			t.Error("a zero max-iterations must not clear the configured value")
		}
	})

	t.Run("an unpassed boolean does not turn a configured one off", func(t *testing.T) {
		cfg := base()

		applyOverrides(&cfg, overrides{Plain: false})

		if !cfg.UI.Plain {
			t.Error("--plain was never passed; the configured value must stand")
		}
	})

	t.Run("a passed boolean does override", func(t *testing.T) {
		cfg := base()

		applyOverrides(&cfg, overrides{
			Plain:  false,
			Passed: map[string]bool{"plain": true},
		})

		if cfg.UI.Plain {
			t.Error("--plain=false was passed and must win")
		}
	})
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
// can be exercised the way the shell invokes it.
func withArgs(t *testing.T, args ...string) {
	t.Helper()

	originalArgs := os.Args
	originalFlags := pflag.CommandLine

	os.Args = append([]string{"zot"}, args...)
	pflag.CommandLine = pflag.NewFlagSet("zot", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)

	t.Cleanup(func() {
		os.Args = originalArgs
		pflag.CommandLine = originalFlags
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

ui:
  plain: true

default_provider: local

providers:
  local:
    driver: openai
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

	if err := os.MkdirAll(filepath.Join(target, ".skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(target, ".skills", "deploy", "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: DEPLOYMENT-SKILL-MARKER\n---\nDeploy carefully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// every path on the command line is relative to the invoking directory -
	// none of them exist inside --dir, so they must resolve before the chdir
	if err := os.WriteFile("order.yaml", []byte("objective: do the thing\n"), 0o644); err != nil {
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

		if strings.Contains(string(body), "DEPLOYMENT-SKILL-MARKER") {
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
ui:
  plain: true
default_provider: local
providers:
  local:
    driver: openai
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", "config.yaml", "--dir", target, "order.yaml")

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
		t.Errorf("project context did not come from --dir (AGENTS.md seen: %v, skills seen: %v)",
			sawContext.Load(), sawSkill.Load())
	}

	// the log lands in the project being worked on, named after the order
	records := readLog(t, filepath.Join(target, ".zot", "orders", "order.jsonl"))

	if records[0].Meta == nil || records[0].Meta.Workdir != target {
		t.Errorf("meta = %+v, want the absolute --dir %q as workdir", records[0].Meta, target)
	}
}

// A batch is N independent runs: each order gets its own session and its own
// recorded outcome, and the batch stops at the first order that does not end in
// success - later orders usually assume the earlier ones landed.
func TestRunABatchOfOrders(t *testing.T) {
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
ui:
  plain: true
default_provider: local
providers:
  local:
    driver: openai
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

	t.Run("every order gets its own run and log", func(t *testing.T) {
		project := t.TempDir()

		server := settle("success", `{"summary":"complete"}`)
		defer server.Close()

		orders := t.TempDir()

		withArgs(t, "--config", configFor(t, server.URL), "--dir", project,
			orderFileIn(t, orders, "first.yaml", "the first order"),
			orderFileIn(t, orders, "second.yaml", "the second order"))

		if _, err := captureStdout(t, run); err != nil {
			t.Fatalf("run: %v", err)
		}

		// each log carries its own order's objective, not a blend of the batch
		for name, want := range map[string]string{"first.jsonl": "the first order", "second.jsonl": "the second order"} {
			records := readLog(t, filepath.Join(project, ".zot", "orders", name))

			if records[0].Meta == nil || records[0].Meta.Task != want {
				t.Errorf("%s opens with %+v, want the task %q", name, records[0], want)
			}
		}
	})

	t.Run("the batch stops at the first failed order", func(t *testing.T) {
		project := t.TempDir()

		server := settle("failure", `{"reason":"cannot"}`)
		defer server.Close()

		orders := t.TempDir()

		first := orderFileIn(t, orders, "doomed.yaml", "the doomed order")

		withArgs(t, "--config", configFor(t, server.URL), "--dir", project,
			first, orderFileIn(t, orders, "never.yaml", "the never-run order"))

		var err error

		quietStderr(t)

		if _, err = captureStdout(t, run); err == nil {
			t.Fatal("a failed order must fail the batch")
		}

		if !strings.Contains(err.Error(), first) {
			t.Errorf("the error should name the order that stopped the batch: %v", err)
		}

		logs, listErr := os.ReadDir(filepath.Join(project, ".zot", "orders"))
		if listErr != nil {
			t.Fatalf("ReadDir: %v", listErr)
		}

		if len(logs) != 1 || logs[0].Name() != "doomed.jsonl" {
			t.Fatalf("got %d logs - the second order must never have run", len(logs))
		}
	})
}

// An explicitly passed --max-iterations is the operator's last word. A per-model
// max_iterations is applied when the run resolves - after the command line has
// been layered into the config - so the file used to quietly win: `zot
// --max-iterations 4` against a model capped at 1 stopped after a single
// iteration. Counting provider calls is the only honest way to ask which limit
// the engine enforced.
func TestExplicitMaxIterationsBeatsAPerModelCap(t *testing.T) {
	t.Chdir(t.TempDir())

	var requests atomic.Int32

	// never finishes on its own: every turn is a non-terminal tool call, so the
	// run ends only when it runs out of iterations
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		step := requests.Add(1)

		fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c%d","type":"function","function":{"name":"tasks","arguments":"{\"tasks\":[{\"title\":\"step %d\",\"status\":\"in_progress\"}]}"}}]},"finish_reason":"tool_calls"}]}`,
			step, step))

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
agent:
  model: capped
  max_iterations: 9
ui:
  plain: true
default_provider: local
providers:
  local:
    driver: openai
    base_url: %s
    api_key: test-key
    models:
      capped:
        model: test-model
        max_iterations: 1
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, "--dir", t.TempDir(), "--max-iterations", "4", orderFile(t, "a task"))

	// exhausting the iteration budget is how this run ends, so the error is the
	// expected outcome - what matters is the budget it exhausted
	_, err := captureStdout(t, run)
	if err == nil {
		t.Fatal("a run that never records an outcome must end on its iteration cap")
	}

	if !strings.Contains(err.Error(), "stopped after 4 iterations") {
		t.Errorf("run stopped with %v, want the command-line cap of 4 to be the one enforced", err)
	}

	if got := requests.Load(); got != 4 {
		t.Errorf("the run made %d provider calls, want the 4 the command line allowed", got)
	}
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

	// a provider that names no known driver and has no endpoint
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

ui:
  plain: true

default_provider: local

providers:
  local:
    driver: openai
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)

	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	orderPath := orderFileIn(t, t.TempDir(), "1758300000.yaml", "the first task")

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
ui:
  plain: true
default_provider: local
providers:
  local:
    driver: openai
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
// The real tool handler answers each call, and the plain transcript - what a
// piped run leaves behind - carries the checklist as the model sent it.
func TestARunsTaskListReachesThePlainTranscript(t *testing.T) {
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
ui:
  plain: true
default_provider: local
providers:
  local:
    driver: openai
    base_url: %s
    api_key: test-key
    models:
      test-model:
        context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, "--dir", t.TempDir(), orderFile(t, "fix the lexer"))

	transcript, err := captureStdout(t, run)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, want := range []string{"1/3 done", "[x] read the parser", "[>] fix the lexer - off by one", "[ ] add a test"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the transcript is missing %q:\n%s", want, transcript)
		}
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
			body: "title: Rate limiting\nobjective: add rate limiting to the api\n",
			want: "Rate limiting",
		},
		{
			name: "otherwise the file name",
			body: "objective: add rate limiting to the api\n",
			want: "Fix the flaky test", // from fix-the-flaky-test.yaml
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orderPath := filepath.Join(t.TempDir(), "fix-the-flaky-test.yaml")

			if err := os.WriteFile(orderPath, []byte(test.body), 0o644); err != nil {
				t.Fatal(err)
			}

			loaded, err := order.Load(orderPath)
			if err != nil {
				t.Fatal(err)
			}

			var got zot.RunOptions

			runs := oneRun{
				ctx:  context.Background(),
				logs: t.TempDir(),
				run: func(_ context.Context, _ zot.Config, _ string, options zot.RunOptions) error {
					got = options

					return nil
				},
			}

			if err := runs.execute(loaded, true); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if got.Title != test.want {
				t.Errorf("viewer title = %q, want %q", got.Title, test.want)
			}
		})
	}
}

// A batch tells each run where it sits in the queue, so the viewer can report
// how much of the queue is left rather than only how much of one order is.
func TestABatchRunKnowsItsPosition(t *testing.T) {
	dir := t.TempDir()

	var orders []order.Order

	for _, name := range []string{"a-first.yaml", "b-second.yaml", "c-third.yaml"} {
		path := filepath.Join(dir, name)

		if err := os.WriteFile(path, []byte("objective: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		loaded, err := order.Load(path)
		if err != nil {
			t.Fatal(err)
		}

		orders = append(orders, loaded)
	}

	var seen []zot.RunOptions

	runs := oneRun{
		ctx:  context.Background(),
		logs: t.TempDir(),
		run: func(_ context.Context, _ zot.Config, _ string, options zot.RunOptions) error {
			seen = append(seen, options)

			return nil
		},
	}

	for i, o := range orders {
		if err := runs.executeAt(o, i < len(orders)-1, i+1, len(orders)); err != nil {
			t.Fatalf("executeAt: %v", err)
		}
	}

	for i, options := range seen {
		if options.BatchIndex != i+1 || options.BatchSize != len(orders) {
			t.Errorf("order %d reported position %d/%d, want %d/%d",
				i+1, options.BatchIndex, options.BatchSize, i+1, len(orders))
		}
	}

	// a lone order is not a batch, and must not claim to be 1 of 1
	var solo zot.RunOptions

	runs.run = func(_ context.Context, _ zot.Config, _ string, options zot.RunOptions) error {
		solo = options

		return nil
	}

	if err := runs.execute(orders[0], false); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if solo.BatchSize != 0 {
		t.Errorf("a single order reported a batch size of %d, want none", solo.BatchSize)
	}
}

func TestABareInvocationRunsTheBook(t *testing.T) {
	project := t.TempDir()

	book := order.OrdersDir(project)

	if err := os.MkdirAll(book, 0o755); err != nil {
		t.Fatal(err)
	}

	// written out of order, and with a file that is not an order beside them
	for name, objective := range map[string]string{
		"b-second.yaml": "the second job",
		"a-first.yaml":  "the first job",
	} {
		if err := os.WriteFile(filepath.Join(book, name), []byte("objective: "+objective+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(book, "notes.txt"), []byte("not an order\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(book, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}

	announced, err := captureStderr(t, func() error {
		orders, err := resolveOrders(nil, book)
		if err != nil {
			return err
		}

		if len(orders) != 2 {
			t.Errorf("a bare invocation resolved %d orders, want the book's 2", len(orders))

			return nil
		}

		// filename order, so a batch is deterministic and can be reasoned about
		if orders[0].Objective != "the first job" || orders[1].Objective != "the second job" {
			t.Errorf("orders = %q, %q - want them in filename order",
				orders[0].Objective, orders[1].Objective)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("a bare invocation in a project with a book must run it: %v", err)
	}

	// silently running work nobody named would be worse than not running it
	if !strings.Contains(announced, book) {
		t.Errorf("a bare invocation must say what it is about to run:\n%s", announced)
	}
}

// With no book and nothing named there is no work to infer, so zot says how to
// make some rather than exiting quietly or guessing.
func TestABareInvocationWithNoBookExplainsItself(t *testing.T) {
	quietStderr(t)

	empty := t.TempDir()

	for _, ordersRoot := range []string{filepath.Join(empty, "never-created"), empty, ""} {
		_, err := resolveOrders(nil, ordersRoot)
		if err == nil {
			t.Fatalf("an empty book (%q) must not resolve to a silent no-op", ordersRoot)
		}

		if !strings.Contains(err.Error(), "zot new") {
			t.Errorf("the error should say how to write an order: %v", err)
		}
	}
}
