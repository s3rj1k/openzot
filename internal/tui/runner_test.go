package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/provider"
)

// testWindow is the context window every test engine is given. A window is
// required, and this one is large enough that no test trims by accident.
const testWindow = 1_000_000

// RunAgent is the seam between the engine and the screen. It is a pure pump,
// and the thing worth proving about a pump is that nothing goes missing: every
// event reaches the program, an error reaches it too, and the stream always
// ends with a done message so the viewer knows the run is over rather than
// hanging on a spinner nobody will ever stop.

// scriptedClient answers with the given SSE frame sets, one per turn.
func scriptedClient(t *testing.T, turns ...[]string) *provider.Client {
	t.Helper()

	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		index := turn
		if index >= len(turns) {
			index = len(turns) - 1
		}

		turn++

		for _, frame := range turns[index] {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	return client
}

// headless starts a Bubble Tea program with no terminal attached, collecting
// every message it receives.
func headless(t *testing.T) (*tea.Program, *collector, func() *model) {
	t.Helper()

	seen := &collector{}

	program := tea.NewProgram(
		&recordingModel{collector: seen, inner: newModel("do the thing", litTestModel, litCustom, "/tmp/work")},
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithoutSignalHandler(),
	)

	finished := make(chan tea.Model, 1)

	go func() {
		final, err := program.Run()
		if err != nil {
			t.Errorf("program.Run: %v", err)
		}

		finished <- final
	}()

	return program, seen, func() *model {
		program.Quit()

		select {
		case final := <-finished:
			if recording, ok := final.(*recordingModel); ok {
				return recording.inner
			}

			return &model{}

		case <-time.After(5 * time.Second):
			t.Fatal("the program did not stop")

			return &model{}
		}
	}
}

// collector records what the program was sent.
type collector struct {
	events  []loop.Event
	results []loop.Result
}

// recordingModel wraps the real model, noting the run's messages as they arrive.
type recordingModel struct {
	collector *collector
	inner     *model
}

func (*recordingModel) Init() tea.Cmd { return nil }

func (r *recordingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := msg.(type) {
	case eventMsg:
		r.collector.events = append(r.collector.events, typed.ev)
	case doneMsg:
		r.collector.results = append(r.collector.results, typed.result)
	}

	updated, cmd := r.inner.Update(msg)

	if typed, ok := updated.(*model); ok {
		r.inner = typed
	}

	return r, cmd
}

func (*recordingModel) View() string { return "" }

// engineFor is an engine over the client for a run of "do the thing".
func engineFor(t *testing.T, client *provider.Client, tweak ...func(*loop.Options)) *loop.Engine {
	t.Helper()

	options := loop.Options{
		Client:        client,
		ContextWindow: testWindow,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "do the thing"}},

		// a persistent outage is retried with a growing backoff; these tests are
		// about what reaches the screen, not about waiting an outage out
		RetryBackoff: -1,
	}

	for _, change := range tweak {
		change(&options)
	}

	engine, err := loop.New(&options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return engine
}

func TestRunAgentRelaysEveryEventAndThenDone(t *testing.T) {
	client := scriptedClient(t,
		[]string{
			`{"choices":[{"delta":{"content":"working on it"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		},
		[]string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
	)

	program, seen, stop := headless(t)

	runAgent(t.Context(), program, engineFor(t, client, func(o *loop.Options) { o.MaxSettles = 5 }),
		make(chan loop.Result, 1), make(chan struct{}))

	final := stop()

	if len(seen.results) != 1 {
		t.Fatalf("the done message arrived %d times, want exactly one", len(seen.results))
	}

	if len(seen.events) == 0 {
		t.Fatal("no events reached the program")
	}

	var tokens strings.Builder

	for _, event := range seen.events {
		if event.Kind == loop.EventToken {
			tokens.WriteString(event.Text)
		}
	}

	if !strings.Contains(tokens.String(), "working on it") {
		t.Errorf("the streamed answer did not reach the screen: %q", tokens.String())
	}

	if seen.results[0].Reason != loop.StopSettled || seen.results[0].Message != "all done" {
		t.Errorf("the ending = %+v, want settled with the summary: it is what stops the spinner", seen.results[0])
	}

	if final.status == statusRunning {
		t.Error("the viewer should not still be showing a running run")
	}
}

// A run that cannot reach its provider must surface the failure rather than
// leaving a spinner turning forever.
func TestRunAgentRelaysAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)

		fmt.Fprint(w, `{"error":{"message":"upstream is down"}}`)
	}))

	defer server.Close()

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	program, seen, stop := headless(t)

	runAgent(t.Context(), program, engineFor(t, client), make(chan loop.Result, 1), make(chan struct{}))

	final := stop()

	if len(seen.results) != 1 {
		t.Fatalf("the done message arrived %d times, want exactly one", len(seen.results))
	}

	if seen.results[0].Err == nil {
		t.Fatal("the provider failure never reached the screen")
	}

	if final.runError() == nil {
		t.Error("a failed run must be reportable to the caller")
	}
}

// A canceled run still has to end cleanly: the pump drains and the done
// message arrives, or the viewer never comes back.
func TestRunAgentEndsOnCancellation(t *testing.T) {
	client := scriptedClient(t, []string{
		`{"choices":[{"delta":{"content":"thinking"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	})

	ctx, cancel := context.WithCancel(t.Context())

	cancel()

	program, seen, stop := headless(t)

	runAgent(ctx, program, engineFor(t, client), make(chan loop.Result, 1), make(chan struct{}))

	stop()

	if len(seen.results) != 1 {
		t.Errorf("the done message arrived %d times, want exactly one", len(seen.results))
	}
}

// Quitting the viewer must stop the agent, not merely stop watching it. The
// agent holds shell and file-write access, so returning from the viewer with the
// run still going leaves something editing the working tree with nothing on
// screen reporting what it does. Process exit would hide this in
// the CLI, but the guarantee belongs to the viewer, not to the exit.
func TestQuittingTheViewerStopsTheAgent(t *testing.T) {
	streaming := make(chan struct{})
	canceled := make(chan struct{})

	markStreaming := sync.OnceFunc(func() { close(streaming) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		flusher, _ := w.(http.Flusher)

		if flusher != nil {
			flusher.Flush()
		}

		markStreaming()

		// hold the turn open, the way a model thinking through a long task does,
		// and report whether the client ever went away
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(20 * time.Second):
		}
	}))

	t.Cleanup(server.Close)

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	m := newModel("do the thing", litTestModel, litCustom, t.TempDir())

	// start stands in for the user pressing q: the program runs headlessly, so the
	// event pump is genuinely consuming, and quits once the agent is under way
	start := func(p *tea.Program) (tea.Model, error) {
		go func() {
			<-streaming

			p.Quit()
		}()

		return p.Run()
	}

	if _, err := runViewer(t.Context(), m, engineFor(t, client), start,
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler()); err == nil {
		t.Error("quitting mid-run should report that the run did not finish")
	}

	select {
	case <-canceled:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent was still running after the viewer quit")
	}
}

// Quitting the viewer must hand the caller the run's aborted outcome, for the
// session. RunViewer used to return the moment the program did, racing the
// engine's ending against the caller's deferred session close - a quit run was
// logged as "running/interrupted" forever, with no outcome at all.
func TestQuittingTheViewerStillRecordsTheOutcome(t *testing.T) {
	streaming := make(chan struct{})

	markStreaming := sync.OnceFunc(func() { close(streaming) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		markStreaming()

		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))

	t.Cleanup(server.Close)

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	m := newModel("do the thing", litTestModel, litCustom, t.TempDir())

	// the program runs headlessly so the event pump is genuinely consuming;
	// quitting once the stream is under way is the user pressing q mid-run
	start := func(p *tea.Program) (tea.Model, error) {
		go func() {
			<-streaming

			p.Quit()
		}()

		return p.Run()
	}

	result, _ := runViewer(t.Context(), m, engineFor(t, client), start,
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())

	if result.Reason != loop.StopAborted {
		t.Errorf("Reason = %q, want the abort handed back", result.Reason)
	}
}
