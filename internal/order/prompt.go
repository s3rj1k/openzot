package order

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// The prompt a run starts from is the config's, a Go text/template. What it can read is documented in the starter config
// and defined by Env and data.

// Tool is one tool the run offers, as the prompt sees it.
type Tool struct {
	Name        string
	Description string
}

// Env is what the prompt can know about the run beyond the order itself.
type Env struct {
	// Tools are the tools the run offers the model.
	Tools []Tool

	// Workdir is the directory the agent works in.
	Workdir string

	// Date is today's date.
	Date string

	// Model and Provider name what the run is talking to.
	Model    string
	Provider string

	// Project is the instructions the config directory and the project itself
	// carry for every run in them (their AGENTS.md files), or empty.
	Project string

	// The path of the log this run is recorded in, the run's long-term memory. It holds every message,
	// including the ones the context window has forgotten.
	Session string
}

// data is what a prompt template is executed against.
type data struct {
	Title       string
	Objective   string
	Acceptance  []string
	Constraints []string

	Tools    []Tool
	Workdir  string
	Date     string
	Model    string
	Provider string
	Project  string
	Session  string
}

func inc(n int) int { return n + 1 }

// The template functions are what a prompt may call. The file function inlines a file, env reads an environment variable,
// and inc adds one, for numbering a list. A prompt is trusted the way a script is, since it can already tell the agent to
// run anything.
func functions(workdir string) template.FuncMap {
	return template.FuncMap{
		"file": func(path string) (string, error) {
			if path == "~" || strings.HasPrefix(path, "~/") {
				home, err := os.UserHomeDir()
				if err != nil {
					return "", err
				}

				path = filepath.Join(home, strings.TrimPrefix(path, "~"))
			}

			if !filepath.IsAbs(path) && workdir != "" {
				path = filepath.Join(workdir, path)
			}

			content, err := os.ReadFile(path) //nolint:gosec // G304: the order template asked for this file by name
			if err != nil {
				return "", err
			}

			return strings.TrimRight(string(content), "\n"), nil
		},
		"env": os.Getenv,
		"inc": inc,
	}
}

// Render runs the config's prompt for this order in a run described by env.
func (o Order) Render(text string, env Env) (string, error) {
	prompt, err := template.New("prompt").Option("missingkey=error").Funcs(functions(env.Workdir)).Parse(text)
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}

	var out strings.Builder

	err = prompt.Execute(&out, data{
		Title:       o.Title,
		Objective:   o.Objective,
		Acceptance:  o.Acceptance,
		Constraints: o.Constraints,
		Tools:       env.Tools,
		Workdir:     env.Workdir,
		Date:        env.Date,
		Model:       env.Model,
		Provider:    env.Provider,
		Project:     env.Project,
		Session:     env.Session,
	})
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}

	return out.String(), nil
}
