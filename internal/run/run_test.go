package run_test

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/run"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/skills"
	"github.com/openzot/openzot/internal/testutils"
	"github.com/openzot/openzot/internal/tools"
	"github.com/openzot/openzot/internal/tui"
)

// testOrder is an order for a run that never was a file.
func testOrder(objective string) order.Order {
	return order.Order{Objective: objective}
}

func TestLoadProjectContext(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	// A global AGENTS.md in the config dir and a project one in the work dir.
	testutils.Write(t, filepath.Join(configDir, "AGENTS.md"), "GLOBAL CONVENTIONS")
	testutils.Write(t, filepath.Join(workDir, "AGENTS.md"), "PROJECT CONVENTIONS")

	project := run.LoadProjectContext(configDir, workDir, workDir)

	// both files are there, the config directory's first, each once
	for _, want := range []string{"GLOBAL CONVENTIONS", "PROJECT CONVENTIONS"} {
		assert.Equal(t, 1, strings.Count(project, want), "project context should hold %q once", want)
	}

	i, j := strings.Index(project, "GLOBAL"), strings.Index(project, "PROJECT")
	assert.LessOrEqual(t, i, j, "expected config-dir AGENTS.md to appear before work-dir AGENTS.md")
}

func TestLoadProjectContextNoFiles(t *testing.T) {
	project := run.LoadProjectContext(t.TempDir())
	assert.Empty(t, project, "expected no project context when no AGENTS.md is present")
}

func TestLoadSkillsFromTheConfiguredFolder(t *testing.T) {
	t.Run("unset means no skills", func(t *testing.T) {
		loaded, err := run.LoadSkills("")
		require.NoError(t, err)
		assert.Nil(t, loaded)
	})

	t.Run("a relative folder is taken against the working directory", func(t *testing.T) {
		project := t.TempDir()
		testutils.Write(t, filepath.Join(project, "my-skills", "greet", "SKILL.md"), "---\nname: greet\ndescription: say hello\n---\nbody")
		t.Chdir(project)

		loaded, err := run.LoadSkills("my-skills")
		require.NoError(t, err)

		assert.Len(t, loaded, 1, "want greet loaded with its content")
		assert.Equal(t, "greet", loaded[0].Name, "want greet loaded with its content")
		assert.NotEmpty(t, loaded[0].Content, "want greet loaded with its content")
	})

	t.Run("~ is the home directory", func(t *testing.T) {
		home := t.TempDir()
		testutils.Write(t, filepath.Join(home, "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\nbody")
		t.Setenv("HOME", home)

		loaded, err := run.LoadSkills("~/skills")
		require.NoError(t, err)

		assert.Len(t, loaded, 1, "want deploy from ~/skills")
		assert.Equal(t, "deploy", loaded[0].Name, "want deploy from ~/skills")
	})

	t.Run("a folder that cannot be read stops the run", func(t *testing.T) {
		_, err := run.LoadSkills(filepath.Join(t.TempDir(), "missing"))
		require.Error(t, err, "want it to name skills_dir")
		assert.Contains(t, err.Error(), "skills_dir", "want it to name skills_dir")
	})
}

// declared is the model list a provider needs to run the named models. Each
// with a context window, since a model without one cannot run.
func declared(names ...string) map[string]config.ModelConfig {
	models := make(map[string]config.ModelConfig, len(names))

	for _, name := range names {
		models[name] = config.ModelConfig{Context: 100_000}
	}

	return models
}

// testDefaults is the built-in configuration plus what it lacks by design, a model
// to run and a prompt, which here is just the goal.
func testDefaults() *config.Config {
	cfg := config.Defaults()
	cfg.Agent.Model = litGlm52
	cfg.Prompt = "{{ .Objective }}"

	return &cfg
}

func stubProvider(t *testing.T) *config.Config {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

	return cfg
}

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

// logged is the options of a run that is recorded, as every run must be.
func logged(t *testing.T) run.Options {
	t.Helper()

	return run.Options{Viewer: headlessViewer, SessionPath: filepath.Join(t.TempDir(), "task.jsonl")}
}

// The whole path a skill takes. The model lists the skills, reads one by name,
// and each answer reaches its next request - from memory, with the folder gone.
func TestTheModelListsAndReadsASkill(t *testing.T) {
	project := t.TempDir()
	skillsDir := filepath.Join(project, "skills")

	testutils.Write(t, filepath.Join(skillsDir, "deploy", "SKILL.md"),
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
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

	offered, err := run.LoadSkills(skillsDir)
	require.NoError(t, err)

	// loaded at startup. The folder is not read again during the run
	require.NoError(t, os.RemoveAll(skillsDir))

	_, err = testutils.CaptureStdout(t, func() error {
		options := logged(t)
		options.Skills = offered

		return run.Run(t.Context(), cfg, testOrder("do the thing"), options)
	})
	require.NoError(t, err)

	close(bodies)

	var all []string
	for body := range bodies {
		all = append(all, body)
	}

	require.Len(t, all, 3, "want list, read, settle")

	assert.NotContains(t, all[0], "LISTING-MARKER", "nothing of a skill may reach the model before it asks")
	assert.NotContains(t, all[0], "INSTRUCTIONS-MARKER", "nothing of a skill may reach the model before it asks")

	assert.Contains(t, all[1], "LISTING-MARKER", "the listing must carry the description and not the instructions")
	assert.NotContains(t, all[1], "INSTRUCTIONS-MARKER", "the listing must carry the description and not the instructions")

	assert.Contains(t, all[2], "INSTRUCTIONS-MARKER", "reading a skill by name must return its full instructions")
}

// Credential resolution is the part of the configuration that fails silently, since a key that never arrives looks like a
// bad key. These assert on the Authorization header the provider receives, the only proof a credential was resolved.
func TestCredentialResolution(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		config string
		want   string
		model  string
	}{
		{
			name:   "a provider api_key",
			config: "  api_key: sk-provider\n  models:\n    gpt-4:\n      context: 100000\n",
			want:   "Bearer sk-provider",
			model:  litGpt4,
		},
		{
			name:   "a $VAR reference, so no secret is on disk",
			env:    map[string]string{"MY_PROVIDER_KEY": "sk-from-env"},
			config: "  api_key: $MY_PROVIDER_KEY\n  models:\n    gpt-4:\n      context: 100000\n",
			want:   "Bearer sk-from-env",
			model:  litGpt4,
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

			path := testutils.WriteConfig(t, fmt.Sprintf(`
agent:
  model: %q
provider:
  base_url: %s
%s`, test.model, server.URL, test.config))

			cfg, err := config.Load(path)
			require.NoError(t, err)

			client, _, err := run.Resolve(t.Context(), &cfg, nil)
			require.NoError(t, err)

			assert.Equal(t, litGpt4, client.Config().Model)

			_, err = testutils.CaptureStdout(t, func() error {
				return run.Run(t.Context(), &cfg, testOrder("do the thing"), logged(t))
			})
			require.NoError(t, err)

			select {
			case got := <-seen:
				assert.Equal(t, test.want, got)
			default:
				require.FailNow(t, "the provider was never called")
			}
		})
	}
}

// content_array on a model has to reach the wire, so every message the run
// sends carries array content, while other models keep the plain string.
func TestContentArrayReachesTheWire(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra string
		want  string
	}{
		{name: "asked for", extra: "      content_array: true\n", want: "["},
		{name: "not asked for", want: `"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AGENT_CONFIG", "")

			seen := make(chan []json.RawMessage, 1)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}

				_ = json.NewDecoder(r.Body).Decode(&body)

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

			path := testutils.WriteConfig(t, fmt.Sprintf(`
prompt: '{{ .Objective }}'
agent:
  model: default
provider:
  base_url: %s
  api_key: x
  models:
    default:
      model: Qwen3.8-27B
      context: 100000
%s`, server.URL, test.extra))

			cfg, err := config.Load(path)
			require.NoError(t, err)

			_, err = testutils.CaptureStdout(t, func() error {
				return run.Run(t.Context(), &cfg, testOrder("do the thing"), logged(t))
			})
			require.NoError(t, err)

			select {
			case contents := <-seen:
				require.GreaterOrEqual(t, len(contents), 2, "want the system prompt and the task")

				for i, content := range contents {
					assert.True(t, strings.HasPrefix(string(content), test.want), "message %d", i)
				}
			default:
				require.FailNow(t, "the provider was never called")
			}
		})
	}
}

// A provider that names no endpoint cannot resolve, and says so rather than
// sending a request to nowhere.
func TestAProviderWithoutAnEndpointIsRejected(t *testing.T) {
	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{APIKey: litSkTest, Models: declared(litGlm52)}

	_, _, err := run.Resolve(t.Context(), cfg, nil)
	require.Error(t, err, "a provider naming no endpoint must be rejected")

	// the error has to be actionable. It names the field to set
	assert.Contains(t, err.Error(), "base_url", "want it to name what is missing")
}

// shell acts on the machine, so a call the model did not finish writing is
// rejected, never mended into one that runs.
func TestResolveNeverRepairsAShellCall(t *testing.T) {
	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: litHTTP12700, Models: declared(litGlm52)}

	_, opts, err := run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	assert.Len(t, opts.Unrepaired, 1, "want just the shell tool")
	assert.Equal(t, tools.ShellTool, opts.Unrepaired[0], "want just the shell tool")
}

// The window is the operator's to state and agent keeps no table of what models
// can take, so a model with none cannot run. Load-time validation says so first.
// Resolve holds the same rule for a config that skipped it.
func TestResolveRefusesAModelWithoutAContextWindow(t *testing.T) {
	cases := map[string]map[string]config.ModelConfig{
		"the model is not declared":     declared("some-other-model"),
		"no models are declared at all": nil,
		"the window is zero":            {litGlm52: {Model: litGlm52}},
		"the window is negative":        {litGlm52: {Context: -1}},
	}

	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testDefaults()
			cfg.Provider = config.ProviderConfig{BaseURL: litHTTP12700, Models: models}

			_, _, err := run.Resolve(t.Context(), cfg, nil)
			require.Error(t, err, "a model with no context window resolved")

			for _, want := range []string{litGlm52, "context window", "provider.models"} {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// A declared provider resolves to its own endpoint and credential, with the
// model name passed through untouched, and its own name is what the client
// reports.
func TestResolveSelectsTheModelFromTheOneProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AGENT_CONFIG", "")
	t.Setenv("ALPHA_KEY", "sk-alpha")

	cfg, err := config.Load(testutils.WriteConfig(t, `
agent:
  model: fast
provider:
  base_url: https://alpha.example.com/v1
  api_key: $ALPHA_KEY
  models:
    fast:
      model: alpha-flash
      context: 32000
      max_iterations: 20
    smart:
      model: alpha-pro
      context: 200000
      reasoning_effort: high
`))
	require.NoError(t, err)

	for name, want := range map[string]struct {
		model      string
		window     int
		iterations int
		effort     string
	}{
		"fast":  {"alpha-flash", 32000, 20, ""},
		"smart": {"alpha-pro", 200000, cfg.Agent.MaxIterations, "high"},
	} {
		cfg.Agent.Model = name

		client, opts, err := run.Resolve(t.Context(), &cfg, nil)
		require.NoError(t, err, "resolve(%s)", name)

		got := client.Config()

		assert.Equal(t, want.model, got.Model, "%s resolved to model", name)
		assert.Equal(t, want.effort, got.ReasoningEffort, "%s resolved to model", name)
		assert.Equal(t, want.window, opts.ContextWindow, "%s resolved to model", name)
		assert.Equal(t, want.iterations, opts.MaxIterations, "%s resolved to model", name)

		// one provider, so one endpoint and one credential whatever the model
		assert.Equal(t, "https://alpha.example.com/v1", got.BaseURL, "%s talks to", name)
		assert.Equal(t, "sk-alpha", got.APIKey, "%s talks to", name)
		assert.Equal(t, "alpha.example.com", got.Provider, "%s talks to", name)
	}

	cfg.Agent.Model = "huge"

	_, _, err = run.Resolve(t.Context(), &cfg, nil)
	require.Error(t, err, "a model the provider does not list resolved")
	assert.Contains(t, err.Error(), "context window", "a model the provider does not list resolved")
}

// Nothing is built in. With no provider declared a run does not resolve, whatever
// the environment holds.
func TestNoProviderIsBuiltIn(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZAI_API_KEY", "sk-zai")

	_, _, err := run.Resolve(t.Context(), testDefaults(), nil)
	require.Error(t, err, "a run resolved with no provider declared")
}

// A model entry aliases a real id and caps iterations, both of which take
// priority over the run defaults.
func TestResolveCustomModelAlias(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AGENT_CONFIG", "")
	path := testutils.WriteConfig(t, `
agent:
  model: fast
provider:
  base_url: https://gw.example.com/v1
  api_key: sk-test
  models:
    fast:
      model: gpt-5
      max_iterations: 50
      context: 32000
`)

	cfg, err := config.Load(path)
	require.NoError(t, err)

	client, opts, err := run.Resolve(t.Context(), &cfg, nil)
	require.NoError(t, err)

	assert.Equal(t, litGpt5, client.Config().Model)

	assert.Equal(t, 50, opts.MaxIterations, "want 50 (from custom model)")

	// the operator declared the endpoint's real window. The run must budget to it
	assert.Equal(t, 32000, opts.ContextWindow, "want the per-model override")
}

// The iteration denominator in the meta bar must be the limit the run will stop at. A per-model max_iterations lowers it,
// and a bar counting to a number the run never reaches ("iter 12/100" when it ends at 40) misreports the run.
func TestTheViewerShowsTheIterationLimitTheRunEnforces(t *testing.T) {
	cfg := testDefaults()
	cfg.Agent.Model = "capped"
	cfg.Agent.MaxIterations = 100
	cfg.Provider = config.ProviderConfig{
		BaseURL: litHTTPSGwExampleCom,
		APIKey:  litSkTest,
		Models: map[string]config.ModelConfig{
			"capped": {Model: litGpt5, MaxIterations: 40, Context: 100_000},
		},
	}

	_, opts, err := run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	require.Equal(t, 40, opts.MaxIterations, "want the model's cap")

	meta := run.ViewerMeta(cfg, "a task", "/somewhere", &opts)

	assert.Equal(t, opts.MaxIterations, meta.MaxIterations, "the viewer shows a limit of %d while the engine stops at %d", meta.MaxIterations, opts.MaxIterations)

	// the default is a 1,000,000 fallback rather than a budget, so there is
	// nothing worth counting towards and the denominator stays hidden
	cfg.Agent.MaxIterations = config.Defaults().Agent.MaxIterations
	cfg.Provider.Models["capped"] = config.ModelConfig{Model: litGpt5, Context: 100_000}

	_, opts, err = run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	assert.Equal(t, 0, run.ViewerMeta(cfg, "a task", "/somewhere", &opts).MaxIterations, "want the backstop hidden")
}

// Run is the whole thing end to end. Config in, a provider call out, a
// transcript back. The tests' stand-in viewer prints what the run said, which is
// what can be asserted on without a terminal.
func TestRunTaskEndToEnd(t *testing.T) {
	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		frames := [][]string{
			{
				`{"choices":[{"delta":{"content":"working on it"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			},
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
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

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

	err := run.Run(t.Context(), cfg, testOrder("do the thing"), logged(t))

	write.Close()

	os.Stdout = original

	output := <-done

	require.NoError(t, err, "Run")

	for _, want := range []string{"do the thing", "working on it", "all done"} {
		assert.Contains(t, output, want)
	}
}

// A misconfigured provider fails before any request is made, with a message that
// says what to fix.
func TestRunRejectsAnUnconfiguredProvider(t *testing.T) {
	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{}

	err := run.Run(t.Context(), cfg, testOrder("task"), logged(t))
	require.Error(t, err, "an unconfigured provider must fail")

	assert.Contains(t, err.Error(), "provider:", "the error should say to declare the provider")
}

// A run leaves a record of itself. What it was asked, which model answered, and
// how it ended.
func TestRunWithRecordsASession(t *testing.T) {
	cfg := stubProvider(t)

	path := filepath.Join(t.TempDir(), ".agent", "orders", "task.jsonl")

	_, err := testutils.CaptureStdout(t, func() error {
		return run.Run(t.Context(), cfg, testOrder("do the thing"), run.Options{Viewer: headlessViewer, SessionPath: path})
	})
	require.NoError(t, err)

	records := testutils.ReadLog(t, path)

	first, last := records[0], records[len(records)-1]

	require.Equal(t, session.KindMeta, first.Kind)
	require.NotNil(t, first.Meta)

	assert.Equal(t, "do the thing", first.Meta.Task)
	assert.Equal(t, cfg.Provider.Label(), first.Meta.Provider)

	assert.NotEmpty(t, first.Meta.Model, "the log must record what it ran against")
	assert.NotEmpty(t, first.Meta.Workdir, "the log must record what it ran against")

	assert.Equal(t, session.KindResult, last.Kind, "the log must end with the outcome")
	assert.NotNil(t, last.Result, "the log must end with the outcome")
	assert.NotEmpty(t, last.Result.Reason, "the log must end with the outcome")

	// the goal is the durable task, recorded in the meta and placed in the
	// instructions. The opening message is the kickoff, not the task
	var opening bool

	for _, record := range records {
		if record.Kind == session.KindMessage && record.Message.Text == run.TaskKickoff {
			opening = true
		}
	}

	assert.True(t, opening, "the log must record the opening message: %v", records)
}

// Running the same order again adds a run to its log rather than replacing it,
// and starts from zero. The second run opens with the kickoff and carries
// nothing of the first run's conversation.
func TestRunningTheSameTaskAgainAppendsAFreshRun(t *testing.T) {
	cfg := stubProvider(t)

	path := filepath.Join(t.TempDir(), "task.jsonl")

	for i := range 2 {
		_, err := testutils.CaptureStdout(t, func() error {
			return run.Run(t.Context(), cfg, testOrder("the same brief"), run.Options{Viewer: headlessViewer, SessionPath: path})
		})
		require.NoError(t, err, "run %d", i+1)
	}

	var runs [][]session.Record

	for _, record := range testutils.ReadLog(t, path) {
		if record.Kind == session.KindMeta {
			runs = append(runs, nil)
		}

		require.NotEmpty(t, runs, "a record precedes the first meta: %+v", record)

		runs[len(runs)-1] = append(runs[len(runs)-1], record)
	}

	require.Len(t, runs, 2)

	assert.Len(t, runs[0], len(runs[1]), "the runs differ in length (%d, %d), so one carried the other", len(runs[0]), len(runs[1]))

	for i, records := range runs {
		assert.Equal(t, session.KindResult, records[len(records)-1].Kind, "run %d does not end with its outcome", i+1)
	}
}

// The log holds what the model thought, and holds it while a tool is still running. A snapshot taken by the command itself
// already carries the turn's reasoning and the request, so a run killed inside a long command loses nothing of that turn.
func TestTheLogHoldsReasoningBeforeItsToolFinishes(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "task.jsonl")
	snapshot := filepath.Join(dir, "snapshot.jsonl")

	command, err := json.Marshal(map[string]string{"command": "cp " + path + " " + snapshot})
	require.NoError(t, err)

	call, err := json.Marshal(string(command))
	require.NoError(t, err)

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
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

	_, err = testutils.CaptureStdout(t, func() error {
		return run.Run(t.Context(), cfg, testOrder("do the thing"), run.Options{Viewer: headlessViewer, SessionPath: path})
	})
	require.NoError(t, err, "RunWith")

	var reasoning, request bool

	for _, record := range testutils.ReadLog(t, snapshot) {
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

	assert.True(t, reasoning, "the log was missing the turn while its tool ran (reasoning %v, request %v)", reasoning, request)
	assert.True(t, request, "the log was missing the turn while its tool ran (reasoning %v, request %v)", reasoning, request)

	// and the finished log keeps it too
	var final bool

	for _, record := range testutils.ReadLog(t, path) {
		if record.Kind == session.KindMessage && record.Message.Type == "reasoning" {
			final = true
		}
	}

	assert.True(t, final, "the finished log lost the model's reasoning")
}

// The digest names the log the run was appended to, and says nothing of one
// when the run was not recorded.
func TestPrintDigestNamesTheSessionLog(t *testing.T) {
	result := loop.Result{Reason: loop.StopSettled, Budget: loop.Budget{Iterations: 1}}

	var recorded, unrecorded strings.Builder

	run.PrintDigest(&recorded, "/w/.agent/orders/1758300000.jsonl", &result)
	run.PrintDigest(&unrecorded, "", &result)

	assert.Contains(t, recorded.String(), "/w/.agent/orders/1758300000.jsonl", "the digest must say where the log is")

	assert.NotContains(t, unrecorded.String(), "session", "no log was written, so the digest must not mention one")
}

// A run that cannot be recorded is rejected before the provider is asked
// anything. Its log is its record and its agent's long-term memory.
func TestARunWithAnUnwritableSessionLogIsRefused(t *testing.T) {
	var asked atomic.Int32

	cfg := stubProvider(t)

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	t.Cleanup(server.Close)

	cfg.Provider.BaseURL = server.URL

	blocked := filepath.Join(t.TempDir(), "a-file")

	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))

	output, err := testutils.CaptureStdout(t, func() error {
		return run.Run(t.Context(), cfg, testOrder("do the thing"), run.Options{
			Viewer:      headlessViewer,
			SessionPath: filepath.Join(blocked, "task.jsonl"),
		})
	})
	require.Error(t, err, "err = %v, want the unwritable log refused and named\n%s", err, output)
	require.Contains(t, err.Error(), "session log", "err = %v, want the unwritable log refused and named\n%s", err, output)

	assert.EqualValues(t, 0, asked.Load(), "the provider was asked something before the log was known to work")
}

// There is no run without a log. The caller must say where it goes.
func TestARunWithNoSessionLogIsRefused(t *testing.T) {
	dir := t.TempDir()

	t.Chdir(dir)

	_, err := testutils.CaptureStdout(t, func() error {
		return run.Run(t.Context(), stubProvider(t), testOrder("do the thing"), run.Options{Viewer: headlessViewer})
	})
	require.Error(t, err, "want a run with no log refused")
	require.Contains(t, err.Error(), "session log", "want a run with no log refused")

	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries, "a refused run wrote %d entries", len(entries))
}

// A run with nothing configured fails before any request, and says what to
// declare - there is no default provider or model to fall back on.
func TestARunWithNothingConfiguredSaysWhatIsMissing(t *testing.T) {
	cfg := config.Defaults()

	err := cfg.Validate()
	require.Error(t, err, "want it to name the missing model")
	assert.Contains(t, err.Error(), "agent.model", "want it to name the missing model")

	cfg.Agent.Model = "m"

	err = cfg.Validate()
	require.Error(t, err, "want it to name the missing prompt")
	assert.Contains(t, err.Error(), "prompt", "want it to name the missing prompt")

	cfg.Prompt = "x"

	err = cfg.Validate()
	require.Error(t, err, "want it to say to declare a provider")
	assert.Contains(t, err.Error(), "provider", "want it to say to declare a provider")

	// and the library entry point, which does not validate, says the same
	err = run.Run(t.Context(), &cfg, testOrder("task"), logged(t))
	require.Error(t, err, "want it to say to declare a provider")
	assert.Contains(t, err.Error(), "provider", "want it to say to declare a provider")
}

// stubProviderConfig is a config that resolves without a network.
func stubProviderConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: litHTTP12700, APIKey: "k", Models: declared(litGlm52)}

	return cfg
}

// starterPrompt is the prompt of the starter config `agent config` seeds, which is the one agent ships.
func starterPrompt(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, configs.ExampleConfigYAML, 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)

	return cfg.Prompt
}

// promptOf renders a prompt for an order the way a run does, with the tools a run really has.
func promptOf(t *testing.T, text string, o order.Order) string {
	t.Helper()

	client, opts, err := run.Resolve(t.Context(), stubProviderConfig(t), nil)
	require.NoError(t, err)

	prompt, err := o.Render(text, run.OrderEnv(stubProviderConfig(t), client, &opts, "/work", "", ""))
	require.NoError(t, err)

	return prompt
}

// newOrderNamed is the order agent new scaffolds, with its goal written in.
func newOrderNamed(t *testing.T, objective string) order.Order {
	t.Helper()

	o, err := order.Parse([]byte(strings.Replace(order.Blank(), "objective:\n", "objective: "+objective+"\n", 1)))
	require.NoError(t, err)

	return o
}

// defaultPrompt is what the starter config sends the model for a freshly scaffolded order.
func defaultPrompt(t *testing.T) string {
	t.Helper()

	return promptOf(t, starterPrompt(t), newOrderNamed(t, "build a parser"))
}

// The task is the durable goal, so it must land in the system prompt, which trimming never drops and always orders first,
// not in a user message that a long run can trim away.
func TestTheObjectiveGoesIntoTheSystemPrompt(t *testing.T) {
	o, err := order.Parse([]byte(strings.Replace(order.Blank(), "objective:\n", "objective: \"  build a parser  \"\n", 1)))
	require.NoError(t, err)

	got := promptOf(t, starterPrompt(t), o)

	assert.Contains(t, got, "You are agent", "the starter config's prompt must be what is sent")

	assert.Contains(t, got, "## Your task\n\nbuild a parser", "the objective must be in the prompt, trimmed")
}

// The tools the prompt names come from the tool set the run really has, so it cannot describe tools that are not offered.
// It once told the agent to call "edit", "exec", "exit" and "progress" when none existed, which generating the list prevents.
func TestTheDefaultPromptNamesOnlyRealTools(t *testing.T) {
	prompt := defaultPrompt(t)

	known := map[string]bool{
		// the terminal tools the loop injects
		"success": true,
		"failure": true,
	}

	for _, tool := range tools.New(1000, nil) {
		known[tool.Info().Name] = true
	}

	// pull every "quoted" token out of the prompt and check the tool-looking ones
	// are known
	for _, quoted := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(prompt, -1) {
		name := quoted[1]

		// only check things that look like tool names (a known tool, or the
		// phantom ones we are guarding against)
		phantom := map[string]bool{"edit": true, "exec": true, "exit": true, "abort": true, "read": true, "write": true, "list": true, "plan": true, "progress": true}

		if phantom[name] {
			assert.True(t, known[name], "the prompt names %q, which is not a known tool", name)
		}
	}

	// and positively assert the tools the prompt promises are all present
	for _, want := range []string{"tasks", "shell", "success", "failure"} {
		assert.True(t, known[want], "the prompt relies on %q but it is not a known tool", want)

		assert.Contains(t, prompt, `"`+want+`"`, "the prompt should name the %q tool so the model knows to use it", want)
	}
}

// A tool the run does not have is not in its prompt. No skills, no skills tool.
func TestThePromptListsTheToolsTheRunHas(t *testing.T) {
	without := defaultPrompt(t)

	assert.NotContains(t, without, `- "skills":`, "the prompt lists a skills tool the run does not have")

	cfg := stubProviderConfig(t)
	offered := []skills.Skill{{Name: "deploy", Description: "ship it"}}

	client, opts, err := run.Resolve(t.Context(), cfg, offered)
	require.NoError(t, err)

	with, err := newOrderNamed(t, "x").Render(starterPrompt(t), run.OrderEnv(cfg, client, &opts, "/work", "", ""))
	require.NoError(t, err)

	assert.Contains(t, with, `- "skills":`, "the prompt must list the skills tool when the run has one")
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
		assert.Contains(t, prompt, want, "the prompt should mention %q so the model knows how to work through shell", want)
	}
}

// The tasks tool only helps if the model keeps it current, and the prompt is the
// only thing that says how. Each status it may use, and that a blocker or an
// assumption belongs in a note.
func TestTheDefaultPromptTeachesHowToKeepTheTasksCurrent(t *testing.T) {
	prompt := defaultPrompt(t)

	for _, want := range []string{"in_progress", "done", "blocked", "note", "whole list"} {
		assert.Contains(t, prompt, want, "the prompt should mention %q so the model keeps its tasks current", want)
	}
}

// The prompt knows where the run is. The project's AGENTS.md, and the facts of the
// run itself.
func TestThePromptCarriesTheProjectAndTheRun(t *testing.T) {
	cfg := stubProviderConfig(t)

	client, opts, err := run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	o, err := order.Parse([]byte("---\nobjective: go\n---\n"))
	require.NoError(t, err)

	got, err := o.Render("{{ .Workdir }}|{{ .Model }}|{{ .Provider }}|{{ .Date }}|{{ .Project }}|{{ range .Tools }}{{ .Name }},{{ end }}", run.OrderEnv(cfg, client, &opts, "/work/project", "", "Always mention PINECONE."))
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(got, "/work/project|glm-5.2|"+cfg.Provider.Label()+"|"+time.Now().Format("2006-01-02")+"|Always mention PINECONE.|shell,tasks,"))
}

// Agent has no input channel, so an agent that asks a question and waits is fatal in a way no other prompt mistake is. These
// pin the directives that prevent it. They are loose by design, asserting the directive survives a rewrite of the wording,
// since a prompt cannot be tested against a model here.
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
		assert.True(t, directive.pattern.MatchString(instructions), "%s does not %s", where, directive.need)
	}
}

// contractHeading is how the contract is spotted in an assembled prompt.
const contractHeading = "## Non-interactive contract"

// The starter config's prompt carries the contract, once.
func TestTheStarterPromptForbidsWaitingForTheUser(t *testing.T) {
	prompt := defaultPrompt(t)

	assertNonInteractive(t, "the starter prompt", prompt)

	n := strings.Count(prompt, contractHeading)
	assert.Equal(t, 1, n, "the contract appears %d times in the starter prompt, want once", n)
}

// The prompt is the config's and agent adds no instructions to it, so the system message is what its template renders to.
func TestTheAgentIsToldWhatTheConfigsPromptRendersTo(t *testing.T) {
	bodies := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		body, _ := io.ReadAll(r.Body)

		select {
		case bodies <- string(body):
		default:
		}

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	cfg := stubProvider(t)
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}
	cfg.Prompt = "You are a haiku bot. Write only haiku about {{ .Objective }}."

	_, err := testutils.CaptureStdout(t, func() error {
		return run.Run(t.Context(), cfg, testOrder("the sea"), logged(t))
	})
	require.NoError(t, err)

	body := <-bodies

	assert.Contains(t, body, `{"content":"You are a haiku bot. Write only haiku about the sea.","role":"system"}`, "want the rendered prompt as the whole system message")
}

// The settle and call budgets are configurable, and the config values must actually reach the run, or the knob in the
// example config is a lie. The max_settles key matters most, since it sets how hard agent pushes the model to record an outcome.
func TestRunBudgetsComeFromConfig(t *testing.T) {
	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: litHTTPSGwExampleCom, APIKey: litSkTest, Models: declared(litGlm52)}
	cfg.Agent.MaxSettles = 5
	cfg.Agent.MaxCalls = 33

	_, opts, err := run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	assert.Equal(t, 5, opts.MaxSettles)

	assert.Equal(t, 33, opts.MaxCalls)

	// max_time is a duration string on the config, a time.Duration on the run
	cfg.Agent.MaxTime = "30m"

	_, timed, err := run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Minute, timed.MaxDuration)

	// zero passes through as zero. The engine, not the config, owns the default,
	// and it never means "no settling"
	cfg.Agent.MaxSettles = 0

	_, opts, err = run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	assert.Equal(t, 0, opts.MaxSettles, "want the unset value left for the engine to default")
}

// A tool result is bounded by a share of the model's own context window, so a
// model with a small window is held tighter without being told to be.
func TestToolOutputIsCappedAtAShareOfTheWindow(t *testing.T) {
	shellOutput := func(window, percent int) int {
		cfg := testDefaults()
		cfg.Agent.MaxToolOutputPercent = percent
		cfg.Provider = config.ProviderConfig{
			BaseURL: litHTTPSGwExampleCom, APIKey: litSkTest,
			Models: map[string]config.ModelConfig{litGlm52: {Context: window}},
		}

		_, opts, err := run.Resolve(t.Context(), cfg, nil)
		require.NoError(t, err)

		for _, tool := range opts.Tools {
			if tool.Info().Name != tools.ShellTool {
				continue
			}

			response, err := tool.Run(t.Context(), fantasy.ToolCall{
				ID: "c", Name: tools.ShellTool, Input: `{"command":"head -c 600000 /dev/zero | tr '\\0' x"}`,
			})
			require.NoError(t, err)

			return len(response.Content)
		}

		require.FailNow(t, "no shell tool")

		return 0
	}

	small, large := shellOutput(8_000, 0), shellOutput(64_000, 0)

	// 25% of the window in tokens, three bytes to a token, and a marker
	want := 8_000 / 4 * 3
	assert.GreaterOrEqual(t, small, want, "a small window let through %d bytes, want about %d", small, want)
	assert.LessOrEqual(t, small, want+100, "a small window let through %d bytes, want about %d", small, want)

	assert.Greater(t, large, small*4, "the cap does not follow the window")

	tight := shellOutput(64_000, 5)
	assert.Less(t, tight, large/3, "max_tool_output_percent 5 let through %d bytes against %d at the default", tight, large)
}

// The session log is the agent's long-term memory, so the prompt a run really
// sends says where it is.
func TestTheRunTellsTheAgentWhereItsLogIs(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()

		bodies = append(bodies, string(body))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprintf(w, "data: %s\n\n",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"done\"}"}}]},"finish_reason":"tool_calls"}]}`)

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}
	cfg.Prompt = starterPrompt(t)

	path := filepath.Join(t.TempDir(), "orders", "task.jsonl")

	_, err := testutils.CaptureStdout(t, func() error {
		return run.Run(t.Context(), cfg, newOrderNamed(t, "do the thing"), run.Options{Viewer: headlessViewer, SessionPath: path})
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, bodies, 1, "the provider saw %d requests, want 1", len(bodies))

	assert.Contains(t, bodies[0], path, "the recorded run's prompt does not point at its log %s", path)
	assert.Contains(t, bodies[0], "short-term memory", "the recorded run's prompt does not point at its log %s", path)
}

// The config states the context threshold defaults because the rule between them is its own, and the engine has fallbacks
// for a caller building options by hand. They are the same numbers, or a config that says nothing would behave differently
// from an engine that was told nothing.
func TestTheConfigAndTheEngineAgreeOnTheContextDefaults(t *testing.T) {
	defaults := config.Defaults()

	assert.Equal(t, loop.DefaultContextSoft, defaults.Agent.ContextSoft)
	assert.Equal(t, loop.DefaultContextHard, defaults.Agent.ContextHard)

	cfg := stubProviderConfig(t)

	_, opts, err := run.Resolve(t.Context(), cfg, nil)
	require.NoError(t, err)

	assert.Equal(t, loop.DefaultContextSoft, opts.ContextSoft, "a default config resolves to %d/%d, want the engine's %d/%d", opts.ContextSoft, opts.ContextHard, loop.DefaultContextSoft, loop.DefaultContextHard)
	assert.Equal(t, loop.DefaultContextHard, opts.ContextHard, "a default config resolves to %d/%d, want the engine's %d/%d", opts.ContextSoft, opts.ContextHard, loop.DefaultContextSoft, loop.DefaultContextHard)
}
