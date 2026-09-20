package run

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

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/skills"
	"github.com/openzot/openzot/internal/tools"
	"github.com/openzot/openzot/internal/tui"
)

// testOrder is an order for a run that never was a file.
func testOrder(objective string) order.Order {
	return order.Order{Objective: objective, Body: "{{ .Objective }}"}
}

func TestLoadProjectContext(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	// A global AGENTS.md in the config dir and a project one in the work dir.
	mustWrite(t, filepath.Join(configDir, "AGENTS.md"), "GLOBAL CONVENTIONS")
	mustWrite(t, filepath.Join(workDir, "AGENTS.md"), "PROJECT CONVENTIONS")

	project := LoadProjectContext(configDir, workDir, workDir)

	// both files are there, the config directory's first, each once
	for _, want := range []string{"GLOBAL CONVENTIONS", "PROJECT CONVENTIONS"} {
		if strings.Count(project, want) != 1 {
			t.Errorf("project context should hold %q once:\n%s", want, project)
		}
	}

	if i, j := strings.Index(project, "GLOBAL"), strings.Index(project, "PROJECT"); i > j {
		t.Error("expected config-dir AGENTS.md to appear before work-dir AGENTS.md")
	}
}

func TestLoadProjectContextNoFiles(t *testing.T) {
	if project := LoadProjectContext(t.TempDir()); project != "" {
		t.Error("expected no project context when no AGENTS.md is present")
	}
}

func TestLoadSkillsFromTheConfiguredFolder(t *testing.T) {
	t.Run("unset means no skills", func(t *testing.T) {
		if loaded, err := LoadSkills(""); err != nil || loaded != nil {
			t.Errorf("skills = %v, err = %v, want none and no error", loaded, err)
		}
	})

	t.Run("a relative folder is taken against the working directory", func(t *testing.T) {
		project := t.TempDir()
		mustWrite(t, filepath.Join(project, "my-skills", "greet", "SKILL.md"), "---\nname: greet\ndescription: say hello\n---\nbody")
		t.Chdir(project)

		loaded, err := LoadSkills("my-skills")
		if err != nil {
			t.Fatalf("LoadSkills: %v", err)
		}

		if len(loaded) != 1 || loaded[0].Name != "greet" || loaded[0].Content == "" {
			t.Errorf("skills = %+v, want greet loaded with its content", loaded)
		}
	})

	t.Run("~ is the home directory", func(t *testing.T) {
		home := t.TempDir()
		mustWrite(t, filepath.Join(home, "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\nbody")
		t.Setenv("HOME", home)

		loaded, err := LoadSkills("~/skills")
		if err != nil {
			t.Fatalf("LoadSkills: %v", err)
		}

		if len(loaded) != 1 || loaded[0].Name != "deploy" {
			t.Errorf("skills = %+v, want deploy from ~/skills", loaded)
		}
	})

	t.Run("a folder that cannot be read stops the run", func(t *testing.T) {
		_, err := LoadSkills(filepath.Join(t.TempDir(), "missing"))
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
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

	offered, err := LoadSkills(skillsDir)
	if err != nil {
		t.Fatal(err)
	}

	// loaded at startup: the folder is not read again during the run
	if err := os.RemoveAll(skillsDir); err != nil {
		t.Fatal(err)
	}

	if _, err := quietly(t, func() error {
		options := logged(t)
		options.Skills = offered

		return Run(t.Context(), cfg, testOrder("do the thing"), options)
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
// than merely accepted by the parser.
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

			path := writeCfg(t, fmt.Sprintf(`
agent:
  model: %q
provider:
  base_url: %s
%s`, test.model, server.URL, test.config))

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			client, _, err := Resolve(t.Context(), cfg, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			if got := client.Config().Model; got != litGpt4 {
				t.Errorf("model = %q", got)
			}

			if _, err := quietly(t, func() error {
				return Run(t.Context(), cfg, testOrder("do the thing"), logged(t))
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
			t.Setenv("ZOT_CONFIG", "")

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

			path := writeCfg(t, fmt.Sprintf(`
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
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if _, err := quietly(t, func() error {
				return Run(t.Context(), cfg, testOrder("do the thing"), logged(t))
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
	cfg.Provider = config.ProviderConfig{APIKey: litSkTest, Models: declared(litGlm52)}

	_, _, err := Resolve(t.Context(), cfg, nil)
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
	cfg.Provider = config.ProviderConfig{BaseURL: litHTTP12700, Models: declared(litGlm52)}

	_, opts, err := Resolve(t.Context(), cfg, nil)
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
		"the window is zero":            {litGlm52: {Model: litGlm52}},
		"the window is negative":        {litGlm52: {Context: -1}},
	}

	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testDefaults()
			cfg.Provider = config.ProviderConfig{BaseURL: litHTTP12700, Models: models}

			_, _, err := Resolve(t.Context(), cfg, nil)
			if err == nil {
				t.Fatal("a model with no context window resolved")
			}

			for _, want := range []string{litGlm52, "context window", "provider.models"} {
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
func TestResolveSelectsTheModelFromTheOneProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	t.Setenv("ALPHA_KEY", "sk-alpha")

	cfg, err := config.Load(writeCfg(t, `
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
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

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

		client, opts, err := Resolve(t.Context(), cfg, nil)
		if err != nil {
			t.Fatalf("resolve(%s): %v", name, err)
		}

		got := client.Config()

		if got.Model != want.model || got.ReasoningEffort != want.effort || opts.ContextWindow != want.window || opts.MaxIterations != want.iterations {
			t.Errorf("%s resolved to model %q effort %q window %d iterations %d, want %+v",
				name, got.Model, got.ReasoningEffort, opts.ContextWindow, opts.MaxIterations, want)
		}

		// one provider, so one endpoint and one credential whatever the model
		if got.BaseURL != "https://alpha.example.com/v1" || got.APIKey != "sk-alpha" || got.Provider != "alpha.example.com" {
			t.Errorf("%s talks to %q as %q with key %q", name, got.BaseURL, got.Provider, got.APIKey)
		}
	}

	cfg.Agent.Model = "huge"

	if _, _, err := Resolve(t.Context(), cfg, nil); err == nil || !strings.Contains(err.Error(), "context window") {
		t.Errorf("a model the provider does not list resolved: %v", err)
	}
}

// Nothing is built in: with no provider declared a run does not resolve, whatever
// the environment holds.
func TestNoProviderIsBuiltIn(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ZAI_API_KEY", "sk-zai")

	if _, _, err := Resolve(t.Context(), testDefaults(), nil); err == nil {
		t.Error("a run resolved with no provider declared")
	}
}

// A model entry aliases a real id and caps iterations, both of which take
// priority over the run defaults.
func TestResolveCustomModelAlias(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZOT_CONFIG", "")
	path := writeCfg(t, `
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
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	client, opts, err := Resolve(t.Context(), cfg, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := client.Config().Model; got != litGpt5 {
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
	cfg.Agent.Model = "capped"
	cfg.Agent.MaxIterations = 100
	cfg.Provider = config.ProviderConfig{
		BaseURL: litHTTPSGwExampleCom,
		APIKey:  litSkTest,
		Models: map[string]config.ModelConfig{
			"capped": {Model: litGpt5, MaxIterations: 40, Context: 100_000},
		},
	}

	_, opts, err := Resolve(t.Context(), cfg, nil)
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
	cfg.Provider.Models["capped"] = config.ModelConfig{Model: litGpt5, Context: 100_000}

	_, opts, err = Resolve(t.Context(), cfg, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := viewerMeta(cfg, "a task", "/somewhere", opts).MaxIterations; got != 0 {
		t.Errorf("the viewer shows a limit of %d, want the backstop hidden", got)
	}
}

// Run is the whole thing end to end: config in, a provider call out, a
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

	err := Run(t.Context(), cfg, testOrder("do the thing"), logged(t))

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
	cfg.Provider = config.ProviderConfig{}

	err := Run(t.Context(), cfg, testOrder("task"), logged(t))
	if err == nil {
		t.Fatal("an unconfigured provider must fail")
	}

	if !strings.Contains(err.Error(), "provider:") {
		t.Errorf("the error should say to declare the provider: %v", err)
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
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

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

// A run leaves a record of itself: what it was asked, which model answered, and
// how it ended.
func TestRunWithRecordsASession(t *testing.T) {
	cfg := stubProvider(t)

	path := filepath.Join(t.TempDir(), ".zot", "orders", "task.jsonl")

	if _, err := quietly(t, func() error {
		return Run(t.Context(), cfg, testOrder("do the thing"), Options{Viewer: headlessViewer, SessionPath: path})
	}); err != nil {
		t.Fatalf("RunWith: %v", err)
	}

	records := readSession(t, path)

	first, last := records[0], records[len(records)-1]

	if first.Kind != session.KindMeta || first.Meta == nil {
		t.Fatalf("the log must open with the meta: %+v", first)
	}

	if first.Meta.Task != "do the thing" || first.Meta.Provider != cfg.Provider.Label() {
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

	for i := range 2 {
		if output, err := quietly(t, func() error {
			return Run(t.Context(), cfg, testOrder("the same brief"), Options{Viewer: headlessViewer, SessionPath: path})
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
	cfg.Provider = config.ProviderConfig{BaseURL: server.URL, APIKey: "k", Models: declared(litGlm52)}

	if output, err := quietly(t, func() error {
		return Run(t.Context(), cfg, testOrder("do the thing"), Options{Viewer: headlessViewer, SessionPath: path})
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

// A run that cannot be recorded is refused before the provider is asked
// anything: its log is its record and its agent's long-term memory.
func TestARunWithAnUnwritableSessionLogIsRefused(t *testing.T) {
	var asked atomic.Int32

	cfg := stubProvider(t)

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	t.Cleanup(server.Close)

	cfg.Provider.BaseURL = server.URL

	blocked := filepath.Join(t.TempDir(), "a-file")

	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := quietly(t, func() error {
		return Run(t.Context(), cfg, testOrder("do the thing"), Options{
			Viewer:      headlessViewer,
			SessionPath: filepath.Join(blocked, "task.jsonl"),
		})
	})
	if err == nil || !strings.Contains(err.Error(), "session log") {
		t.Fatalf("err = %v, want the unwritable log refused and named\n%s", err, output)
	}

	if asked.Load() != 0 {
		t.Error("the provider was asked something before the log was known to work")
	}
}

// There is no run without a log: the caller must say where it goes.
func TestARunWithNoSessionLogIsRefused(t *testing.T) {
	dir := t.TempDir()

	t.Chdir(dir)

	_, err := quietly(t, func() error {
		return Run(t.Context(), stubProvider(t), testOrder("do the thing"), Options{Viewer: headlessViewer})
	})
	if err == nil || !strings.Contains(err.Error(), "session log") {
		t.Fatalf("err = %v, want a run with no log refused", err)
	}

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused run wrote %d entries", len(entries))
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
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Errorf("Validate = %v, want it to say to declare a provider", err)
	}

	// and the library entry point, which does not validate, says the same
	err = Run(t.Context(), cfg, testOrder("task"), logged(t))
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Errorf("Run = %v, want it to say to declare a provider", err)
	}
}

// logged is the options of a run that is recorded, as every run must be.
func logged(t *testing.T) Options {
	t.Helper()

	return Options{Viewer: headlessViewer, SessionPath: filepath.Join(t.TempDir(), "task.jsonl")}
}

// promptOf renders an order the way a run does, with the tools a run really has.
func promptOf(t *testing.T, o order.Order) string {
	t.Helper()

	client, opts, err := Resolve(t.Context(), stubProviderConfig(t), nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	prompt, err := o.Render(orderEnv(stubProviderConfig(t), client, opts, "/work", "", ""))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	return prompt
}

// stubProviderConfig is a config that resolves without a network.
func stubProviderConfig(t *testing.T) config.Config {
	t.Helper()

	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: litHTTP12700, APIKey: "k", Models: declared(litGlm52)}

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

		if !known[name] && phantom[name] {
			t.Errorf("the prompt names %q, which is not a known tool", name)
		}
	}

	// and positively assert the tools the prompt promises are all present
	for _, want := range []string{"tasks", "shell", "success", "failure"} {
		if !known[want] {
			t.Errorf("the prompt relies on %q but it is not a known tool", want)
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
	offered := []skills.Skill{{Name: "deploy", Description: "ship it"}}

	client, opts, err := Resolve(t.Context(), cfg, offered)
	if err != nil {
		t.Fatal(err)
	}

	with, err := newOrderNamed(t, "x").Render(orderEnv(cfg, client, opts, "/work", "", ""))
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

	client, opts, err := Resolve(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	o, err := order.Parse([]byte("---\nobjective: go\n---\n{{ .Workdir }}|{{ .Model }}|{{ .Provider }}|{{ .Date }}|{{ .Project }}|{{ range .Tools }}{{ .Name }},{{ end }}"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := o.Render(orderEnv(cfg, client, opts, "/work/project", "", "Always mention PINECONE."))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(got, "/work/project|glm-5.2|"+cfg.Provider.Label()+"|"+time.Now().Format("2006-01-02")+"|Always mention PINECONE.|shell,tasks,") {
		t.Errorf("rendered = %q", got)
	}
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
// Max_settles is the one the operator most wants: how hard zot pushes the model
// to record an outcome before giving up.
func TestRunBudgetsComeFromConfig(t *testing.T) {
	cfg := testDefaults()
	cfg.Provider = config.ProviderConfig{BaseURL: litHTTPSGwExampleCom, APIKey: litSkTest, Models: declared(litGlm52)}
	cfg.Agent.MaxSettles = 5
	cfg.Agent.MaxCalls = 33

	_, opts, err := Resolve(t.Context(), cfg, nil)
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

	_, timed, err := Resolve(t.Context(), cfg, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if timed.MaxDuration != 30*time.Minute {
		t.Errorf("MaxDuration = %v, want 30m", timed.MaxDuration)
	}

	// zero passes through as zero: the engine, not the config, owns the default,
	// and it never means "no settling"
	cfg.Agent.MaxSettles = 0

	_, opts, err = Resolve(t.Context(), cfg, nil)
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
		cfg.Agent.MaxToolOutputPercent = percent
		cfg.Provider = config.ProviderConfig{
			BaseURL: litHTTPSGwExampleCom, APIKey: litSkTest,
			Models: map[string]config.ModelConfig{litGlm52: {Context: window}},
		}

		_, opts, err := Resolve(t.Context(), cfg, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}

		for _, tool := range opts.Tools {
			if tool.Info().Name != tools.ShellTool {
				continue
			}

			response, err := tool.Run(t.Context(), fantasy.ToolCall{
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

	path := filepath.Join(t.TempDir(), "orders", "task.jsonl")

	if _, err := quietly(t, func() error {
		return Run(t.Context(), cfg, newOrderNamed(t, "do the thing"), Options{Viewer: headlessViewer, SessionPath: path})
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 1 {
		t.Fatalf("the provider saw %d requests, want 1", len(bodies))
	}

	if !strings.Contains(bodies[0], path) || !strings.Contains(bodies[0], "short-term memory") {
		t.Errorf("the recorded run's prompt does not point at its log %s", path)
	}
}

// The config states the defaults of the context thresholds because the rule
// between them is its own; the engine has fallbacks for a caller building its
// options by hand. They are the same numbers, or a config that says nothing would
// behave differently from an engine that was told nothing.
func TestTheConfigAndTheEngineAgreeOnTheContextDefaults(t *testing.T) {
	defaults := config.Defaults()

	if defaults.Agent.ContextSoft != loop.DefaultContextSoft || defaults.Agent.ContextHard != loop.DefaultContextHard {
		t.Errorf("config defaults %d/%d, engine defaults %d/%d",
			defaults.Agent.ContextSoft, defaults.Agent.ContextHard, loop.DefaultContextSoft, loop.DefaultContextHard)
	}

	cfg := stubProviderConfig(t)

	_, opts, err := Resolve(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	if opts.ContextSoft != loop.DefaultContextSoft || opts.ContextHard != loop.DefaultContextHard {
		t.Errorf("a default config resolves to %d/%d, want the engine's %d/%d",
			opts.ContextSoft, opts.ContextHard, loop.DefaultContextSoft, loop.DefaultContextHard)
	}
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

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// declared is the model list a provider needs to run the named models: each
// with a context window, since a model without one cannot run.
func declared(names ...string) map[string]config.ModelConfig {
	models := make(map[string]config.ModelConfig, len(names))

	for _, name := range names {
		models[name] = config.ModelConfig{Context: 100_000}
	}

	return models
}

// testDefaults is the built-in configuration with the one thing it deliberately
// lacks: a model to run.
func testDefaults() config.Config {
	cfg := config.Defaults()
	cfg.Agent.Model = litGlm52

	return cfg
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

// contractHeading is how the contract is spotted in an assembled prompt.
const contractHeading = "## Non-interactive contract"
