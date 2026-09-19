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

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/agent"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/tui"
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
// questions: --orders-dir files it in a shared folder of briefs while --dir
// still says which project it is for.
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
// order paths: `zot orders/a.yaml --dir proj` parses --dir as a flag and keeps
// the paths intact. The stdlib flag package stopped at the first non-flag,
// folding the flag into the positionals - this locks the behaviour that
// motivated the switch.
func TestFlagsAfterThePositionalOrdersAreParsed(t *testing.T) {
	set := pflag.NewFlagSet("zot", pflag.ContinueOnError)
	dir := set.String("dir", ".", "")

	if err := set.Parse([]string{"a.yaml", "b.yaml", "--dir", "proj"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if *dir != "proj" {
		t.Errorf("--dir given after the orders = %q, want it parsed as a flag", *dir)
	}

	if got := strings.Join(set.Args(), " "); got != "a.yaml b.yaml" {
		t.Errorf("positional orders = %q, want the paths before the flag", got)
	}
}

// Everything the config can say, the config alone says: a flag that duplicated a
// key would be a second place to look for what a run was told.
func TestConfigKeysAreNotFlags(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "missing.yaml"), orderFile(t, "a task"))

	_, _ = captureStderr(t, func() error { return run() })

	for _, name := range []string{"provider", "model", "max-iterations", "plain", "color"} {
		if pflag.CommandLine.Lookup(name) != nil {
			t.Errorf("--%s is a flag, but the config already says it", name)
		}
	}

	for _, name := range []string{"config", "dir", "orders-dir"} {
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

			var got runOptions

			runs := oneRun{
				ctx:  context.Background(),
				logs: t.TempDir(),
				run: func(_ context.Context, _ config.Config, _ string, options runOptions) error {
					got = options

					return nil
				},
			}

			if err := runs.executeAt(loaded, true, 0, 0); err != nil {
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

	var seen []runOptions

	runs := oneRun{
		ctx:  context.Background(),
		logs: t.TempDir(),
		run: func(_ context.Context, _ config.Config, _ string, options runOptions) error {
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
	var solo runOptions

	runs.run = func(_ context.Context, _ config.Config, _ string, options runOptions) error {
		solo = options

		return nil
	}

	if err := runs.executeAt(orders[0], false, 0, 0); err != nil {
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

func TestLoadProjectContext(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	// A global AGENTS.md in the config dir and a project one in the work dir.
	mustWrite(t, filepath.Join(configDir, "AGENTS.md"), "GLOBAL CONVENTIONS")
	mustWrite(t, filepath.Join(workDir, "AGENTS.md"), "PROJECT CONVENTIONS")

	// A skill in each location: plain "skills/" in the config dir and hidden
	// ".skills/" in the project dir - both layouts must be picked up.
	mustWrite(t, filepath.Join(configDir, "skills", "greet", "SKILL.md"),
		"---\nname: greet\ndescription: say hello\n---\nbody")
	mustWrite(t, filepath.Join(workDir, ".skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: ship it\n---\nbody")

	cfg := config.Config{}
	if err := loadProjectContext(&cfg, configDir, workDir); err != nil {
		t.Fatalf("LoadProjectContext: %v", err)
	}

	// Instructions keeps the default and appends both AGENTS.md files in order.
	for _, want := range []string{defaultInstructions[:20], "GLOBAL CONVENTIONS", "PROJECT CONVENTIONS"} {
		if !strings.Contains(cfg.Agent.Instructions, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
	if i, j := strings.Index(cfg.Agent.Instructions, "GLOBAL"), strings.Index(cfg.Agent.Instructions, "PROJECT"); i > j {
		t.Error("expected config-dir AGENTS.md to appear before work-dir AGENTS.md")
	}

	// Both skills' directories are recorded; the run rescans them live rather
	// than snapshotting the skills at load. Load them the same way the run
	// does and confirm both are found.
	loaded, err := agent.LoadSkills(cfg.SkillDirectories)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(loaded.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d (%v)", len(loaded.Skills), loaded.Skills)
	}

	names := map[string]bool{}
	for _, skill := range loaded.Skills {
		names[skill.Name] = true

		if skill.Path == "" {
			t.Errorf("skill %q has no path for the model to read", skill.Name)
		}
	}
}

func TestLoadProjectContextNoFiles(t *testing.T) {
	cfg := config.Config{}
	if err := loadProjectContext(&cfg, t.TempDir()); err != nil {
		t.Fatalf("LoadProjectContext: %v", err)
	}
	if cfg.Agent.Instructions != "" {
		t.Error("expected instructions untouched when no AGENTS.md is present")
	}

	// Candidate skill directories are recorded even when empty - the run
	// watches them live, so a skill added later is still found - but no skill
	// resolves from them yet.
	loaded, err := agent.LoadSkills(cfg.SkillDirectories)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(loaded.Skills) != 0 {
		t.Error("expected no skills when none are present")
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
ui:
  plain: true
default_provider: myprovider
providers:
  myprovider:
    driver: openai
    base_url: %s
%s`, test.model, server.URL, test.config))

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			client, _, err := resolve(cfg, defaultInstructions)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			if got := client.Model(); got != "gpt-4" {
				t.Errorf("model = %q", got)
			}

			if _, err := quietly(t, func() error {
				return runTask(context.Background(), cfg, "do the thing", runOptions{})
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
ui:
  plain: true
default_provider: selfhosted
providers:
  selfhosted:
    driver: openai
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
				return runTask(context.Background(), cfg, "do the thing", runOptions{})
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

	_, _, err := resolve(cfg, defaultInstructions)
	if err == nil {
		t.Fatal("a provider naming no endpoint must be rejected")
	}

	// the error has to be actionable: it names the field to set
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error = %q, want it to name what is missing", err)
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

			_, _, err := resolve(cfg, defaultInstructions)
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
    driver: openai
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

		client, _, err := resolve(cfg, defaultInstructions)
		if err != nil {
			t.Fatalf("resolve(%s): %v", name, err)
		}

		if got := client.Model(); got != "some-model" {
			t.Errorf("%s model = %q, want it unchanged", name, got)
		}

		if got := client.Provider(); got != name {
			t.Errorf("%s provider = %q, want %q", name, got, name)
		}

		if got := client.Driver(); got != agent.DriverOpenAI {
			t.Errorf("%s driver = %q, want %q", name, got, agent.DriverOpenAI)
		}

		if got := client.BaseURL(); got != wantURL {
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

		if _, _, err := resolve(cfg, defaultInstructions); err == nil {
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
    driver: openai
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
	client, opts, err := resolve(cfg, defaultInstructions)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := client.Model(); got != "gpt-5" {
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

	_, opts, err := resolve(cfg, defaultInstructions)
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

	_, opts, err = resolve(cfg, defaultInstructions)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := viewerMeta(cfg, "a task", "/somewhere", opts).MaxIterations; got != 0 {
		t.Errorf("the viewer shows a limit of %d, want the backstop hidden", got)
	}
}

// runTask is the whole thing end to end: config in, a provider call out, a
// transcript back. With ui.plain set it takes the non-TTY path, which is what CI
// uses and what can be asserted on.
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
	cfg.UI.Plain = true
	cfg.DefaultProvider = "local"
	cfg.Providers = map[string]config.ProviderConfig{
		"local": {Driver: "openai", BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")},
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

	err := runTask(context.Background(), cfg, "do the thing", runOptions{})

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

	err := runTask(context.Background(), cfg, "task", runOptions{})

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
	cfg.UI.Plain = true
	cfg.DefaultProvider = "local"
	cfg.Providers = map[string]config.ProviderConfig{
		"local": {Driver: "openai", BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")},
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
		return runTask(context.Background(), cfg, "do the thing", runOptions{SessionPath: path})
	}); err != nil {
		t.Fatalf("RunWith: %v", err)
	}

	records := readSession(t, path)

	first, last := records[0], records[len(records)-1]

	if first.Kind != session.KindMeta || first.Meta == nil {
		t.Fatalf("the log must open with the meta: %+v", first)
	}

	if first.Meta.Task != "do the thing" || first.Meta.Provider != "local" || first.Meta.Driver != "openai" {
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
			return runTask(context.Background(), cfg, "the same brief", runOptions{SessionPath: path})
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
	cfg.Providers["local"] = config.ProviderConfig{Driver: "openai", BaseURL: server.URL, APIKey: "k", Models: declared("glm-5.2")}

	if output, err := quietly(t, func() error {
		return runTask(context.Background(), cfg, "do the thing", runOptions{SessionPath: path})
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
	summary := &agent.Summary{Reason: "success", Iterations: 1}

	var recorded, unrecorded strings.Builder

	printDigest(&recorded, "/w/.zot/orders/1758300000.jsonl", tui.Outcome{}, summary)
	printDigest(&unrecorded, "", tui.Outcome{}, summary)

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
		return runTask(context.Background(), cfg, "do the thing", runOptions{
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
		return runTask(context.Background(), cfg, "do the thing", runOptions{})
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
	err = runTask(context.Background(), cfg, "task", runOptions{})
	if err == nil || !strings.Contains(err.Error(), "providers:") {
		t.Errorf("Run = %v, want it to say to declare a provider", err)
	}
}

// The task is the durable objective, so it must land in the instructions (the
// system prompt), which trimming never drops and always orders first -
// not as a user message, which a long run can trim away. An agent that
// forgets its own objective is the worst way for a run to fail.
func TestTaskGoesIntoTheInstructions(t *testing.T) {
	got := withTask("SYSTEM PROMPT", "  build a parser  ")

	if !strings.Contains(got, "SYSTEM PROMPT") {
		t.Error("the base instructions must be preserved")
	}

	if !strings.Contains(got, "build a parser") {
		t.Errorf("the task must be in the instructions: %q", got)
	}

	// trimmed, and clearly the task rather than run together with the prompt
	if strings.Contains(got, "  build a parser  ") {
		t.Error("the task should be trimmed")
	}

	// an empty task leaves the instructions untouched
	if withTask("SYSTEM PROMPT", "   ") != "SYSTEM PROMPT" {
		t.Error("an empty task must not alter the instructions")
	}
}

// The instructions drifted once already - it told the agent to call "edit",
// "exec", "exit" and "progress" when those tools did not exist. This pins it:
// every tool the default instructions names in quotes must be a real tool the
// agent is actually given, or a terminal tool the engine adds in settle mode.
func TestDefaultInstructionsNamesOnlyRealTools(t *testing.T) {
	real := map[string]bool{
		// the terminal tools the loop injects in settle mode
		"success": true,
		"failure": true,
	}

	for name := range agent.DefaultTools() {
		real[name] = true
	}

	// pull every "quoted" token out of the instructions and check the tool-looking
	// ones are real
	for _, quoted := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(defaultInstructions, -1) {
		name := quoted[1]

		// only check things that look like tool names (a real tool, or the
		// phantom ones we are guarding against)
		phantom := map[string]bool{"edit": true, "exec": true, "exit": true, "abort": true, "read": true, "write": true, "list": true, "plan": true, "progress": true}

		if !real[name] && phantom[name] {
			t.Errorf("the instructions names %q, which is not a real tool", name)
		}
	}

	// and positively assert the tools the instructions promises are all present
	for _, want := range []string{"tasks", "shell", "success", "failure"} {
		if !real[want] {
			t.Errorf("the instructions relies on %q but it is not a real tool", want)
		}

		if !strings.Contains(defaultInstructions, `"`+want+`"`) {
			t.Errorf("the instructions should name the %q tool so the model knows to use it", want)
		}
	}
}

// With shell the only tool that touches the machine, the model has to be told so
// and shown how to read, list and write with it. A prompt that only said "shell"
// would leave a model reaching for file tools it does not have.
func TestDefaultInstructionsTeachShellAsTheOnlyWayToTouchTheMachine(t *testing.T) {
	for _, want := range []string{
		"only way to act on the machine",
		"cat", "sed -n", "grep -n", "ls", "heredoc",

		// writing a file through the shell is where an unquoted heredoc mangles
		// what was written, so the rule that prevents it is part of the prompt
		"quoted heredoc",
	} {
		if !strings.Contains(defaultInstructions, want) {
			t.Errorf("the instructions should mention %q so the model knows how to work through shell", want)
		}
	}
}

// The tasks tool only helps if the model keeps it current, and the prompt is the
// only thing that says how: each status it may use, and that a blocker or an
// assumption belongs in a note.
func TestDefaultInstructionsTeachHowToKeepTheTasksCurrent(t *testing.T) {
	for _, want := range []string{"in_progress", "done", "blocked", "note", "whole list"} {
		if !strings.Contains(defaultInstructions, want) {
			t.Errorf("the instructions should mention %q so the model keeps its tasks current", want)
		}
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

// The built-in prompt carries the contract.
func TestDefaultInstructionsForbidWaitingForTheUser(t *testing.T) {
	assertNonInteractive(t, "defaultInstructions", defaultInstructions)

	if n := strings.Count(defaultInstructions, contractHeading); n != 1 {
		t.Errorf("the contract appears %d times in the default prompt, want once", n)
	}
}

// contractHeading is how the contract is spotted in an assembled prompt.
const contractHeading = "## Non-interactive contract"

// Overriding the instructions replaces zot's prompt - that is what an override
// is for - but it cannot hand the agent an interactivity the run does not have.
// A custom prompt that forgot to say "never wait" would otherwise produce runs
// that hang on a question nobody can answer, and the operator would have no way
// to tell that from a slow model.
func TestCustomInstructionsKeepTheNonInteractiveContract(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, `
agent:
  model: test-model
default_provider: local
providers:
  local:
    driver: openai
    base_url: http://127.0.0.1:1
    api_key: test-key
    models:
      test-model:
        context: 100000
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cfg.Agent.Instructions = "You are a haiku bot. Write only haiku.\n"

	_, opts, err := resolve(cfg, defaultInstructions)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// the override really did replace the built-in prompt...
	if !strings.Contains(opts.Instructions, "haiku bot") {
		t.Errorf("the custom instructions were not used:\n%s", opts.Instructions)
	}

	if strings.Contains(opts.Instructions, "Your tools:") {
		t.Error("an override should replace the built-in prompt, not be appended to it")
	}

	// ...and the contract came along anyway
	assertNonInteractive(t, "custom instructions", opts.Instructions)
}

// The contract must not pile up. The default prompt already carries it, and so
// does a default prompt that LoadProjectContext extended with an AGENTS.md;
// re-attaching it there would spend context repeating the same paragraph.
func TestResolvedInstructionsCarryTheContractExactlyOnce(t *testing.T) {
	configPath := writeCfg(t, `
agent:
  model: test-model
default_provider: local
providers:
  local:
    driver: openai
    base_url: http://127.0.0.1:1
    api_key: test-key
    models:
      test-model:
        context: 100000
`)

	project := t.TempDir()

	mustWrite(t, filepath.Join(project, "AGENTS.md"), "PROJECT CONVENTIONS")

	for _, test := range []struct {
		name    string
		prepare func(*config.Config)
	}{
		{"the built-in prompt", func(*config.Config) {}},
		{"the built-in prompt plus AGENTS.md", func(cfg *config.Config) {
			if err := loadProjectContext(cfg, project); err != nil {
				t.Fatalf("LoadProjectContext: %v", err)
			}
		}},
		{"a custom prompt", func(cfg *config.Config) { cfg.Agent.Instructions = "Do the thing." }},
		{"a custom prompt that already quotes the contract", func(cfg *config.Config) {
			cfg.Agent.Instructions = "Do the thing.\n\n" + nonInteractiveContract
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			test.prepare(&cfg)

			_, opts, err := resolve(cfg, defaultInstructions)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			assertNonInteractive(t, test.name, opts.Instructions)

			if n := strings.Count(opts.Instructions, contractHeading); n != 1 {
				t.Errorf("the contract appears %d times, want once:\n%s", n, opts.Instructions)
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

	_, opts, err := resolve(cfg, defaultInstructions)
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

	_, timed, err := resolve(cfg, defaultInstructions)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if timed.MaxDuration != 30*time.Minute {
		t.Errorf("MaxDuration = %v, want 30m", timed.MaxDuration)
	}

	// zero stays zero, so the agent layer falls back to its built-in default
	// rather than pinning the budget to zero
	cfg.Agent.MaxSettles = 0

	_, opts, err = resolve(cfg, defaultInstructions)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if opts.MaxSettles != 0 {
		t.Errorf("MaxSettles = %d, want 0 (the sentinel for 'use the default')", opts.MaxSettles)
	}
}

// A single order must hold its final screen for review: QuitOnDone defaults
// off, and only the batch loop switches it on for intermediate orders.
func TestQuitOnDoneDefaultsOff(t *testing.T) {
	cfg := stubProvider(t)

	_, opts, err := resolve(cfg, defaultInstructions)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if viewerMeta(cfg, "task", "/w", opts).QuitOnDone {
		t.Fatal("QuitOnDone must default off - a single order holds its screen for review")
	}
}
