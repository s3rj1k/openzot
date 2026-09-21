package render_test

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/openzot/openzot/internal/render"
	"github.com/openzot/openzot/internal/testutils"
)

// A shell tool reports failure on stderr, and that is exactly the output an
// operator reading a failed run needs to see.
func TestCommandOutputPrefersStdoutButFallsBackToStderr(t *testing.T) {
	assert.Contains(t, render.CommandOutput(map[string]any{litStdout: "all good\n"}), "all good")

	got := render.CommandOutput(map[string]any{litStdout: "", "stderr": "permission denied\n"})

	assert.Contains(t, got, "permission denied", "stderr was not rendered when stdout was empty")

	got = render.CommandOutput(map[string]any{})
	assert.Empty(t, got, "a silent command rendered %q, want nothing", got)
}

func TestFmtTokens(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{532, "532"},
		{45200, "45.2k"},
		{1_200_000, "1.2M"},
	} {
		assert.Equal(t, tc.want, render.FmtTokens(tc.n))
	}
}

func TestFormattedDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		want     string
	}{
		{duration: 0, want: "00:00"},
		{duration: 45 * time.Second, want: "00:45"},
		{duration: 90 * time.Second, want: "01:30"},
		{duration: 61 * time.Minute, want: "61:00"},
	}

	for _, test := range tests {
		assert.Equal(t, test.want, render.FmtDuration(test.duration))
	}
}

// A call the tool rejects - no tasks, an unknown status - still shows its header
// rather than crashing the render, and draws nothing it cannot vouch for.
func TestRenderTasksIsRobust(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"no arguments":  {},
		"an empty list": {litTasks: []any{}},
		"a bad status":  testutils.TasksArgs([3]string{"a", "started", ""}),
		"not a list":    {litTasks: "do it"},
		"a non-object":  {litTasks: []any{"do it"}},
	} {
		out := testutils.StripANSI(render.RenderToolStart(litTasks, args))

		assert.Contains(t, out, litTasks, "%s: should still render a header", name)

		assert.NotContains(t, out, litDone, "%s: a refused list must not report progress", name)
	}
}

// The task list is the one piece of the run worth reading in full, so it renders
// as a checklist. What is done, what is under way, what is left, what is stuck.
func TestRenderTasksShowsTheChecklist(t *testing.T) {
	out := testutils.StripANSI(render.RenderToolStart(litTasks, testutils.TasksArgs(
		[3]string{"read the handler", litDone, ""},
		[3]string{"add validation", "in_progress", "the error path is missing"},
		[3]string{"write a test", "pending", ""},
		[3]string{"deploy", "blocked", "needs credentials"},
	)))

	for _, want := range []string{
		litTasks, "1/4 done",
		"✓ read the handler", "▶ add validation", "· write a test", "✗ deploy",
		"the error path is missing", "needs credentials",
	} {
		assert.Contains(t, out, want, "rendered tasks missing %q", want)
	}
}

// agent's tools return strings, so a summary that only understood maps rendered
// nothing at all.
func TestRenderToolEndHandlesStringResults(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		result  any
		wantAny bool
		want    string
	}{
		{"shell echoes its output", litShell, "hello\nworld", true, "hello"},
		{"a silent command still confirms", litShell, "", true, litDone},
		{"an unknown tool echoes", litCustom, "some output", true, "some output"},
		{"an unknown tool with nothing to say", litCustom, "", false, ""},
		{"a non-string, non-map result", litShell, 42, false, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := testutils.StripANSI(render.RenderToolEnd(test.tool, test.result))

			if !test.wantAny {
				assert.Empty(t, got)

				return
			}

			assert.Contains(t, got, test.want)
		})
	}
}

func TestRenderToolEndHandlesStructuredResults(t *testing.T) {
	failure := testutils.StripANSI(render.RenderToolEnd(litShell, map[string]any{
		"success": false,
		"error":   "exit status 1",
		"stderr":  "compile failed",
	}))

	assert.Contains(t, failure, "exit status 1")

	success := testutils.StripANSI(render.RenderToolEnd(litShell, map[string]any{litStdout: "all good"}))

	assert.Contains(t, success, "all good", "structured output must surface")
}

// The renderers have to know the real tool names. A mismatch is not a compile
// error - it just renders the agent's most-used tool as an anonymous
// key/value dump, which is how `shell` went unstyled.
func TestRenderToolStartCoversTheBuiltInTools(t *testing.T) {
	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{litShell, map[string]any{"command": "go test ./..."}, "go test"},

		// a caller's own tool still renders, just generically
		{litCustom, map[string]any{"thing": "value"}, "thing=value"},
	}

	for _, test := range tests {
		got := testutils.StripANSI(render.RenderToolStart(test.tool, test.args))

		assert.Contains(t, got, test.want)
	}
}

func TestTruncateAddsAnEllipsisAndFlattensNewlines(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{in: "short", max: 10, want: "short"},
		{in: "exactly-10", max: 10, want: "exactly-10"},
		{in: "a longer string", max: 8, want: "a longe…"},
		{in: "two\nlines", max: 20, want: "two lines"},
		{in: "two\nlines here", max: 6, want: "two l…"},

		// A task or tool argument in CJK or emoji was cut mid-rune, rendering a replacement character, and the cap
		// counted bytes, so the line was cut far short of its width.
		{in: "日本語のタスク説明文です", max: 6, want: "日本語のタ…"},
		{in: "🚀🚀🚀🚀🚀", max: 3, want: "🚀🚀…"},
		{in: "日本語", max: 10, want: "日本語"},
	}

	for _, test := range tests {
		assert.Equal(t, test.want, render.Truncate(test.in, test.max))
	}
}

// A path is read from its end. The last segment names the project, the first
// are shared by every project on the machine. Cutting the head is the whole
// point - truncate does the opposite and was wrong here.
func TestShortPathKeepsTheInformativeEnd(t *testing.T) {
	tests := []struct {
		name string
		path string
		max  int
		want string
	}{
		{
			name: "a path that fits is left alone",
			path: litSrvAPI,
			max:  28,
			want: litSrvAPI,
		},
		{
			name: "leading segments are dropped, not trailing characters",
			path: "/workspaces/monorepo-agent/repos/agent/tool",
			max:  28,
			want: "…/repos/agent/tool",
		},
		{
			name: "segments are kept whole rather than half-named",
			path: "/home/vscode/development/projects/webservice",
			max:  20,
			want: "…/webservice",
		},
		{
			name: "a final segment that cannot fit is cut from the left",
			path: "/srv/an-extremely-long-directory-name-here",
			max:  12,
			want: "…y-name-here",
		},
		{
			name: "a trailing separator does not become an empty segment",
			path: "/workspaces/monorepo-agent/repos/agent/tool/",
			max:  28,
			want: "…/repos/agent/tool",
		},
		{
			name: "no room at all yields nothing rather than a stray ellipsis",
			path: litSrvAPI,
			max:  0,
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := render.ShortPath(test.path, test.max)

			assert.Equal(t, test.want, got)

			// whatever it returns must actually fit the budget it was given
			n := utf8.RuneCountInString(got)
			assert.LessOrEqual(t, n, test.max, "shortPath(%q, %d) is %d columns wide: %q", test.path, test.max, n, got)
		})
	}
}
