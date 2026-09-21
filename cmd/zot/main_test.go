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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.NoError(t, os.WriteFile(path, []byte(orderText(objective)), 0o644))

	return path
}

func orderFile(t *testing.T, objective string) string {
	t.Helper()

	return orderFileIn(t, t.TempDir(), "order.md", objective)
}

// withArgs runs a function with a fresh flag set and the given argv, so command() can be exercised the way the shell invokes it.
// It chdirs into --dir, so the working directory is put back afterwards for the next test.
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

	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(`
agent:
  model: test-model
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)), 0o644))

	withArgs(t, "--config", configPath, litDir, t.TempDir(), orderFile(t, "a task"))

	err := command()
	require.Error(t, err, "want it to say zot needs a terminal")
	require.Contains(t, err.Error(), "terminal", "want it to say zot needs a terminal")

	assert.EqualValues(t, 0, requests.Load(), "a run with no terminal must not reach the provider")
}

func TestLoadOrderLoadsTheFile(t *testing.T) {
	path := orderFile(t, "build the parser")

	o, err := loadOrder([]string{path})
	require.NoError(t, err)

	assert.Equal(t, "build the parser", o.Objective)
	assert.Equal(t, path, o.Path)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// A broken order fails the run before a provider is touched.
func TestLoadOrderFailsUpFront(t *testing.T) {
	_, err := loadOrder([]string{filepath.Join(t.TempDir(), "nope.md")})
	require.Error(t, err, "a missing order must not load")

	broken := filepath.Join(t.TempDir(), "broken.md")

	mustWrite(t, broken, "---\nobjective: x\n---\n{{ .Objectve }}")

	_, err = loadOrder([]string{broken})
	require.Error(t, err, "an order whose prompt names a field that does not exist must not load")
}

// Someone typing prose where an order file goes is the retraining moment. The
// error has to teach the new shape, not just report a missing file.
func TestLoadOrderTeachesProseTypers(t *testing.T) {
	_, err := loadOrder([]string{"add a health endpoint"})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "zot new", "the error should point at `zot new`")
}

// quietStderr silences stderr for a test that by design triggers the usage
// block.
func quietStderr(t *testing.T) {
	t.Helper()

	original := os.Stderr

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)

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
	require.Error(t, err, "want it to say how to write one")
	assert.Contains(t, err.Error(), "zot new", "want it to say how to write one")

	_, err = loadOrder([]string{orderFile(t, "a"), orderFile(t, "b")})
	require.Error(t, err, "want it to say zot runs one at a time")
	assert.Contains(t, err.Error(), "one order per invocation", "want it to say zot runs one at a time")
}

// withEditor makes $VISUAL a script that runs the given shell body against the
// file it is handed, so a test can play the part of someone writing an order.
func withEditor(t *testing.T, body string) {
	t.Helper()

	script := filepath.Join(t.TempDir(), "editor.sh")

	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755))

	t.Setenv("VISUAL", script)
	t.Setenv("EDITOR", "")
}

// readLog decodes every line of a session log, failing on any line that is not
// a JSON record.
func readLog(t *testing.T, path string) []session.Record {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	records := make([]session.Record, 0, len(lines))

	for i, line := range lines {
		var record session.Record

		err := json.Unmarshal([]byte(line), &record)
		require.NoError(t, err, "line %d is not a JSON record: %v\n%s", i+1, err, line)

		records = append(records, record)
	}

	return records
}

// The usage text is what a user sees when they get it wrong, so it has to name
// the things they can actually do - and nothing they cannot.
func TestUsageDescribesTheRealCommands(t *testing.T) {
	original := os.Stderr

	read, write, err := os.Pipe()
	require.NoError(t, err)

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
		assert.Contains(t, text, want, "usage does not mention %q", want)
	}

	// --dir belongs to both shapes. Where a run works, and where `zot new`
	// scaffolds - someone standing outside the project needs it either way
	n := strings.Count(text, litDir)
	assert.GreaterOrEqual(t, n, 2, "usage should document --dir for both running an order and `zot new` (%d mentions):\n%s", n, text)

	// The book is a convention, so --help is where someone finds out where
	// their orders went.
	assert.Contains(t, text, order.BookDir+"/orders", "usage does not say where zot new files an order")

	assert.NotContains(t, text, "--orders-dir", "usage still mentions --orders-dir")

	// ACP is gone. Zot runs unattended and has no protocol server
	assert.NotContains(t, strings.ToLower(text), "acp", "usage still mentions acp")

	// nothing is resumed, skipped or recorded between runs, and the help must
	// not promise it
	for _, gone := range []string{"--resume", "--fresh", "--rerun", "--records-dir", "--draft", "ledger"} {
		assert.NotContains(t, text, gone, "usage still mentions %q", gone)
	}
}

// The CLI uses pflag, so a flag may come after the positional order paths, as in `zot orders/a.md --dir proj`. The stdlib flag
// package stopped at the first non-flag and folded the flag into the positionals, which motivated the switch.
func TestFlagsAfterThePositionalOrdersAreParsed(t *testing.T) {
	set := pflag.NewFlagSet("zot", pflag.ContinueOnError)
	dir := set.String("dir", ".", "")

	require.NoError(t, set.Parse([]string{"a.md", "b.md", litDir, "proj"}))

	assert.Equal(t, "proj", *dir, "want it parsed as a flag")

	assert.Equal(t, "a.md b.md", strings.Join(set.Args(), " "), "want the paths before the flag")
}

// capture redirects one of the process's standard streams for the duration of a
// call and returns what was written to it.
func capture(t *testing.T, stream **os.File, fn func() error) (string, error) {
	t.Helper()

	original := *stream

	read, write, err := os.Pipe()
	require.NoError(t, err)

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
		assert.Nil(t, pflag.CommandLine.Lookup(name), "--%s is a flag, but the config already says it", name)
	}

	for _, name := range []string{"config", "dir"} {
		assert.NotNil(t, pflag.CommandLine.Lookup(name), "the config cannot say it")
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
	require.NoError(t, err)

	assert.Contains(t, output, "/some/where/config.yaml")
}

func TestRunRequiresAnOrder(t *testing.T) {
	quietStderr(t)
	withArgs(t)

	require.Error(t, command())
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

	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o644))

	withArgs(t, "--config", configPath, litDir, workdir, orderFile(t, "do the thing"))

	output, err := captureStdout(t, command)
	require.NoError(t, err, "run")

	for _, want := range []string{"do the thing", "on it", "complete"} {
		assert.Contains(t, output, want)
	}
}

// contractHeading is how the contract is spotted in an assembled prompt.
const contractHeading = "## Non-interactive contract"

// The whole loop of the new order. Zot new scaffolds the file with the full prompt, the operator writes the goal, and what the
// model is sent is that prompt rendered, with the goal, the real tools, the working directory, AGENTS.md and the contract once.
func TestAScaffoldedOrderRunsWithItsFullPrompt(t *testing.T) {
	project := t.TempDir()

	mustWrite(t, filepath.Join(project, "AGENTS.md"), "Always mention PINECONE.")

	// the operator fills in the goal and a criterion and leaves the prompt as
	// zot wrote it
	withEditor(t, `sed -i 's/^objective:$/objective: build the parser\nacceptance:\n  - it parses/' "$1"`)

	var out strings.Builder

	require.NoError(t, newOrder([]string{litDir, project}, &out))

	written, err := filepath.Glob(filepath.Join(project, order.BookDir, "orders", "*.md"))
	require.NoError(t, err)
	require.Len(t, written, 1)

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

	_, err = captureStdout(t, command)
	require.NoError(t, err)

	for _, want := range []string{
		"## Your task\n\nbuild the parser",
		"1. it parses",
		`- "shell":`,
		`- "tasks":`,
		"# Project context\n\nAlways mention PINECONE.",
	} {
		assert.Contains(t, system, want)
	}

	n := strings.Count(system, contractHeading)
	assert.Equal(t, 1, n, "the contract appears %d times, want once", n)
}

// A run pointed at another directory works end to end. Relative paths on the command line resolve from the invoking directory
// before zot chdirs into --dir, the session records the real working directory, and project context comes from --dir.
func TestRunFromADifferentDirectoryEndToEnd(t *testing.T) {
	invocation := t.TempDir()

	t.Chdir(invocation)

	target := filepath.Join(invocation, "project")

	require.NoError(t, os.MkdirAll(target, 0o755))

	// project context that only exists inside --dir. If either reaches the
	// provider, it was loaded from the right tree
	require.NoError(t, os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte("# Project context\n\nAlways mention PINECONE.\n"), 0o644))

	// a relative skills_dir means the project. The skills tool only exists if
	// this folder, inside --dir, was found
	mustWrite(t, filepath.Join(target, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: ship it\n---\nDeploy carefully.\n")

	// every path on the command line is relative to the invoking directory -
	// none of them exist inside --dir, so they must resolve before the chdir
	require.NoError(t, os.WriteFile("order.md", []byte(orderText("do the thing")+"{{ .Project }}\n"), 0o644))

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

	require.NoError(t, os.WriteFile("config.yaml", []byte(fmt.Sprintf(`
agent:
  model: test-model
skills_dir: skills
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)), 0o644))

	withArgs(t, "--config", "config.yaml", litDir, target, "order.md")

	output, err := captureStdout(t, command)
	require.NoError(t, err, "run")

	for _, want := range []string{"do the thing", "on it", "complete"} {
		assert.Contains(t, output, want)
	}

	assert.True(t, sawContext.Load(), "project context did not come from --dir (AGENTS.md seen: %v, skills tool seen: %v)", sawContext.Load(), sawSkill.Load())
	assert.True(t, sawSkill.Load(), "project context did not come from --dir (AGENTS.md seen: %v, skills tool seen: %v)", sawContext.Load(), sawSkill.Load())

	// the log lands in the project being worked on, named after the order
	records := readLog(t, filepath.Join(target, ".zot", "orders", "order.jsonl"))

	assert.NotNil(t, records[0].Meta)
	assert.Equal(t, target, records[0].Meta.Workdir)
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

		require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`
agent:
  model: test-model
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, url)), 0o644))

		return path
	}

	t.Run("the run gets its own log", func(t *testing.T) {
		project := t.TempDir()

		server := settle("success", `{"summary":"complete"}`)
		defer server.Close()

		withArgs(t, "--config", configFor(t, server.URL), litDir, project,
			orderFileIn(t, t.TempDir(), "first.md", "the first order"))

		_, err := captureStdout(t, command)
		require.NoError(t, err)

		records := readLog(t, filepath.Join(project, ".zot", "orders", "first.jsonl"))

		assert.NotNil(t, records[0].Meta, "want the order's objective as the task")
		assert.Equal(t, "the first order", records[0].Meta.Task, "want the order's objective as the task")
	})

	t.Run("a failed order fails the run", func(t *testing.T) {
		project := t.TempDir()

		server := settle("failure", `{"reason":"cannot"}`)
		defer server.Close()

		withArgs(t, "--config", configFor(t, server.URL), litDir, project,
			orderFileIn(t, t.TempDir(), "doomed.md", "the doomed order"))

		quietStderr(t)

		_, err := captureStdout(t, command)
		require.Error(t, err, "a failed order must fail the run")
	})
}

// A model with no context window is rejected before any request, and the error
// names the model and the key to set. There is no table to guess from.
func TestRunRefusesAModelWithNoContextWindow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	require.NoError(t, os.WriteFile(configPath, []byte(`
agent:
  model: my-model
provider:
  base_url: http://127.0.0.1:1
  models:
    my-model:
      model: some-real-id
`), 0o644))

	withArgs(t, "--config", configPath, orderFile(t, "a task"))

	err := command()
	require.Error(t, err, "a model with no context window must not run")

	for _, want := range []string{"provider.models.my-model", "context is required"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestRunRejectsAnInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	// a provider that has no endpoint
	require.NoError(t, os.WriteFile(configPath, []byte(`
provider: {}
`), 0o644))

	withArgs(t, "--config", configPath, orderFile(t, "a task"))

	require.Error(t, command(), "an unreachable provider must fail before any request")
}

func TestRunRejectsAMissingConfigFile(t *testing.T) {
	withArgs(t, "--config", filepath.Join(t.TempDir(), "nope.yaml"), orderFile(t, "a task"))

	require.Error(t, command(), "an explicit but missing --config must be an error")
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

	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o644))

	orderPath := orderFileIn(t, t.TempDir(), "1758300000.md", "the first task")

	withArgs(t, "--config", configPath, litDir, workdir, orderPath)

	_, err := captureStdout(t, command)
	require.NoError(t, err)

	logPath := filepath.Join(workdir, ".zot", "orders", "1758300000.jsonl")

	first := readLog(t, logPath)

	// the task is the durable goal, recorded in the meta (and placed in the
	// instructions), not as the opening user message
	meta := first[0]

	assert.Equal(t, session.KindMeta, meta.Kind, "the log must open with the objective")
	assert.Equal(t, "the first task", meta.Meta.Task, "the log must open with the objective")

	assert.Equal(t, "test-model", meta.Meta.Model)
	assert.NotEmpty(t, meta.Meta.Workdir)

	last := first[len(first)-1]
	assert.Equal(t, session.KindResult, last.Kind, "the log must end with the outcome")
	assert.NotEmpty(t, last.Result.Reason, "the log must end with the outcome")

	// running the order again adds a run to the same log, after the first
	withArgs(t, "--config", configPath, litDir, workdir, orderPath)

	_, err = captureStdout(t, command)
	require.NoError(t, err)

	second := readLog(t, logPath)

	require.Len(t, second, 2*len(first), "the log holds %d records after two runs, want the first run's %d twice", len(second), len(first))

	for i, record := range first {
		assert.Equal(t, record.Kind, second[i].Kind, "the first run must be left as it was")
	}

	again := second[len(first)]
	assert.Equal(t, session.KindMeta, again.Kind, "the second run must open with its own meta, got %+v", again)
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

	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(`
agent:
  model: test-model
provider:
  base_url: %s
  api_key: test-key
  models:
    test-model:
      context: 100000
`, server.URL)), 0o644))

	withArgs(t, "--config", configPath, litDir, t.TempDir(), orderFile(t, "fix the lexer"))

	_, err := captureStdout(t, command)
	require.NoError(t, err)

	assert.EqualValues(t, 2, requests.Load(), "want the tasks turn and the settling turn")
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

			require.NoError(t, os.WriteFile(orderPath, []byte(test.body), 0o644))

			loaded, err := order.Load(orderPath)
			require.NoError(t, err)

			got := orderOptions(t.TempDir(), loaded)

			assert.Equal(t, test.want, got.Title)
		})
	}
}

// The example config is what `zot config` writes on first run, so its knobs drifting from the code's defaults would give a copier
// different behavior from someone with no config file. The provider and model are the exception, since there are no defaults for them.
func TestTheExampleConfigMatchesTheDefaults(t *testing.T) {
	var example config.Config

	require.NoError(t, yaml.Unmarshal(configs.ExampleConfigYAML, &example), "the embedded example config does not parse")

	defaults := config.Defaults()

	assert.Equal(t, defaults.Agent.MaxIterations, example.Agent.MaxIterations)
}

// A config file is only useful if it survives being loaded, and the example is
// the one file guaranteed to be in front of a new user.
func TestTheExampleConfigLoadsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	require.NoError(t, os.WriteFile(path, configs.ExampleConfigYAML, 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err, "the example config does not load")

	// a key so validation is judging the shape rather than the environment
	cfg.Provider.APIKey = "test-key"

	require.NoError(t, cfg.Validate(), "the example config does not validate")
}
