// Package order defines the work order: the document a zot run is dispatched
// from.
//
// zot deliberately takes no prose on the command line. A factory accepts a work
// order, not a conversation - a durable objective, the acceptance criteria that
// define "done", and the constraints the work must hold to. The order is a file
// so it outlives the invocation: it can be edited, committed and re-run, and
// every run of it starts from zero.
//
// An order is advisory input - what to do - and may therefore live anywhere,
// including the repository being worked on. How the result is judged (quality
// gates) is deliberately not part of the order schema: adjudication belongs to
// the operator's configuration, never to a document the agent can write.
package order

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The book's layout. A project's orders live under one dotted directory at its
// root, the way every other tool that keeps state in a repository does it:
// .zot/orders/<name>.yaml. A top-level orders/ directory would claim a generic
// name in the root of somebody else's project, which is not zot's to take.
//
// Only the default lives here. An order may be read from anywhere, so this
// names the convention rather than enforcing it.
const (
	// BookDir is the per-project directory holding the orders.
	BookDir = ".zot"

	ordersName = "orders"
)

// OrdersDir is where new orders for the project rooted at dir are created.
func OrdersDir(dir string) string { return filepath.Join(dir, BookDir, ordersName) }

// Order is one work order: a single run's brief.
type Order struct {
	// Title is an optional short label for the order, for people rather than
	// for the agent. It never reaches the model - see Task - because the
	// objective is the contract and a title is only how a human recognises it
	// in a list or a viewer.
	Title string `yaml:"title,omitempty"`

	// Objective is the durable goal of the run. It goes into the system prompt
	// and survives trimming, so the agent cannot forget it on a long run.
	Objective string `yaml:"objective"`

	// Acceptance are the criteria that define "done". They travel with the
	// objective into the system prompt, and they are the contract a future
	// verification gate judges the result against.
	Acceptance []string `yaml:"acceptance,omitempty"`

	// Constraints are rules the work must hold to throughout - boundaries, not
	// goals.
	Constraints []string `yaml:"constraints,omitempty"`

	// Path is where the order was loaded from, for reporting. Empty for an
	// order that never was a file (a synthesized one).
	Path string `yaml:"-"`
}

// List returns the order files directly inside dir, in filename order - the
// batch a bare `zot` runs. Only the top level is listed, matching the shell
// glob the invocation is named after, and a missing directory is an empty
// listing rather than an error: a project with no book yet simply has no
// outstanding work.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read orders: %w", err)
	}

	var paths []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".yaml") {
			continue
		}

		paths = append(paths, filepath.Join(dir, entry.Name()))
	}

	sort.Strings(paths)

	return paths, nil
}

// Load reads and parses one order file.
func Load(path string) (Order, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Order{}, fmt.Errorf("read order: %w", err)
	}

	order, err := Parse(data)
	if err != nil {
		return Order{}, fmt.Errorf("order %s: %w", path, err)
	}

	order.Path = path

	return order, nil
}

// Parse decodes order YAML. Unknown fields are rejected - a typo like
// "acceptence:" must fail loudly rather than silently dropping the criteria the
// operator thought they set.
func Parse(data []byte) (Order, error) {
	var order Order

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)

	if err := decoder.Decode(&order); err != nil {
		return Order{}, fmt.Errorf("parse: %w", err)
	}

	order.Title = strings.TrimSpace(order.Title)
	order.Objective = strings.TrimSpace(order.Objective)
	order.Acceptance = cleanList(order.Acceptance)
	order.Constraints = cleanList(order.Constraints)

	if order.Objective == "" {
		return Order{}, fmt.Errorf("no objective")
	}

	return order, nil
}

// Encode renders the order back to YAML, for handing to another process.
func (o Order) Encode() string {
	// Order is plain strings and slices, which Marshal cannot fail on.
	data, _ := yaml.Marshal(o)

	return string(data)
}

// DisplayTitle is what to call this order on screen.
//
// A declared title wins. Failing that the file name is one: order files are
// named from their objective already, so fix-the-flaky-test.yaml is a
// perfectly good "Fix the flaky test" and deriving it costs the operator
// nothing. An order that is neither titled nor a file - one synthesized in
// memory by a dispatcher - has no name to show, and gets none: inventing a
// label from the objective would put a truncated sentence where a title goes,
// which is the thing having titles is meant to stop.
func (o Order) DisplayTitle() string {
	if o.Title != "" {
		return o.Title
	}

	if o.Path == "" {
		return ""
	}

	return titleFromFilename(o.Path)
}

// titleFromFilename turns an order's file name into a label: dashes and
// underscores become spaces, and the first word is capitalised. Sentence case
// rather than Title Case, because an objective-derived name is a sentence -
// "Fix The Flaky Test" reads like a headline for something that is not one.
func titleFromFilename(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))

	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' {
			return ' '
		}

		return r
	}, name)

	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return ""
	}

	first, size := utf8.DecodeRuneInString(name)

	return string(unicode.ToUpper(first)) + name[size:]
}

// Task renders the order as the durable objective text placed in the system
// prompt: the objective, then the acceptance criteria and constraints as the
// terms the agent works - and is judged - against.
func (o Order) Task() string {
	var b strings.Builder

	b.WriteString(o.Objective)

	if len(o.Acceptance) > 0 {
		b.WriteString("\n\nAcceptance criteria - the objective is not met until every one of these holds:")

		for i, criterion := range o.Acceptance {
			fmt.Fprintf(&b, "\n%d. %s", i+1, criterion)
		}
	}

	if len(o.Constraints) > 0 {
		b.WriteString("\n\nConstraints - these hold for the whole run:")

		for _, constraint := range o.Constraints {
			b.WriteString("\n- " + constraint)
		}
	}

	return b.String()
}

// blank is the form a new order starts from. The objective is left empty, so
// the order will not run until it is written.
const blank = `# zot work order - what to do, and what "done" means.

# An optional short label for this order, shown in the viewer. Without
# one the file name is used.
# title:

# The durable goal of the run. The order will not run until this is filled in.
objective:

# The objective is not met until every one of these holds.
# acceptance:
#   - the new behaviour is covered by a test that fails without the change
#   - the full test suite passes

# Rules that hold for the whole run.
# constraints:
#   - do not change public API signatures
`

// Blank returns the form a new order starts from.
func Blank() string { return blank }

// Create writes a blank order into dir, creating the directory if needed, and
// returns its path. The file is named for the moment it was made, in unix
// seconds, so orders sort in the order they were written and no name has to be
// invented. A name already taken moves on to the next second rather than
// overwriting: creating two orders in a second is routine, not an error.
func Create(dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create order directory: %w", err)
	}

	for stamp := now.Unix(); ; stamp++ {
		path := filepath.Join(dir, strconv.FormatInt(stamp, 10)+".yaml")

		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return "", fmt.Errorf("create order: %w", err)
		}

		_, err = file.WriteString(blank)

		if closeErr := file.Close(); err == nil {
			err = closeErr
		}

		if err != nil {
			return "", fmt.Errorf("write order: %w", err)
		}

		return path, nil
	}
}

// cleanList trims entries and drops empty ones, so a stray "- " in the YAML
// does not become an empty criterion the agent is asked to satisfy.
func cleanList(items []string) []string {
	var out []string

	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}

	return out
}
