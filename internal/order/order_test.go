package order

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadReadsAFullOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.yaml")

	write(t, path, `
objective: |-
  add rate limiting to the API
acceptance:
  - "requests beyond the limit receive 429"
  - "  the suite passes  "
  - ""
constraints:
  - do not change handler signatures
`)

	order, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if order.Objective != "add rate limiting to the API" {
		t.Errorf("Objective = %q", order.Objective)
	}

	// entries are trimmed and empties dropped, so a stray "- " in the YAML does
	// not become a criterion the agent is asked to satisfy
	if len(order.Acceptance) != 2 || order.Acceptance[1] != "the suite passes" {
		t.Errorf("Acceptance = %q", order.Acceptance)
	}

	if len(order.Constraints) != 1 {
		t.Errorf("Constraints = %q", order.Constraints)
	}

	if order.Path != path {
		t.Errorf("Path = %q, want the file it came from", order.Path)
	}
}

func TestLoadErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	empty := filepath.Join(t.TempDir(), "empty.yaml")
	write(t, empty, "objective: '  '\n")

	// a typo must fail loudly rather than silently dropping what the operator
	// thought they set
	typo := filepath.Join(t.TempDir(), "typo.yaml")
	write(t, typo, "objective: x\nacceptence:\n  - y\n")

	prose := filepath.Join(t.TempDir(), "prose.yaml")
	write(t, prose, "fix the bug in the parser\n")

	for name, path := range map[string]string{
		"a missing file":      missing,
		"an empty objective":  empty,
		"an unknown field":    typo,
		"prose, not an order": prose,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(path); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// Encode must round-trip through Parse: it is how an order travels to another
// process (a sandbox, a CI job), and an encoding Parse rejects would strand it.
func TestEncodeRoundTrips(t *testing.T) {
	original := Order{
		Objective:   "line one\nline two: with a colon",
		Acceptance:  []string{"a: tricky { criterion }", "plain"},
		Constraints: []string{"# not a comment"},
	}

	decoded, err := Parse([]byte(original.Encode()))
	if err != nil {
		t.Fatalf("Parse(Encode()): %v", err)
	}

	decoded.Path = original.Path

	if decoded.Objective != original.Objective {
		t.Errorf("Objective = %q, want %q", decoded.Objective, original.Objective)
	}

	if len(decoded.Acceptance) != 2 || decoded.Acceptance[0] != original.Acceptance[0] {
		t.Errorf("Acceptance = %q", decoded.Acceptance)
	}

	if len(decoded.Constraints) != 1 || decoded.Constraints[0] != original.Constraints[0] {
		t.Errorf("Constraints = %q", decoded.Constraints)
	}
}

func TestTaskRendersTheWholeContract(t *testing.T) {
	order := Order{
		Objective:   "add rate limiting",
		Acceptance:  []string{"429 beyond the limit", "suite passes"},
		Constraints: []string{"no new dependencies"},
	}

	task := order.Task()

	for _, want := range []string{
		"add rate limiting",
		"Acceptance criteria",
		"1. 429 beyond the limit",
		"2. suite passes",
		"Constraints",
		"- no new dependencies",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("Task() is missing %q:\n%s", want, task)
		}
	}
}

func TestTaskOfABareObjectiveIsJustTheObjective(t *testing.T) {
	task := Order{Objective: "fix the typo"}.Task()

	if task != "fix the typo" {
		t.Errorf("Task() = %q - empty sections must not render headings", task)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
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
			order: Order{Title: "Rate limiting", Path: "/book/.zot/orders/add-rate-limiting-to-the-api.yaml"},
			want:  "Rate limiting",
		},
		{
			name:  "the file name becomes one",
			order: Order{Path: "/book/.zot/orders/fix-the-flaky-test.yaml"},
			want:  "Fix the flaky test",
		},
		{
			name:  "underscores read as spaces too",
			order: Order{Path: "fix_the_flaky_test.yaml"},
			want:  "Fix the flaky test",
		},
		{
			name:  "a one-word name still capitalises",
			order: Order{Path: "cleanup.yaml"},
			want:  "Cleanup",
		},
		{
			name:  "an already-capitalised name is left alone",
			order: Order{Path: "API-cleanup.yaml"},
			want:  "API cleanup",
		},
		{
			name:  "a name that is only separators yields nothing to show",
			order: Order{Path: "---.yaml"},
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

// The title is a label for humans and never reaches the model: the objective is
// the contract, and spending context on a name for it would be paying twice for
// the same words.
func TestTheTitleStaysOutOfTheTask(t *testing.T) {
	o := Order{
		Title:      "Rate limiting",
		Objective:  "add rate limiting to the api",
		Acceptance: []string{"the suite passes"},
	}

	if strings.Contains(o.Task(), "Rate limiting") {
		t.Errorf("the title must not enter the task the agent is given:\n%s", o.Task())
	}

	if !strings.Contains(o.Task(), "add rate limiting to the api") {
		t.Errorf("the objective must still be the task:\n%s", o.Task())
	}
}

// The field is optional in both directions: an order without one parses as it
// always did, and one with a title round-trips through the file.
func TestTitleIsOptionalAndRoundTrips(t *testing.T) {
	plain, err := Parse([]byte("objective: do the thing\n"))
	if err != nil {
		t.Fatalf("an order without a title must parse: %v", err)
	}

	if plain.Title != "" {
		t.Errorf("Title = %q, want empty", plain.Title)
	}

	titled, err := Parse([]byte("title: The Thing\nobjective: do the thing\n"))
	if err != nil {
		t.Fatalf("an order with a title must parse: %v", err)
	}

	if titled.Title != "The Thing" {
		t.Errorf("Title = %q, want %q", titled.Title, "The Thing")
	}

	// through a file and back
	path := filepath.Join(t.TempDir(), "thing.yaml")

	if err := os.WriteFile(path, []byte(titled.Encode()), 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("a titled order does not load: %v", err)
	}

	if reloaded.Title != "The Thing" {
		t.Errorf("the title did not survive the file: %q", reloaded.Title)
	}
}

// A new order is a blank form, not a runnable one: it must be written before it
// can run, and it says so when loaded.
func TestBlankIsNotARunnableOrder(t *testing.T) {
	if _, err := Parse([]byte(Blank())); err == nil {
		t.Error("the blank form parsed as an order with an objective")
	}

	// the form still has to be valid YAML once the objective is written in
	filled := strings.Replace(Blank(), "objective:\n", "objective: fix the typo\n", 1)

	loaded, err := Parse([]byte(filled))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if loaded.Objective != "fix the typo" {
		t.Errorf("objective = %q", loaded.Objective)
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

	if filepath.Base(path) != "1758300000.yaml" {
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

	if err := os.WriteFile(first, []byte("objective: keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := Create(dir, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if second == first || filepath.Base(second) != "1758300001.yaml" {
		t.Errorf("second = %q, want the next free second", filepath.Base(second))
	}

	kept, _ := os.ReadFile(first)
	if string(kept) != "objective: keep me\n" {
		t.Errorf("the first order was overwritten: %q", kept)
	}

	listed, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(listed) != 2 || listed[0] != first || listed[1] != second {
		t.Errorf("listed = %v, want the orders in the order they were made", listed)
	}
}

func TestCreateReportsAnUnwritableDirectory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")

	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(filepath.Join(blocker, "orders"), time.Now()); err == nil {
		t.Error("creating under a file must fail")
	}
}
