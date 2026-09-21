package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/order"
)

// `zot new` opens a blank order in the editor, named for the moment it was
// made. What the operator writes there is the order.
func TestNewOrderOpensABlankOrderInTheEditor(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: fix the typo\n---\n' > "$1"`)

	var out strings.Builder

	require.NoError(t, newOrder(nil, &out))

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	require.Len(t, matches, 1, "orders written = %v, want the one", matches)

	assert.True(t, regexp.MustCompile(`^\d+\.md$`).MatchString(filepath.Base(matches[0])))

	assert.Contains(t, out.String(), matches[0], "the output should say where the order went and how to run it")

	o, err := loadOrder([]string{matches[0]})
	require.NoError(t, err, "the written order does not resolve")

	assert.Equal(t, "fix the typo", o.Objective)

	// the book is one dotted directory. Zot does not claim the generic
	// top-level names in the root of somebody else's project
	_, err = os.Stat("orders")
	require.Error(t, err, "a top-level orders/ was created; the book lives under %s", order.BookDir)
}

// Prose has no place on the command line. Someone typing it out of habit is told
// where it goes, and nothing is created.
func TestNewOrderTakesNoProse(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: never\n---\nbody\n' > "$1"`)

	err := newOrder([]string{"fix", "the", "typo"}, io.Discard)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "no arguments", "the error should say zot new takes none")

	_, statErr := os.Stat(order.BookDir)
	assert.True(t, os.IsNotExist(statErr), "a refused invocation must create nothing")
}

// `zot new --dir` creates the order in another working directory, not the one
// the command was invoked from - the order belongs to the project it is for.
func TestNewOrderWithDirCreatesItInThatDirectory(t *testing.T) {
	invocation := t.TempDir()
	target := t.TempDir()

	t.Chdir(invocation)

	withEditor(t, `printf -- '---\nobjective: fix the typo\n---\nbody\n' > "$1"`)

	require.NoError(t, newOrder([]string{litDir, target}, io.Discard))

	_, err := os.Stat(filepath.Join(invocation, order.BookDir))
	assert.True(t, os.IsNotExist(err), "the invoking directory must stay untouched")

	matches, _ := filepath.Glob(filepath.Join(target, order.BookDir, "orders", "*.md"))
	assert.Len(t, matches, 1, "orders in the target project = %v, want the one", matches)
}

// An order closed without a word written is not an order, and a blank one left
// in the book would fail every bare `zot` after it. Nothing was written, so
// nothing is kept.
func TestNewOrderLeftUnchangedIsNotKept(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `true`)

	var out strings.Builder

	require.NoError(t, newOrder(nil, &out))

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	assert.Empty(t, matches, "an unedited order was kept")

	assert.Contains(t, out.String(), "no order was created", "the output should say nothing was created")
}

// An editor that fails must not cost the operator the file. They may have
// written the order before it went wrong.
func TestNewOrderKeepsTheFileWhenTheEditorFails(t *testing.T) {
	t.Chdir(t.TempDir())

	withEditor(t, `printf -- '---\nobjective: half written\n---\nbody\n' > "$1"; exit 3`)

	require.Error(t, newOrder(nil, io.Discard), "an editor that fails must be reported")

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	require.Len(t, matches, 1, "want the file kept")
}

// With no editor to be found the order is still created, and the operator is
// told where it is, the way `zot config` does.
func TestNewOrderWithoutAnEditorSaysWhereTheFileIs(t *testing.T) {
	t.Chdir(t.TempDir())

	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", t.TempDir())

	err := newOrder(nil, io.Discard)
	require.Error(t, err, "want it to say no editor was found")
	require.Contains(t, err.Error(), "no editor found", "want it to say no editor was found")

	matches, _ := filepath.Glob(filepath.Join(order.BookDir, "orders", "*.md"))
	assert.Len(t, matches, 1, "want the blank order left to be edited")
}

// editConfig is the setup path. It must create the config from the template on
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
	require.Error(t, err, "expected an error when no editor is available")

	assert.Contains(t, err.Error(), "editor", "the error should mention the missing editor")

	_, statErr := os.Stat(config.DefaultConfigPath())
	require.NoError(t, statErr, "the config should have been seeded from the template")
}

func TestEditConfigOpensTheConfiguredEditor(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")

	t.Setenv("ZOT_CONFIG", path)

	// a no-op "editor" that just succeeds. $VISUAL wins over $EDITOR, which here
	// would fail the edit if it were the one run
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "false")

	require.NoError(t, editConfig())

	content, err := os.ReadFile(path)
	require.NoError(t, err, "read seeded config")

	assert.NotEmpty(t, content, "the seeded config is empty")
}
