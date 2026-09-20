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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/tui"
)

// headlessViewer is tui.Run without the screen. It reports endings the way the
// viewer does. An error behind the run as itself, otherwise an agent-declared
// failure as an AgentExitError.
func headlessViewer(ctx context.Context, meta tui.Meta, opts *loop.Options) (loop.Result, error) {
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

// TestMain gives every test a stand-in for the terminal and the full-screen
// viewer, which need a real TTY. The stand-in runs the agent to its ending and
// prints what it said, so a test can assert on the run without a screen.
func TestMain(m *testing.M) {
	isTerminal = func() bool { return true }
	runViewer = headlessViewer

	os.Exit(m.Run())
}

// orderText is an order file with the given goal and the smallest prompt
// that uses it. The contract is not in it, because the run supplies that.
func orderText(objective string) string {
	return "---\nobjective: " + fmt.Sprintf("%q", objective) + "\n---\n{{ .Objective }}\n"
}

// orderFileIn writes an order with the given goal to dir/name.
func orderFileIn(t *testing.T, dir, name, objective string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(orderText(objective)), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func orderFile(t *testing.T, objective string) string {
	t.Helper()

	return orderFileIn(t, t.TempDir(), "order.md", objective)
}

// withArgs runs a function with a fresh flag set and the given argv, so command()
// can be exercised the way the shell invokes it. It chdirs into --dir, so the
// working directory is put back afterwards. A later test must not inherit a
// temp directory that is already gone.
func withArgs(t *testing.T, args ...string) {
	t.Helper()

	originalArgs := os.Args
	originalFlags := pflag.CommandLine

	// command chdirs into --dir. This registers the return to where the test began
	t.Chdir(".")

	os.Args = append([]string{"zot"}, args...)
	pflag.CommandLine = pflag.NewFlagSet("zot", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)

	t.Cleanup(func() {
		os.Args = originalArgs
		pflag.CommandLine = originalFlags
	})
}

// With no terminal there is nothing to show a run in, so zot rejects before it
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
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, litDir, t.TempDir(), orderFile(t, "a task"))

	err := command()
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

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
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

// Someone typing prose where an order file goes is the retraining moment. The
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

// quietStderr silences stderr for a test that by design triggers the usage
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

// One order per invocation. None is told how to make one, several are told to
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

// readLog decodes every line of a session log, failing on any line that is not
// a JSON record.
func readLog(t *testing.T, path string) []session.Record {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	records := make([]session.Record, 0, len(lines))

	for i, line := range lines {
		var record session.Record

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not a JSON record: %v\n%s", i+1, err, line)
		}

		records = append(records, record)
	}

	return records
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

	for _, want := range []string{"zot [flags] <order.md>", "zot new", "zot config", litDir, ".jsonl"} {
		if !strings.Contains(text, want) {
			t.Errorf("usage does not mention %q:\n%s", want, text)
		}
	}

	// --dir belongs to both shapes. Where a run works, and where `zot new`
	// scaffolds - someone standing outside the project needs it either way
	if n := strings.Count(text, litDir); n < 2 {
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

	// ACP is gone. Zot runs unattended and has no protocol server
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
// order paths. `zot orders/a.md --dir proj` parses --dir as a flag and keeps
// the paths intact. The stdlib flag package stopped at the first non-flag,
// folding the flag into the positionals - this locks the behavior that
// motivated the switch.
func TestFlagsAfterThePositionalOrdersAreParsed(t *testing.T) {
	set := pflag.NewFlagSet("zot", pflag.ContinueOnError)
	dir := set.String("dir", ".", "")

	if err := set.Parse([]string{"a.md", "b.md", litDir, "proj"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if *dir != "proj" {
		t.Errorf("--dir given after the orders = %q, want it parsed as a flag", *dir)
	}

	if got := strings.Join(set.Args(), " "); got != "a.md b.md" {
		t.Errorf("positional orders = %q, want the paths before the flag", got)
	}
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

// captureStderr collects what a function prints to stderr. Stdout and stderr are
// worth telling apart. Stdout is the transcript, stderr is where zot talks about
// itself, and something that belongs on one must not leak onto the other.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	return capture(t, &os.Stderr, fn)
}

// Everything the config can say, the config alone says. A flag that duplicated a
// key would be a second place to look for what a run was told.
func TestConfigKeysAreNotFlags(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "missing.yaml"), orderFile(t, "a task"))

	_, _ = captureStderr(t, command)

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

// captureStdout collects what a function prints to stdout.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	return capture(t, &os.Stdout, fn)
}

func TestRunConfigPath(t *testing.T) {
	t.Setenv("ZOT_CONFIG", "/some/where/config.yaml")

	withArgs(t, "config", "path")

	output, err := captureStdout(t, command)
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

	if err := command(); err == nil {
		t.Error("running with no task must be an error")
	}
}

// The whole path. Argv in, config resolved, provider called, transcript out.
func TestRunEndToEnd(t *testing.T) {
	// the run's log lands under --dir, which defaults to here. Keep it out of the
	// source tree
	t.Chdir(t.TempDir())

	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		frames := [][]string{
			{
				`{"choices":[{"delta":{"content":"on it"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			},
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


provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)

	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, litDir, workdir, orderFile(t, "do the thing"))

	output, err := captureStdout(t, command)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, output)
	}

	for _, want := range []string{"do the thing", "on it", "complete"} {
		if !strings.Contains(output, want) {
			t.Errorf("transcript is missing %q:\n%s", want, output)
		}
	}
}

// contractHeading is how the contract is spotted in an assembled prompt.
const contractHeading = "## Non-interactive contract"

// The whole loop of the new order. Zot new scaffolds the file with the full
// prompt in it, the operator writes the goal, and what the model is sent is
// that prompt rendered - the goal, the tools the run really has, where it is
// working, and the project's AGENTS.md, with the contract once.
func TestAScaffoldedOrderRunsWithItsFullPrompt(t *testing.T) {
	project := t.TempDir()

	mustWrite(t, filepath.Join(project, "AGENTS.md"), "Always mention PINECONE.")

	// the operator fills in the goal and a criterion and leaves the prompt as
	// zot wrote it
	withEditor(t, `sed -i 's/^objective:$/objective: build the parser\nacceptance:\n  - it parses/' "$1"`)

	var out strings.Builder

	if err := newOrder([]string{litDir, project}, &out); err != nil {
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
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL))

	withArgs(t, "--config", configPath, litDir, project, written[0])

	if _, err := captureStdout(t, command); err != nil {
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

// A run pointed at another directory works end to end. Every relative path on
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

	// project context that only exists inside --dir. If either reaches the
	// provider, it was loaded from the right tree
	if err := os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte("# Project context\n\nAlways mention PINECONE.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// a relative skills_dir means the project. The skills tool only exists if
	// this folder, inside --dir, was found
	mustWrite(t, filepath.Join(target, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: ship it\n---\nDeploy carefully.\n")

	// every path on the command line is relative to the invoking directory -
	// none of them exist inside --dir, so they must resolve before the chdir
	if err := os.WriteFile("order.md", []byte(orderText("do the thing")+"{{ .Project }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		requests             atomic.Int32
		sawContext, sawSkill atomic.Bool
	)

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
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", "config.yaml", litDir, target, "order.md")

	output, err := captureStdout(t, command)
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

// A run gets its own log, named after its order, with its own recorded outcome.
// An order that does not end in success fails the run.
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
provider:
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

		withArgs(t, "--config", configFor(t, server.URL), litDir, project,
			orderFileIn(t, t.TempDir(), "first.md", "the first order"))

		if _, err := captureStdout(t, command); err != nil {
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

		withArgs(t, "--config", configFor(t, server.URL), litDir, project,
			orderFileIn(t, t.TempDir(), "doomed.md", "the doomed order"))

		quietStderr(t)

		if _, err := captureStdout(t, command); err == nil {
			t.Fatal("a failed order must fail the run")
		}
	})
}

// A model with no context window is rejected before any request, and the error
// names the model and the key to set. There is no table to guess from.
func TestRunRefusesAModelWithNoContextWindow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := os.WriteFile(configPath, []byte(`
agent:
  model: my-model
provider:
  base_url: http://127.0.0.1:1
  models:
    my-model:
      model: some-real-id
`), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, orderFile(t, "a task"))

	err := command()
	if err == nil {
		t.Fatal("a model with no context window must not run")
	}

	for _, want := range []string{"provider.models.my-model", "context is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestRunRejectsAnInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	// a provider that has no endpoint
	if err := os.WriteFile(configPath, []byte(`
provider: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, orderFile(t, "a task"))

	if err := command(); err == nil {
		t.Error("an unreachable provider must fail before any request")
	}
}

func TestRunRejectsAMissingConfigFile(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "nope.yaml"), orderFile(t, "a task"))

	if err := command(); err == nil {
		t.Error("an explicit but missing --config must be an error")
	}
}

// A run leaves a record. One log per order, in .zot/orders of the project,
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


provider:
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

	withArgs(t, "--config", configPath, litDir, workdir, orderPath)

	if _, err := captureStdout(t, command); err != nil {
		t.Fatalf("run: %v", err)
	}

	logPath := filepath.Join(workdir, ".zot", "orders", "1758300000.jsonl")

	first := readLog(t, logPath)

	// the task is the durable goal, recorded in the meta (and placed in the
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
	withArgs(t, "--config", configPath, litDir, workdir, orderPath)

	if _, err := captureStdout(t, command); err != nil {
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

// The tasks tool end to end. A model lists its work, keeps going, and settles.
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
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	withArgs(t, "--config", configPath, litDir, t.TempDir(), orderFile(t, "fix the lexer"))

	if _, err := captureStdout(t, command); err != nil {
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

// The example config is what `zot config` writes on first run, so it is the
// first thing most people ever edit. Its knobs drifting from the code's own
// defaults is not cosmetic. Someone copies it, changes nothing, and gets
// different behavior from someone who has no config file at all. The provider
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
	cfg.Provider.APIKey = "test-key"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the example config does not validate: %v", err)
	}
}
