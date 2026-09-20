package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/order"
)

// `zot new` opens a blank order in the editor, named for the moment it was
// made. What the operator writes there is the order.
func TestNewOrderOpensABlankOrderInTheEditor(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: fix the typo\n---\nbody\n' > "$1"`)

	var out strings.Builder

	if err := newOrder(nil, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("orders written = %v, want the one", matches)
	}

	if name := filepath.Base(matches[0]); !regexp.MustCompile(`^\d+\.md$`).MatchString(name) {
		t.Errorf("name = %q, want a unix timestamp", name)
	}

	if !strings.Contains(out.String(), matches[0]) {
		t.Errorf("the output should say where the order went and how to run it:\n%s", out.String())
	}

	o, err := loadOrder([]string{matches[0]})
	if err != nil {
		t.Fatalf("the written order does not resolve: %v", err)
	}

	if o.Objective != "fix the typo" {
		t.Errorf("objective = %q", o.Objective)
	}

	// the book is one dotted directory: zot does not claim the generic
	// top-level names in the root of somebody else's project
	if _, err := os.Stat("orders"); err == nil {
		t.Errorf("a top-level orders/ was created; the book lives under %s", order.BookDir)
	}
}

// Prose has no place on the command line. Someone typing it out of habit is told
// where it goes, and nothing is created.
func TestNewOrderTakesNoProse(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: never\n---\nbody\n' > "$1"`)

	err := newOrder([]string{"fix", "the", "typo"}, io.Discard)
	if err == nil {
		t.Fatal("prose must be refused")
	}

	if !strings.Contains(err.Error(), "no arguments") {
		t.Errorf("the error should say zot new takes none: %v", err)
	}

	if _, statErr := os.Stat(order.BookDir); !os.IsNotExist(statErr) {
		t.Errorf("a refused invocation must create nothing: %v", statErr)
	}
}

// `zot new --dir` creates the order in another working directory, not the one
// the command was invoked from - the order belongs to the project it is for.
func TestNewOrderWithDirCreatesItInThatDirectory(t *testing.T) {
	invocation := t.TempDir()
	target := t.TempDir()

	t.Chdir(invocation)

	withEditor(t, `printf -- '---\nobjective: fix the typo\n---\nbody\n' > "$1"`)

	if err := newOrder([]string{"--dir", target}, io.Discard); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	if _, err := os.Stat(filepath.Join(invocation, order.BookDir)); !os.IsNotExist(err) {
		t.Errorf("the invoking directory must stay untouched: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(target, order.BookDir, "orders", "*.md"))
	if len(matches) != 1 {
		t.Errorf("orders in the target project = %v, want the one", matches)
	}
}

// An order closed without a word written is not an order, and a blank one left
// in the book would fail every bare `zot` after it. Nothing was written, so
// nothing is kept.
func TestNewOrderLeftUnchangedIsNotKept(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `true`)

	var out strings.Builder

	if err := newOrder(nil, &out); err != nil {
		t.Fatalf("newOrder: %v", err)
	}

	if matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md")); len(matches) != 0 {
		t.Errorf("an unedited order was kept: %v", matches)
	}

	if !strings.Contains(out.String(), "no order was created") {
		t.Errorf("the output should say nothing was created:\n%s", out.String())
	}
}

// An editor that fails must not cost the operator the file: they may have
// written the order before it went wrong.
func TestNewOrderKeepsTheFileWhenTheEditorFails(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: half written\n---\nbody\n' > "$1"; exit 3`)

	if err := newOrder(nil, io.Discard); err == nil {
		t.Fatal("an editor that fails must be reported")
	}

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("orders = %v, want the file kept", matches)
	}
}

// With no editor to be found the order is still created, and the operator is
// told where it is, the way `zot config` does.
func TestNewOrderWithoutAnEditorSaysWhereTheFileIs(t *testing.T) {
	t.Chdir(t.TempDir())

	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", t.TempDir())

	err := newOrder(nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no editor found") {
		t.Fatalf("err = %v, want it to say no editor was found", err)
	}

	if matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md")); len(matches) != 1 {
		t.Errorf("orders = %v, want the blank order left to be edited", matches)
	}
}

// editConfig is the setup path: it must create the config from the template on
// first run, and say something useful when there is no editor to open it with.
func TestEditConfigSeedsTheTemplate(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("ZOT_CONFIG", filepath.Join(dir, "nested", "config.yaml"))
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", dir) // no nano/vi/vim reachable

	err := editConfig()

	// with no editor available it must fail loudly rather than silently doing
	// nothing - but the file it would have opened must exist by then
	if err == nil {
		t.Fatal("expected an error when no editor is available")
	}

	if !strings.Contains(err.Error(), "editor") {
		t.Errorf("the error should mention the missing editor: %v", err)
	}

	if _, statErr := os.Stat(config.DefaultConfigPath()); statErr != nil {
		t.Errorf("the config should have been seeded from the template: %v", statErr)
	}
}

func TestEditConfigOpensTheConfiguredEditor(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")

	t.Setenv("ZOT_CONFIG", path)

	// a no-op "editor" that just succeeds; $VISUAL wins over $EDITOR, which here
	// would fail the edit if it were the one run
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "false")

	if err := editConfig(); err != nil {
		t.Fatalf("editConfig: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}

	if len(content) == 0 {
		t.Error("the seeded config is empty")
	}
}
