package order_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/testutils"
)

// validOrder is the smallest order that parses.
const validOrder = "---\nobjective: do the thing\n---\n"

// parsed is the valid order, for the tests of what a prompt does with it.
func parsed(t *testing.T) order.Order {
	t.Helper()

	o, err := order.Parse([]byte(validOrder))
	require.NoError(t, err)

	return o
}

func TestLoadReadsAFullOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.md")

	testutils.Write(t, path, `
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
`)

	loaded, err := order.Load(path)
	require.NoError(t, err)

	assert.Equal(t, litRateLimiting, loaded.Title)
	assert.Equal(t, "add rate limiting to the API", loaded.Objective)

	assert.Len(t, loaded.Acceptance, 2, "want two trimmed criteria and no blank one")
	assert.Equal(t, "the suite passes", loaded.Acceptance[1], "want two trimmed criteria and no blank one")

	assert.Len(t, loaded.Constraints, 1)

	assert.Equal(t, path, loaded.Path)
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
		{"an empty objective", "---\nobjective:\n---\n", "no objective"},
		{"no objective key", "---\ntitle: x\n---\n", "no objective"},
		{"a prompt after the front matter", "---\nobjective: do it\n---\nYou are an agent.\n", "front matter only"},
		//nolint:misspell // the misspelled key is the fixture
		{"an unknown front matter key", "---\nobjective: do it\nacceptence:\n  - x\n---\n", "acceptence"},
		{"front matter that is not YAML", "---\nobjective: [unclosed\n---\n", "front matter"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := order.Parse([]byte(test.content))
			require.Error(t, err)

			assert.Contains(t, err.Error(), test.want)
		})
	}

	_, err := order.Load(filepath.Join(t.TempDir(), "missing.md"))
	require.Error(t, err, "a missing file must fail")
}

// Blank lines around the front matter and Windows line endings are harmless. Anything
// else after the closing line is text, and an order holds none.
func TestFrontMatterSplitting(t *testing.T) {
	loaded, err := order.Parse([]byte("\n\n---\r\nobjective: go\r\n---\r\n\n  \n"))
	require.NoError(t, err)

	assert.Equal(t, "go", loaded.Objective)

	_, err = order.Parse([]byte("---\nobjective: go\n---\nabove\n---\nbelow\n"))
	require.Error(t, err, "want the text after the front matter refused, its own --- included")
}

var testEnv = order.Env{
	Tools:    []order.Tool{{Name: "shell", Description: "runs commands"}, {Name: "tasks", Description: "keeps the plan"}},
	Workdir:  "/work/project",
	Date:     "2026-09-19",
	Model:    "glm-5.2",
	Provider: "gateway",
	Project:  "Always mention PINECONE.",
	Session:  "/work/.agent/orders/1.jsonl",
}

func TestRenderSubstitutesTheOrderAndTheRun(t *testing.T) {
	loaded, err := order.Parse([]byte(`---
title: The Thing
objective: fix the parser
acceptance:
  - it parses
  - tests pass
constraints:
  - no new deps
---
`))
	require.NoError(t, err)

	got, err := loaded.Render(`{{ .Title }} | {{ .Objective }}
{{ range $i, $a := .Acceptance }}{{ inc $i }}) {{ $a }}
{{ end }}{{ range .Constraints }}- {{ . }}
{{ end }}{{ range .Tools }}[{{ .Name }}: {{ .Description }}]{{ end }}
{{ .Workdir }} {{ .Date }} {{ .Model }} {{ .Provider }}
{{ .Project }} {{ .Session }}
`, testEnv)
	require.NoError(t, err)

	for _, want := range []string{
		"The Thing | fix the parser",
		"1) it parses", "2) tests pass",
		"- no new deps",
		"[shell: runs commands][tasks: keeps the plan]",
		"/work/project 2026-09-19 glm-5.2 gateway",
		"Always mention PINECONE. /work/.agent/orders/1.jsonl",
	} {
		assert.Contains(t, got, want, "rendered prompt is missing %q", want)
	}
}

// The prompt is the operator's to write, so what it renders to is all the agent is told.
func TestRenderAddsNothingOfItsOwn(t *testing.T) {
	got, err := parsed(t).Render("Start by {{ .Objective }}.", testEnv)
	require.NoError(t, err)

	assert.Equal(t, "Start by do the thing.", got)
}

func TestRenderErrors(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{"a prompt that does not parse", "{{ .Objective ", "prompt"},
		{"a prompt naming a field that does not exist", "{{ .Objectve }}", "Objectve"},
		{"a field that only the taken branch reads", "{{ if .Objective }}{{ .Objectve }}{{ end }}", "Objectve"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parsed(t).Render(test.prompt, testEnv)
			require.Error(t, err)

			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestTheFileAndEnvFunctions(t *testing.T) {
	workdir := t.TempDir()

	testutils.Write(t, filepath.Join(workdir, "style.txt"), "use tabs\n")

	absolute := filepath.Join(t.TempDir(), "abs.txt")
	testutils.Write(t, absolute, "absolute")

	t.Setenv("AGENT_TEST_VALUE", "from-the-environment")

	got, err := parsed(t).Render(`{{ file "style.txt" }}|{{ file "`+absolute+`" }}|{{ env "AGENT_TEST_VALUE" }}|{{ env "AGENT_TEST_UNSET" }}|`, order.Env{Workdir: workdir})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(got, "use tabs|absolute|from-the-environment||"))

	// a file that is not there is an error, not an empty string. A prompt that
	// silently lost its style guide is a worse failure than one that says so
	_, err = parsed(t).Render(`{{ file "nope.txt" }}`, order.Env{Workdir: workdir})
	require.Error(t, err, "rendering a prompt that includes a missing file must fail")
}

func TestFileExpandsTheHomeDirectory(t *testing.T) {
	home := t.TempDir()

	t.Setenv("HOME", home)
	testutils.Write(t, filepath.Join(home, "notes.txt"), "from home")

	got, err := parsed(t).Render(`{{ file "~/notes.txt" }}`, order.Env{Workdir: t.TempDir()})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "from home"))
}

// The scaffold is a blank form. It must be written before it can run.
func TestBlankIsNotRunnableUntilTheObjectiveIsWritten(t *testing.T) {
	_, err := order.Parse([]byte(order.Blank()))
	require.Error(t, err, "the blank form parsed as an order with an objective")

	filled := strings.Replace(order.Blank(), "objective:\n", "objective: fix the typo\n", 1)

	loaded, err := order.Parse([]byte(filled))
	require.NoError(t, err)

	assert.Equal(t, "fix the typo", loaded.Objective)
}

// A title is a label for people. A declared one wins. Without one the file name
// is already a perfectly good name, because order files are named from their
// goal. Sentence case, not Title Case - the name is a sentence.
func TestDisplayTitlePrefersTheDeclaredOneThenTheFileName(t *testing.T) {
	tests := []struct {
		name  string
		order order.Order
		want  string
	}{
		{
			name:  "a declared title wins",
			order: order.Order{Title: litRateLimiting, Path: "/book/.agent/orders/add-rate-limiting-to-the-api.md"},
			want:  litRateLimiting,
		},
		{
			name:  "the file name becomes one",
			order: order.Order{Path: "/book/.agent/orders/fix-the-flaky-test.md"},
			want:  "Fix the flaky test",
		},
		{
			name:  "underscores read as spaces too",
			order: order.Order{Path: "fix_the_flaky_test.md"},
			want:  "Fix the flaky test",
		},
		{
			name:  "a one-word name still capitalises",
			order: order.Order{Path: "cleanup.md"},
			want:  "Cleanup",
		},
		{
			name:  "an already-capitalised name is left alone",
			order: order.Order{Path: "API-cleanup.md"},
			want:  "API cleanup",
		},
		{
			name:  "a name that is only separators yields nothing to show",
			order: order.Order{Path: "---.md"},
			want:  "",
		},
		{
			name:  "an order that was never a file has no name to show",
			order: order.Order{Objective: "a synthesized order with a very long objective nobody wants as a title"},
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

	path, err := order.Create(dir, now)
	require.NoError(t, err)

	assert.Equal(t, "1758300000.md", filepath.Base(path), "want the unix timestamp")

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, order.Blank(), string(written))
}

// Two orders in the same second are routine. The second must not overwrite the
// first, and must still sort after it.
func TestCreateNeverOverwritesAndKeepsTheOrder(t *testing.T) {
	dir := t.TempDir()

	now := time.Unix(1758300000, 0)

	first, err := order.Create(dir, now)
	require.NoError(t, err)

	testutils.Write(t, first, "keep me")

	second, err := order.Create(dir, now)
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

	testutils.Write(t, blocker, "x")

	_, err := order.Create(filepath.Join(blocker, "orders"), time.Now())
	require.Error(t, err, "creating under a file must fail")
}
