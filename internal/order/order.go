// Package order defines the work order, the document a agent run is dispatched from. An order is a file, so it outlives the
// invocation, and holds a front matter block (goal, acceptance criteria, constraints) that the config's prompt template
// reads. It is advisory input and may live anywhere. How the result is judged belongs to the operator's config.
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

// The book's layout. A project's orders live under one dotted directory, .agent/orders/<name>.md, since a top-level
// orders/ would claim a generic name in someone else's repository. Only the default lives here. An order may be
// read from anywhere, so this names the convention rather than enforcing it.
const (
	// BookDir is the per-project directory holding the orders.
	BookDir = ".agent"

	ordersName = "orders"

	// Ext is the extension of an order file, front matter in a Markdown-shaped file.
	Ext = ".md"
)

// OrdersDir is where new orders for the project rooted at dir are created.
func OrdersDir(dir string) string { return filepath.Join(dir, BookDir, ordersName) }

// Order is one work order, a single run's brief.
type Order struct {
	// An optional short label for people rather than the agent, to recognize the order in a list or the
	// viewer. The prompt may use it, but nothing does by default.
	Title string

	// The durable goal of the run. The prompt puts it where the
	// agent cannot forget it on a long run.
	Objective string

	// The criteria that define "done". They travel with the goal into the prompt and are the contract
	// a future verification gate judges the result against.
	Acceptance []string

	// Constraints are rules the work must hold to throughout - boundaries, not
	// goals.
	Constraints []string

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

// splitFrontMatter separates the data block from what follows it. The block opens the
// file with a line of three dashes and closes with another.
func splitFrontMatter(text string) (header, body string, err error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	start := 0

	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	if start == len(lines) || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("no front matter: an order starts with a line of three dashes, then its objective")
	}

	for end := start + 1; end < len(lines); end++ {
		if strings.TrimSpace(lines[end]) == "---" {
			return strings.Join(lines[start+1:end], "\n"), strings.Join(lines[end+1:], "\n"), nil
		}
	}

	return "", "", errors.New("the front matter is not closed: it ends with a line of three dashes")
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

// Parse reads an order, which is its front matter and nothing after it. Unknown front matter keys are rejected, so a
// misspelled key cannot silently drop the criteria the operator thought they set. Text after the front matter is
// rejected too, since the system prompt is the config's.
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
	}

	if order.Objective == "" {
		return Order{}, errors.New("no objective")
	}

	if strings.TrimSpace(body) != "" {
		return Order{}, errors.New("an order is front matter only, since the system prompt is the config's (prompt:)")
	}

	return order, nil
}

// Load reads and parses one order file.
func Load(path string) (Order, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the order path is the one the operator named
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

// titleFromFilename turns an order's file name into a label. Dashes and underscores become spaces and the first word is
// capitalized. Sentence case, not Title Case, since a goal-derived name is a sentence.
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

// DisplayTitle is what to call this order on screen. A declared title wins, then the file name (order files are named from
// their goal, so fix-the-flaky-test.md reads well). An order that is neither titled nor a file gets no name, since a
// label invented from the goal would put a truncated sentence where a title goes.
func (o Order) DisplayTitle() string {
	if o.Title != "" {
		return o.Title
	}

	if o.Path == "" {
		return ""
	}

	return titleFromFilename(o.Path)
}

// Create writes a blank order into dir, creating the directory if needed, and returns its path. The file is named for the
// moment it was made, in unix seconds, so orders sort as written. A taken name moves on to the next second rather than
// overwriting, since two orders in a second is routine.
func Create(dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create order directory: %w", err)
	}

	for stampSec := now.Unix(); ; stampSec++ {
		path := filepath.Join(dir, strconv.FormatInt(stampSec, 10)+Ext)

		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G304: the path is built from the order directory and a timestamp
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
