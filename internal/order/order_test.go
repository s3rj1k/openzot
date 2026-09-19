package order

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if order.Title != "Rate limiting" || order.Objective != "add rate limiting to the API" {
		t.Errorf("title = %q, objective = %q", order.Title, order.Objective)
	}

	if len(order.Acceptance) != 2 || order.Acceptance[1] != "the suite passes" {
		t.Errorf("acceptance = %q, want two trimmed criteria and no blank one", order.Acceptance)
	}

	if len(order.Constraints) != 1 {
		t.Errorf("constraints = %q", order.Constraints)
	}

	if !strings.Contains(order.Body, "You are an agent.") || !strings.Contains(order.Body, "{{ .Objective }}") {
		t.Errorf("body = %q, want the prompt after the front matter", order.Body)
	}

	if order.Path != path {
		t.Errorf("path = %q", order.Path)
	}
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
		{"an unknown front matter key", "---\nobjective: do it\nacceptence:\n  - x\n---\nbody", "acceptence"},
		{"front matter that is not YAML", "---\nobjective: [unclosed\n---\nbody", "front matter"},
		{"a prompt that does not parse", simple("{{ .Objective "), "prompt"},
		{"a prompt naming a field that does not exist", simple("{{ .Objectve }}"), "Objectve"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.content))
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error %q should mention %q", err, test.want)
			}
		})
	}

	_, err := Load(filepath.Join(t.TempDir(), "missing.md"))
	if err == nil {
		t.Error("a missing file must fail")
	}
}

// A field that only exists on the branch a real run takes is still found at
// load: the stand-in data fills every list, so the branch is exercised.
func TestATypoInABranchIsFoundAtLoad(t *testing.T) {
	_, err := Parse([]byte(simple("{{ if .Acceptance }}{{ .Acceptnce }}{{ end }}")))
	if err == nil || !strings.Contains(err.Error(), "Acceptnce") {
		t.Errorf("err = %v, want the misspelt field named", err)
	}

	_, err = Parse([]byte(simple("{{ range .Constraints }}{{ .Nope }}{{ end }}")))
	if err == nil {
		t.Error("a bad field inside a range over an empty list must still be found")
	}
}

// Blank lines before the opening line are harmless; a --- inside the prompt is
// just prompt.
func TestFrontMatterSplitting(t *testing.T) {
	order, err := Parse([]byte("\n\n---\r\nobjective: go\r\n---\r\nabove\n---\nbelow\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if order.Objective != "go" {
		t.Errorf("objective = %q", order.Objective)
	}

	if !strings.Contains(order.Body, "above\n---\nbelow") {
		t.Errorf("body = %q, want the rest of the file, its own --- included", order.Body)
	}
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
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := order.Render(testEnv)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"The Thing | fix the parser",
		"1) it parses", "2) tests pass",
		"- no new deps",
		"[shell: runs commands][tasks: keeps the plan]",
		"/work/project 2026-09-19 glm-5.2 gateway",
		"Always mention PINECONE.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt is missing %q:\n%s", want, got)
		}
	}
}

func TestTheFileAndEnvFunctions(t *testing.T) {
	workdir := t.TempDir()

	write(t, filepath.Join(workdir, "style.txt"), "use tabs\n")

	absolute := filepath.Join(t.TempDir(), "abs.txt")
	write(t, absolute, "absolute")

	t.Setenv("ZOT_TEST_VALUE", "from-the-environment")

	order, err := Parse([]byte(simple(`{{ file "style.txt" }}|{{ file "` + absolute + `" }}|{{ env "ZOT_TEST_VALUE" }}|{{ env "ZOT_TEST_UNSET" }}|`)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := order.Render(Env{Workdir: workdir})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !strings.HasPrefix(got, "use tabs|absolute|from-the-environment||") {
		t.Errorf("rendered = %q", got)
	}

	// a file that is not there is an error, not an empty string: a prompt that
	// silently lost its style guide is a worse failure than one that says so
	missing, err := Parse([]byte(simple(`{{ file "nope.txt" }}`)))
	if err != nil {
		t.Fatalf("Parse must not read files: %v", err)
	}

	if _, err := missing.Render(Env{Workdir: workdir}); err == nil {
		t.Error("rendering a prompt that includes a missing file must fail")
	}
}

func TestFileExpandsTheHomeDirectory(t *testing.T) {
	home := t.TempDir()

	t.Setenv("HOME", home)
	write(t, filepath.Join(home, "notes.txt"), "from home")

	order, err := Parse([]byte(simple(`{{ file "~/notes.txt" }}`)))
	if err != nil {
		t.Fatal(err)
	}

	got, err := order.Render(Env{Workdir: t.TempDir()})
	if err != nil || !strings.HasPrefix(got, "from home") {
		t.Errorf("rendered = %q, %v", got, err)
	}
}

// Whatever the prompt says, the contract is in what the agent is given - once.
func TestTheContractIsAlwaysThereExactlyOnce(t *testing.T) {
	bare, err := Parse([]byte(simple("Just do {{ .Objective }}.")))
	if err != nil {
		t.Fatal(err)
	}

	got, err := bare.Render(testEnv)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Count(got, Contract) != 1 || !strings.HasPrefix(got, "Just do do the thing.") {
		t.Errorf("a prompt without the contract must get it, once, after its own text:\n%s", got)
	}

	withIt, err := Parse([]byte(simple("Rules.\n\n{{ .Contract }}\n\nGo.")))
	if err != nil {
		t.Fatal(err)
	}

	got, err = withIt.Render(testEnv)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Count(got, Contract) != 1 || !strings.HasSuffix(strings.TrimSpace(got), "Go.") {
		t.Errorf("a prompt that already carries the contract must not get it again:\n%s", got)
	}
}

// The scaffold is a blank form: it must be written before it can run, and once it
// is, the default prompt it carries has to render into the prompt zot has always
// run with.
func TestBlankIsNotRunnableUntilTheObjectiveIsWritten(t *testing.T) {
	if _, err := Parse([]byte(Blank())); err == nil {
		t.Fatal("the blank form parsed as an order with an objective")
	}

	filled := strings.Replace(Blank(), "objective:\n", "objective: fix the typo\n", 1)

	order, err := Parse([]byte(filled))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := order.Render(testEnv)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"You are zot",
		`- "shell": runs commands`,
		`- "tasks": keeps the plan`,
		"Operating rules:",
		"# Project context\n\nAlways mention PINECONE.",
		"## Your task\n\nfix the typo",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the default prompt is missing %q:\n%s", want, got)
		}
	}

	if strings.Count(got, Contract) != 1 {
		t.Errorf("the default prompt must carry the contract once:\n%s", got)
	}

	// with no project context the section is left out, not left empty
	got, err = order.Render(Env{Tools: testEnv.Tools})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, "# Project context") {
		t.Errorf("no project context, no heading:\n%s", got)
	}
}

// The task section reads as it always has: the objective, then the criteria as a
// numbered list, then the constraints as bullets.
func TestTheDefaultTaskSectionListsCriteriaAndConstraints(t *testing.T) {
	filled := strings.Replace(Blank(), "objective:\n", "objective: build it\nacceptance:\n  - a works\n  - b works\nconstraints:\n  - keep it small\n", 1)

	order, err := Parse([]byte(filled))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := order.Render(Env{})
	if err != nil {
		t.Fatal(err)
	}

	want := "## Your task\n\nbuild it\n\n" +
		"Acceptance criteria - the objective is not met until every one of these holds:\n1. a works\n2. b works\n\n" +
		"Constraints - these hold for the whole run:\n- keep it small"

	if !strings.Contains(got, want) {
		t.Errorf("the task section is not laid out as it was:\n%s", got)
	}
}

// A title is a label for people. A declared one wins; without one the file name
// is already a perfectly good name, because order files are named from their
// objective. Sentence case, not Title Case - the name is a sentence.
func TestDisplayTitlePrefersTheDeclaredOneThenTheFileName(t *testing.T) {
	tests := []struct {
		name  string
		order Order
		want  string
	}{
		{
			name:  "a declared title wins",
			order: Order{Title: "Rate limiting", Path: "/book/.zot/orders/add-rate-limiting-to-the-api.md"},
			want:  "Rate limiting",
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
			if got := test.order.DisplayTitle(); got != test.want {
				t.Errorf("DisplayTitle = %q, want %q", got, test.want)
			}
		})
	}
}

// The name is the moment of creation in unix seconds, so a directory of orders
// lists in the order they were written and nothing has to be named.
func TestCreateNamesTheFileForTheMoment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "orders")

	now := time.Unix(1758300000, 0)

	path, err := Create(dir, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if filepath.Base(path) != "1758300000.md" {
		t.Errorf("name = %q, want the unix timestamp", filepath.Base(path))
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(written) != Blank() {
		t.Errorf("file = %q, want the blank form", written)
	}
}

// Two orders in the same second are routine. The second must not overwrite the
// first, and must still sort after it.
func TestCreateNeverOverwritesAndKeepsTheOrder(t *testing.T) {
	dir := t.TempDir()

	now := time.Unix(1758300000, 0)

	first, err := Create(dir, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	write(t, first, "keep me")

	second, err := Create(dir, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if second == first || filepath.Base(second) != "1758300001.md" {
		t.Errorf("second = %q, want the next free second", filepath.Base(second))
	}

	kept, _ := os.ReadFile(first)
	if string(kept) != "keep me" {
		t.Errorf("the first order was overwritten: %q", kept)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) != 2 || entries[0].Name() != filepath.Base(first) || entries[1].Name() != filepath.Base(second) {
		t.Errorf("entries = %v, want both orders, named so they sort in the order they were made", entries)
	}
}

func TestCreateReportsAnUnwritableDirectory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")

	write(t, blocker, "x")

	if _, err := Create(filepath.Join(blocker, "orders"), time.Now()); err == nil {
		t.Error("creating under a file must fail")
	}
}
