// Package order defines the work order: the document a zot run is dispatched
// from.
//
// zot deliberately takes no prose on the command line. A factory accepts a work
// order, not a conversation. The order is a file so it outlives the invocation:
// it can be edited, committed and re-run, and every run of it starts from zero.
//
// An order is the run's whole system prompt. The file opens with a front matter
// block - the objective, the acceptance criteria that define "done", the
// constraints the work must hold to - and the rest is the prompt itself, a Go
// text/template that reads that block and a few facts about the run. Someone who
// only wants to say what to do fills in the front matter and leaves the prompt as
// zot wrote it; someone who wants to change how the agent works rewrites it.
//
// An order is advisory input - what to do - and may therefore live anywhere,
// including the repository being worked on. How the result is judged (quality
// gates) is deliberately not part of the order schema: adjudication belongs to
// the operator's configuration, never to a document the agent can write.
package order

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The book's layout. A project's orders live under one dotted directory at its
// root, the way every other tool that keeps state in a repository does it:
// .zot/orders/<name>.md. A top-level orders/ directory would claim a generic
// name in the root of somebody else's project, which is not zot's to take.
//
// Only the default lives here. An order may be read from anywhere, so this
// names the convention rather than enforcing it.
const (
	// BookDir is the per-project directory holding the orders.
	BookDir = ".zot"

	ordersName = "orders"

	// Ext is the extension of an order file: front matter and a prompt, which is
	// Markdown-shaped text.
	Ext = ".md"
)

// OrdersDir is where new orders for the project rooted at dir are created.
func OrdersDir(dir string) string { return filepath.Join(dir, BookDir, ordersName) }

// Order is one work order: a single run's brief, and the prompt it is run with.
type Order struct {
	// Title is an optional short label for the order, for people rather than
	// for the agent: it is how a human recognises the order in a list or a
	// viewer. The prompt may use it, but nothing does by default.
	Title string

	// Objective is the durable goal of the run. The prompt puts it where the
	// agent cannot forget it on a long run.
	Objective string

	// Acceptance are the criteria that define "done". They travel with the
	// objective into the prompt, and they are the contract a future
	// verification gate judges the result against.
	Acceptance []string

	// Constraints are rules the work must hold to throughout - boundaries, not
	// goals.
	Constraints []string

	// Body is the system prompt, as a Go text/template. See Render.
	Body string

	// Path is where the order was loaded from, for reporting. Empty for an
	// order that never was a file (a synthesized one).
	Path string
}

// frontMatter is the data block at the head of an order file.
type frontMatter struct {
	Title       string   `yaml:"title"`
	Objective   string   `yaml:"objective"`
	Acceptance  []string `yaml:"acceptance"`
	Constraints []string `yaml:"constraints"`
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

// Parse reads an order: the front matter, then the prompt.
//
// Unknown front matter keys are rejected - a typo like "acceptence:" must fail
// loudly rather than silently dropping the criteria the operator thought they
// set. The prompt is parsed as a template and run once against stand-in data, so
// a syntax error or a misspelt field is found now, at load, before a provider is
// touched, and not when the run reaches it.
func Parse(data []byte) (Order, error) {
	header, body, err := splitFrontMatter(string(data))
	if err != nil {
		return Order{}, err
	}

	var front frontMatter

	decoder := yaml.NewDecoder(strings.NewReader(header))
	decoder.KnownFields(true)

	if err := decoder.Decode(&front); err != nil && !errors.Is(err, io.EOF) {
		return Order{}, fmt.Errorf("front matter: %w", err)
	}

	order := Order{
		Title:       strings.TrimSpace(front.Title),
		Objective:   strings.TrimSpace(front.Objective),
		Acceptance:  cleanList(front.Acceptance),
		Constraints: cleanList(front.Constraints),
		Body:        body,
	}

	if order.Objective == "" {
		return Order{}, fmt.Errorf("no objective")
	}

	if strings.TrimSpace(order.Body) == "" {
		return Order{}, fmt.Errorf("no prompt: the text after the front matter is the system prompt, and it is empty")
	}

	if err := order.check(); err != nil {
		return Order{}, err
	}

	return order, nil
}

// splitFrontMatter separates the data block from the prompt. The block opens the
// file with a line of three dashes and closes with another; the prompt is what
// follows.
func splitFrontMatter(text string) (header, body string, err error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	start := 0

	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	if start == len(lines) || strings.TrimSpace(lines[start]) != "---" {
		return "", "", fmt.Errorf("no front matter: an order starts with a line of three dashes, then its objective")
	}

	for end := start + 1; end < len(lines); end++ {
		if strings.TrimSpace(lines[end]) == "---" {
			return strings.Join(lines[start+1:end], "\n"), strings.Join(lines[end+1:], "\n"), nil
		}
	}

	return "", "", fmt.Errorf("the front matter is not closed: it ends with a line of three dashes")
}

// DisplayTitle is what to call this order on screen.
//
// A declared title wins. Failing that the file name is one: order files are
// named from their objective already, so fix-the-flaky-test.md is a
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
		path := filepath.Join(dir, strconv.FormatInt(stamp, 10)+Ext)

		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return "", fmt.Errorf("create order: %w", err)
		}

		_, err = file.WriteString(Blank())

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
