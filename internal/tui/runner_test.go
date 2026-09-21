package tui_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3rj1k/agent/internal/conversation"
	"github.com/s3rj1k/agent/internal/loop"
	"github.com/s3rj1k/agent/internal/outcome"
	"github.com/s3rj1k/agent/internal/testutils"
	"github.com/s3rj1k/agent/internal/tui"
)

// testWindow is the context window every test engine is given. A window is
// required, and this one is large enough that no test trims by accident.
const testWindow = 1_000_000

// The RunAgent function is the seam between the engine and the screen, a pure pump, so what is worth proving is that nothing goes
// missing. Every event and any error reaches the program, and the stream always ends with a done message so the viewer
// never hangs on a spinner nobody will stop.

// headless starts a Bubble Tea program with no terminal attached, collecting
// every message it receives.
func headless(t *testing.T) (*tea.Program, *collector, func() *tui.Model) {
	t.Helper()

	seen := &collector{}

	program := tea.NewProgram(
		&recordingModel{collector: seen, inner: tui.NewModel("do the thing", litTestModel, litCustom, "/tmp/work")},
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithoutSignalHandler(),
	)

	finished := make(chan tea.Model, 1)

	go func() {
		final, err := program.Run()
		assert.NoError(t, err)

		finished <- final
	}()

	return program, seen, func() *tui.Model {
		program.Quit()

		select {
		case final := <-finished:
			if recording, ok := final.(*recordingModel); ok {
				return recording.inner
			}

			return &tui.Model{}

		case <-time.After(5 * time.Second):
			require.FailNow(t, "the program did not stop")

			return &tui.Model{}
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
	inner     *tui.Model
}

func (*recordingModel) Init() tea.Cmd { return nil }

func (r *recordingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := msg.(type) {
	case tui.EventMsg:
		r.collector.events = append(r.collector.events, typed.Event)
	case tui.DoneMsg:
		r.collector.results = append(r.collector.results, typed.Result)
	}

	updated, cmd := r.inner.Update(msg)

	if typed, ok := updated.(*tui.Model); ok {
		r.inner = typed
	}

	return r, cmd
}

func (*recordingModel) View() string { return "" }

// engineFor is an engine over the model for a run of "do the thing".
func engineFor(t *testing.T, model fantasy.LanguageModel, tweak ...func(*loop.Options)) *loop.Engine {
	t.Helper()

	options := loop.Options{
		Model:         model,
		ContextWindow: testWindow,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "do the thing"}},

		// a persistent outage is retried with a growing backoff. These tests are
		// about what reaches the screen, not about waiting an outage out
		RetryBackoff: -1,
	}

	for _, change := range tweak {
		change(&options)
	}

	engine, err := loop.New(&options)
	require.NoError(t, err)

	return engine
}

func TestRunAgentRelaysEveryEventAndThenDone(t *testing.T) {
	client := testutils.ScriptedModel(t,
		[]string{
			`{"choices":[{"delta":{"content":"working on it"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		},
		[]string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"all done\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
	)

	program, seen, stop := headless(t)

	tui.RunAgent(t.Context(), program, engineFor(t, client, func(o *loop.Options) { o.MaxSettles = 5 }),
		make(chan loop.Result, 1), make(chan struct{}))

	final := stop()

	require.Len(t, seen.results, 1, "want exactly one")

	require.NotEmpty(t, seen.events)

	var tokens strings.Builder

	for _, event := range seen.events {
		if event.Kind == loop.EventToken {
			tokens.WriteString(event.Text)
		}
	}

	assert.Contains(t, tokens.String(), "working on it", "the streamed answer did not reach the screen")

	assert.Equal(t, outcome.StopSettled, seen.results[0].Reason, "want settled with the summary: it is what stops the spinner")
	assert.Equal(t, "all done", seen.results[0].Message, "want settled with the summary: it is what stops the spinner")

	assert.NotEqual(t, tui.StatusRunning, final.Status, "the viewer should not still be showing a running run")
}

// A run that cannot reach its provider must surface the failure rather than
// leaving a spinner turning forever.
func TestRunAgentRelaysAFailure(t *testing.T) {
	client := testutils.Script(t, testutils.Reject(http.StatusInternalServerError, `{"error":{"message":"upstream is down"}}`)).Model(t)

	program, seen, stop := headless(t)

	tui.RunAgent(t.Context(), program, engineFor(t, client), make(chan loop.Result, 1), make(chan struct{}))

	final := stop()

	require.Len(t, seen.results, 1, "want exactly one")

	require.Error(t, seen.results[0].Err, "the provider failure never reached the screen")

	require.Error(t, final.RunError(), "a failed run must be reportable to the caller")
}

// A canceled run still has to end cleanly. The pump drains and the done
// message arrives, or the viewer never comes back.
func TestRunAgentEndsOnCancellation(t *testing.T) {
	client := testutils.ScriptedModel(t, []string{
		`{"choices":[{"delta":{"content":"thinking"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	})

	ctx, cancel := context.WithCancel(t.Context())

	cancel()

	program, seen, stop := headless(t)

	tui.RunAgent(ctx, program, engineFor(t, client), make(chan loop.Result, 1), make(chan struct{}))

	stop()

	assert.Len(t, seen.results, 1, "want exactly one")
}

// Quitting the viewer must stop the agent, not only the watching. The agent holds shell and file-write access, so a run
// left going with nothing on screen would keep editing the working tree unseen. Process exit would hide this in the CLI,
// but the guarantee belongs to the viewer.
func TestQuittingTheViewerStopsTheAgent(t *testing.T) {
	streaming := make(chan struct{})
	canceled := make(chan struct{})

	markStreaming := sync.OnceFunc(func() { close(streaming) })

	client := testutils.Raw(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})).Model(t)

	m := tui.NewModel("do the thing", litTestModel, litCustom, t.TempDir())

	// start stands in for the user pressing q. The program runs headlessly, so the
	// event pump is really consuming, and quits once the agent is under way
	start := func(p *tea.Program) (tea.Model, error) {
		go func() {
			<-streaming

			p.Quit()
		}()

		return p.Run()
	}

	_, err := tui.RunViewer(t.Context(), m, engineFor(t, client), start,
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())
	require.Error(t, err, "quitting mid-run should report that the run did not finish")

	select {
	case <-canceled:
	case <-time.After(15 * time.Second):
		require.FailNow(t, "the agent was still running after the viewer quit")
	}
}

// Quitting the viewer must hand the caller the run's aborted outcome, for the session. The viewer once returned as soon
// as the program did, racing the engine's ending against the session close, so a quit run stayed "running/interrupted".
func TestQuittingTheViewerStillRecordsTheOutcome(t *testing.T) {
	streaming := make(chan struct{})

	markStreaming := sync.OnceFunc(func() { close(streaming) })

	client := testutils.Raw(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		markStreaming()

		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	})).Model(t)

	m := tui.NewModel("do the thing", litTestModel, litCustom, t.TempDir())

	// the program runs headlessly so the event pump is really consuming.
	// Quitting once the stream is under way is the user pressing q mid-run
	start := func(p *tea.Program) (tea.Model, error) {
		go func() {
			<-streaming

			p.Quit()
		}()

		return p.Run()
	}

	result, _ := tui.RunViewer(t.Context(), m, engineFor(t, client), start,
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())

	assert.Equal(t, outcome.StopAborted, result.Reason, "want the abort handed back")
}
