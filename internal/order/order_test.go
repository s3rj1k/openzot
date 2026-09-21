package order

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// simple is a valid order with the given prompt.
func simple(body string) string {
	return "---\nobjective: do the thing\n---\n" + body
}

func TestLoadReadsAFullOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.md")

	write(t, path, `
---
title: Rate limiting
objective: |-
  add rate limiting to the API
acceptance:
  - "requests beyond the limit receive 429"
  - "  the suite passes  "
  - ""
constraints:
  - do not change handler signatures
---

You are an agent.

{{ .Objective }}
`)

	order, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, litRateLimiting, order.Title)
	assert.Equal(t, "add rate limiting to the API", order.Objective)

	assert.Len(t, order.Acceptance, 2, "want two trimmed criteria and no blank one")
	assert.Equal(t, "the suite passes", order.Acceptance[1], "want two trimmed criteria and no blank one")

	assert.Len(t, order.Constraints, 1)

	assert.Contains(t, order.Body, "You are an agent.", "want the prompt after the front matter")
	assert.Contains(t, order.Body, "{{ .Objective }}", "want the prompt after the front matter")

	assert.Equal(t, path, order.Path)
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"a bare YAML order has no front matter", "objective: do it\n", "no front matter"},
		{"an empty file", "", "no front matter"},
		{"front matter that is never closed", "---\nobjective: do it\n", "not closed"},
		{"an empty objective", "---\nobjective:\n---\nbody", "no objective"},
		{"no objective key", "---\ntitle: x\n---\nbody", "no objective"},
		{"no prompt", "---\nobjective: do it\n---\n  \n\n", "no prompt"},
		//nolint:misspell // the misspelled key is the fixture
		{"an unknown front matter key", "---\nobjective: do it\nacceptence:\n  - x\n---\nbody", "acceptence"},
		{"front matter that is not YAML", "---\nobjective: [unclosed\n---\nbody", "front matter"},
		{"a prompt that does not parse", simple("{{ .Objective "), "prompt"},
		{"a prompt naming a field that does not exist", simple("{{ .Objectve }}"), "Objectve"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.content))
			require.Error(t, err)

			assert.Contains(t, err.Error(), test.want)
		})
	}

	_, err := Load(filepath.Join(t.TempDir(), "missing.md"))
	require.Error(t, err, "a missing file must fail")
}

// A field that only exists on the branch a real run takes is still found at
// load. The stand-in data fills every list, so the branch is exercised.
func TestATypoInABranchIsFoundAtLoad(t *testing.T) {
	_, err := Parse([]byte(simple("{{ if .Acceptance }}{{ .Acceptnce }}{{ end }}")))
	require.Error(t, err, "want the misspelled field named")
	assert.Contains(t, err.Error(), "Acceptnce", "want the misspelled field named")

	_, err = Parse([]byte(simple("{{ range .Constraints }}{{ .Nope }}{{ end }}")))
	require.Error(t, err, "a bad field inside a range over an empty list must still be found")
}

// Blank lines before the opening line are harmless. A --- inside the prompt is
// just prompt.
func TestFrontMatterSplitting(t *testing.T) {
	order, err := Parse([]byte("\n\n---\r\nobjective: go\r\n---\r\nabove\n---\nbelow\n"))
	require.NoError(t, err)

	assert.Equal(t, "go", order.Objective)

	assert.Contains(t, order.Body, "above\n---\nbelow", "want the rest of the file, its own --- included")
}

var testEnv = Env{
	Tools:    []Tool{{Name: "shell", Description: "runs commands"}, {Name: "tasks", Description: "keeps the plan"}},
	Workdir:  "/work/project",
	Date:     "2026-09-19",
	Model:    "glm-5.2",
	Provider: "gateway",
	Project:  "Always mention PINECONE.",
}

func TestRenderSubstitutesTheOrderAndTheRun(t *testing.T) {
	order, err := Parse([]byte(`---
title: The Thing
objective: fix the parser
acceptance:
  - it parses
  - tests pass
constraints:
  - no new deps
---
{{ .Title }} | {{ .Objective }}
{{ range $i, $a := .Acceptance }}{{ inc $i }}) {{ $a }}
{{ end }}{{ range .Constraints }}- {{ . }}
{{ end }}{{ range .Tools }}[{{ .Name }}: {{ .Description }}]{{ end }}
{{ .Workdir }} {{ .Date }} {{ .Model }} {{ .Provider }}
{{ .Project }}
`))
	require.NoError(t, err)

	got, err := order.Render(testEnv)
	require.NoError(t, err)

	for _, want := range []string{
		"The Thing | fix the parser",
		"1) it parses", "2) tests pass",
		"- no new deps",
		"[shell: runs commands][tasks: keeps the plan]",
		"/work/project 2026-09-19 glm-5.2 gateway",
		"Always mention PINECONE.",
	} {
		assert.Contains(t, got, want, "rendered prompt is missing %q", want)
	}
}

func TestTheFileAndEnvFunctions(t *testing.T) {
	workdir := t.TempDir()

	write(t, filepath.Join(workdir, "style.txt"), "use tabs\n")

	absolute := filepath.Join(t.TempDir(), "abs.txt")
	write(t, absolute, "absolute")

	t.Setenv("ZOT_TEST_VALUE", "from-the-environment")

	order, err := Parse([]byte(simple(`{{ file "style.txt" }}|{{ file "` + absolute + `" }}|{{ env "ZOT_TEST_VALUE" }}|{{ env "ZOT_TEST_UNSET" }}|`)))
	require.NoError(t, err)

	got, err := order.Render(Env{Workdir: workdir})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(got, "use tabs|absolute|from-the-environment||"))

	// a file that is not there is an error, not an empty string. A prompt that
	// silently lost its style guide is a worse failure than one that says so
	missing, err := Parse([]byte(simple(`{{ file "nope.txt" }}`)))
	require.NoError(t, err, "Parse must not read files")

	_, err = missing.Render(Env{Workdir: workdir})
	require.Error(t, err, "rendering a prompt that includes a missing file must fail")
}

func TestFileExpandsTheHomeDirectory(t *testing.T) {
	home := t.TempDir()

	t.Setenv("HOME", home)
	write(t, filepath.Join(home, "notes.txt"), "from home")

	order, err := Parse([]byte(simple(`{{ file "~/notes.txt" }}`)))
	require.NoError(t, err)

	got, err := order.Render(Env{Workdir: t.TempDir()})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "from home"))
}

// Whatever the prompt says, the contract is in what the agent is given - once.
func TestTheContractIsAlwaysThereExactlyOnce(t *testing.T) {
	bare, err := Parse([]byte(simple("Start by {{ .Objective }}.")))
	require.NoError(t, err)

	got, err := bare.Render(testEnv)
	require.NoError(t, err)

	assert.Equal(t, 1, strings.Count(got, Contract), "a prompt without the contract must get it, once, after its own text")
	assert.True(t, strings.HasPrefix(got, "Start by do the thing."), "a prompt without the contract must get it, once, after its own text")

	withIt, err := Parse([]byte(simple("Rules.\n\n{{ .Contract }}\n\nGo.")))
	require.NoError(t, err)

	got, err = withIt.Render(testEnv)
	require.NoError(t, err)

	assert.Equal(t, 1, strings.Count(got, Contract), "a prompt that already carries the contract must not get it again")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(got), "Go."), "a prompt that already carries the contract must not get it again")
}

// The scaffold is a blank form. It must be written before it can run, and once it
// is, the default prompt it carries has to render into the prompt zot has always
// run with.
func TestBlankIsNotRunnableUntilTheObjectiveIsWritten(t *testing.T) {
	_, err := Parse([]byte(Blank()))
	require.Error(t, err, "the blank form parsed as an order with an objective")

	filled := strings.Replace(Blank(), "objective:\n", "objective: fix the typo\n", 1)

	order, err := Parse([]byte(filled))
	require.NoError(t, err)

	got, err := order.Render(testEnv)
	require.NoError(t, err)

	for _, want := range []string{
		"You are zot",
		`- "shell": runs commands`,
		`- "tasks": keeps the plan`,
		"Operating rules:",
		"# Project context\n\nAlways mention PINECONE.",
		"## Your task\n\nfix the typo",
	} {
		assert.Contains(t, got, want, "the default prompt is missing %q", want)
	}

	assert.Equal(t, 1, strings.Count(got, Contract), "the default prompt must carry the contract once")

	// with no project context the section is left out, not left empty
	got, err = order.Render(Env{Tools: testEnv.Tools})
	require.NoError(t, err)

	assert.NotContains(t, got, "# Project context", "no project context, no heading")
}

// The task section reads as it always has. The goal, then the criteria as a
// numbered list, then the constraints as bullets.
func TestTheDefaultTaskSectionListsCriteriaAndConstraints(t *testing.T) {
	filled := strings.Replace(Blank(), "objective:\n", "objective: build it\nacceptance:\n  - a works\n  - b works\nconstraints:\n  - keep it small\n", 1)

	order, err := Parse([]byte(filled))
	require.NoError(t, err)

	got, err := order.Render(Env{})
	require.NoError(t, err)

	want := "## Your task\n\nbuild it\n\n" +
		"Acceptance criteria - the objective is not met until every one of these holds:\n1. a works\n2. b works\n\n" +
		"Constraints - these hold for the whole run:\n- keep it small"

	assert.Contains(t, got, want, "the task section is not laid out as it was")
}

// A title is a label for people. A declared one wins. Without one the file name
// is already a perfectly good name, because order files are named from their
// goal. Sentence case, not Title Case - the name is a sentence.
func TestDisplayTitlePrefersTheDeclaredOneThenTheFileName(t *testing.T) {
	tests := []struct {
		name  string
		order Order
		want  string
	}{
		{
			name:  "a declared title wins",
			order: Order{Title: litRateLimiting, Path: "/book/.zot/orders/add-rate-limiting-to-the-api.md"},
			want:  litRateLimiting,
		},
		{
			name:  "the file name becomes one",
			order: Order{Path: "/book/.zot/orders/fix-the-flaky-test.md"},
			want:  "Fix the flaky test",
		},
		{
			name:  "underscores read as spaces too",
			order: Order{Path: "fix_the_flaky_test.md"},
			want:  "Fix the flaky test",
		},
		{
			name:  "a one-word name still capitalises",
			order: Order{Path: "cleanup.md"},
			want:  "Cleanup",
		},
		{
			name:  "an already-capitalised name is left alone",
			order: Order{Path: "API-cleanup.md"},
			want:  "API cleanup",
		},
		{
			name:  "a name that is only separators yields nothing to show",
			order: Order{Path: "---.md"},
			want:  "",
		},
		{
			name:  "an order that was never a file has no name to show",
			order: Order{Objective: "a synthesized order with a very long objective nobody wants as a title"},
			want:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, test.order.DisplayTitle())
		})
	}
}

// The name is the moment of creation in unix seconds, so a directory of orders
// lists in the order they were written and nothing has to be named.
func TestCreateNamesTheFileForTheMoment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "orders")

	now := time.Unix(1758300000, 0)

	path, err := Create(dir, now)
	require.NoError(t, err)

	assert.Equal(t, "1758300000.md", filepath.Base(path), "want the unix timestamp")

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, Blank(), string(written))
}

// Two orders in the same second are routine. The second must not overwrite the
// first, and must still sort after it.
func TestCreateNeverOverwritesAndKeepsTheOrder(t *testing.T) {
	dir := t.TempDir()

	now := time.Unix(1758300000, 0)

	first, err := Create(dir, now)
	require.NoError(t, err)

	write(t, first, "keep me")

	second, err := Create(dir, now)
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
	assert.Equal(t, "1758300001.md", filepath.Base(second))

	kept, _ := os.ReadFile(first)
	assert.Equal(t, "keep me", string(kept), "the first order was overwritten")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	assert.Len(t, entries, 2, "want both orders, named so they sort in the order they were made")
	assert.Equal(t, filepath.Base(first), entries[0].Name(), "want both orders, named so they sort in the order they were made")
	assert.Equal(t, filepath.Base(second), entries[1].Name(), "want both orders, named so they sort in the order they were made")
}

func TestCreateReportsAnUnwritableDirectory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")

	write(t, blocker, "x")

	_, err := Create(filepath.Join(blocker, "orders"), time.Now())
	require.Error(t, err, "creating under a file must fail")
}

// The default prompt tells the agent about its long-term memory. Where the log is,
// and that it outlives the context window.
func TestTheDefaultPromptPointsAtTheSessionLog(t *testing.T) {
	filled := strings.Replace(Blank(), "objective:\n", "objective: build it\n", 1)

	order, err := Parse([]byte(filled))
	require.NoError(t, err)

	got, err := order.Render(Env{Session: "/work/.zot/orders/1.jsonl"})
	require.NoError(t, err)

	for _, want := range []string{"## Memory", "short-term memory", "/work/.zot/orders/1.jsonl", "earlier run"} {
		assert.Contains(t, got, want, "the prompt does not say %q", want)
	}
}

// An order that writes its own prompt can put the log where it likes.
func TestAnOrdersOwnPromptCanReadTheSessionLog(t *testing.T) {
	order, err := Parse([]byte("---\nobjective: x\n---\nyour notes are in {{ .Session }}\n"))
	require.NoError(t, err)

	got, err := order.Render(Env{Session: "/log.jsonl"})
	require.NoError(t, err)

	assert.Contains(t, got, "your notes are in /log.jsonl", "Session was not available to the prompt")
}
