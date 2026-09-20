package order

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// The prompt an order is run with.
//
// An order's body is a Go text/template. What it can read is documented in the
// front matter of the file zot new writes - the order's own fields, a few facts
// about the run, and three functions - and is defined by Env and data here.

// Contract is the half of the prompt no order may leave out. Every other prompt
// rule is a preference; this one is a fact about the machine the agent is running
// on. Zot has no input channel at all - a run is a work order, a provider and a
// read-only viewer - so an agent that asks a question is not answered tersely, it
// is not answered at all: it waits until a guard kills the run, and everything it
// had not yet written is lost. That failure is silent and expensive, and it costs
// a whole run to discover, so the contract is re-attached to whatever an order
// renders to rather than left to whoever wrote it.
//
// It is written to stand alone, naming the terminal tools itself, because an
// order's own prompt need not mention them at all.
const Contract = `## Non-interactive contract

Nothing you address to the user is delivered. There is no reader, no reply, and no approval on its way. A question you ask is discarded unheard, and a run that stops to wait for an answer waits until a guard kills it, losing the work it had not yet finished.

- Never stop to wait for input, approval, permission or confirmation. No one can grant what you asked for, so asking and waiting is the one certain way to fail the task.
- Never end your turn with a question, an offer, or a promise to continue once told to. Continue now instead.
- Where the task is ambiguous or underspecified, decide it the way a careful engineer would, act on the decision, and record the assumption in a task's note and again in your final summary. A stated assumption is reviewable afterwards; an unasked question is not.
- Only a terminal tool call ends the task: "success" with a summary when the objective is met, or "failure" with the reason when it genuinely cannot be. Uncertainty is not a reason to stop - it is a reason to choose, act, and say what you chose. Do not simply stop.`

// promptIntro opens the default prompt: who the agent is, and that no one is
// listening.
const promptIntro = `You are zot, a fully autonomous software engineering agent operating inside a real working directory on the user's machine.

This is a non-interactive session running in the background. No one is watching, and no questions or further guidance can be answered - you will receive NO further input. Complete the assigned task end to end on your own, using your tools.

Your tools:`

// promptTools lists the tools the run really has, so the prompt cannot describe
// tools that are not offered or leave out ones that are.
const promptTools = `
{{- range .Tools }}
- "{{ .Name }}": {{ .Description }}
{{- end }}

`

// promptRules is how the agent is expected to work.
const promptRules = `Operating rules:
- Begin by calling "tasks" to list the concrete tasks the work needs, in the order you will do them.
- Look before you change. Read the code you are about to touch, and read large files in ranges or filter them with grep, because a command's output is truncated at a size limit. After you change anything, build and run the tests, and fix what you broke.
- Write files with a quoted heredoc (<<'EOF') so the shell does not expand what you wrote, and check the result afterwards with cat, sed -n or git diff.
- Keep "tasks" current: mark a task in_progress when you begin it and done when it is finished, and mark it blocked, with a note saying why, when it cannot go on.
- Never run interactive or long-lived commands.
- Act, do not narrate. The deliverable is the changed working tree, not an explanation of it; there is no reader to address. Do not pause to summarize, interpret, or analyze tool output - keep working, and use "tasks" for status.`

// promptMemory tells the agent where its long-term memory is. The context window
// is short-term memory and is forgotten oldest first as it fills; the session log
// keeps every message, so a model that knows it exists can go back for what it
// lost. Every run has one: zot refuses to run without.
const promptMemory = `

## Memory

What you can see is short-term memory: your context window. When it fills, the oldest messages are forgotten, and you are not told which. Everything you have said and done - in this run and in any earlier run of this order - is kept, one JSON record per line, in the session log:

    {{ .Session }}

Search it with your shell when you need something that is no longer in view: a file you read, a command's output, a decision you made. A message record looks like {"kind":"message","message":{"type":"user|bot|reasoning|activity","text":"...","activity":{"kind":"request|response","name":"...","arguments":"...","result":...}}}; a "meta" record opens each run and a "result" record closes it. Records can be large, so filter before you print - grep -n, tail -n, sed -n 'START,ENDp', or jq -c if it is installed - and never cat the whole file.`

// promptProject adds the project's own instructions - its AGENTS.md - when it has
// any.
const promptProject = `
{{- if .Project }}

# Project context

{{ .Project }}
{{- end }}`

// promptTask is where the objective goes. It lives in the system prompt rather
// than as a user message so it survives trimming: the oldest messages are dropped
// first to fit the window, so a user message can fall out of a long run, and an
// autonomous agent that forgets its own objective is the worst way for a run to
// fail. The instructions are never dropped and always ordered first.
const promptTask = `

## Your task

{{ .Objective }}
{{- if .Acceptance }}

Acceptance criteria - the objective is not met until every one of these holds:
{{- range $index, $criterion := .Acceptance }}
{{ inc $index }}. {{ $criterion }}
{{- end }}
{{- end }}
{{- if .Constraints }}

Constraints - these hold for the whole run:
{{- range .Constraints }}
- {{ . }}
{{- end }}
{{- end }}
`

// DefaultBody is the prompt zot writes into a new order.
const DefaultBody = promptIntro + promptTools + promptRules + promptMemory + "\n\n" + Contract + promptProject + promptTask

// frontMatterBlank is the data block a new order starts from. The objective is
// left empty, so the order will not run until it is written.
const frontMatterBlank = `---
# zot work order. This block says what to do and what "done" means; everything
# below the closing --- is the system prompt, a Go text/template that reads it.

# An optional short label for this order, shown in the viewer. Without
# one the file name is used.
# title:

# The durable goal of the run. The order will not run until this is filled in.
objective:

# The objective is not met until every one of these holds.
# acceptance:
#   - the new behavior is covered by a test that fails without the change
#   - the full test suite passes

# Rules that hold for the whole run.
# constraints:
#   - do not change public API signatures

# The prompt below may use, as {{ .Name }}:
#   Title Objective Acceptance Constraints      from this block
#   Tools                                       each tool's Name and Description
#   Workdir Date Model Provider                 facts about the run
#   Project                                     the AGENTS.md of the config and the project
#   Session                                     the log of this run, its long-term memory
#   Contract                                    the non-interactive contract, below
# and the functions: file "path", env "NAME", inc N.
# Edit the prompt freely. If the rendered result loses the non-interactive
# contract, zot adds it back: the run has no way to ask anyone anything.
---
`

// Blank returns the form a new order starts from: an empty front matter and the
// default prompt.
func Blank() string { return frontMatterBlank + DefaultBody }

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

	// Session is the path of the log this run is recorded in. It is the run's
	// long-term memory: every message, including the ones the context window has
	// forgotten.
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
	Contract string
}

// Render runs the order's prompt for a run in env. The result always carries the
// non-interactive contract: whatever the order's own text says, it cannot opt a
// run into an interactivity zot does not have.
func (o Order) Render(env Env) (string, error) {
	rendered, err := o.execute(env, functions(env.Workdir))
	if err != nil {
		return "", err
	}

	// a prompt that already carries the contract - the default one does - is left
	// untouched, so the text is never repeated
	if strings.Contains(rendered, Contract) {
		return rendered, nil
	}

	return strings.TrimRight(rendered, "\n") + "\n\n" + Contract, nil
}

// execute renders the body with the given functions.
func (o Order) execute(env Env, funcs template.FuncMap) (string, error) {
	prompt, err := template.New("order").Option("missingkey=error").Funcs(funcs).Parse(o.Body)
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
		Contract:    Contract,
	})
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}

	return out.String(), nil
}

// check runs the prompt once against stand-in data, so that a template that
// cannot be rendered - a syntax error, a field that does not exist - is found
// when the order is loaded. The stand-in fills every field, so the branches that
// depend on a field being there are exercised too; the file and env functions are
// stubs, as what they read is a fact about the machine, not about the order.
func (o Order) check() error {
	stub := template.FuncMap{
		"file": func(string) (string, error) { return "", nil },
		"env":  func(string) string { return "" },
		"inc":  inc,
	}

	_, err := o.execute(Env{
		Tools:    []Tool{{Name: "tool", Description: "does something"}},
		Workdir:  "/work",
		Date:     "2000-01-01",
		Model:    "model",
		Provider: "provider",
		Project:  "project",
		Session:  "/work/.zot/orders/order.jsonl",
	}, stub)

	// the stand-in order is the real one, but a list that is empty in the real
	// order is filled here, so a range over it is exercised as well
	if err == nil && (len(o.Acceptance) == 0 || len(o.Constraints) == 0) {
		filled := o

		if len(filled.Acceptance) == 0 {
			filled.Acceptance = []string{"criterion"}
		}

		if len(filled.Constraints) == 0 {
			filled.Constraints = []string{"constraint"}
		}

		_, err = filled.execute(Env{Tools: []Tool{{Name: "tool"}}, Project: "project"}, stub)
	}

	return err
}

// functions are what a prompt may call.
//
//   - file "path" inlines a file: relative to the working directory, or absolute,
//     or under ~. A house style guide or a checklist stays a file of its own.
//   - env "NAME" reads an environment variable.
//   - inc N is N+1, for numbering a list: templates have no arithmetic of their own.
//
// An order is trusted the way a script is: it can already tell the agent to run
// anything, so what it can read into its own prompt is no larger a power.
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

			content, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}

			return strings.TrimRight(string(content), "\n"), nil
		},
		"env": os.Getenv,
		"inc": inc,
	}
}

func inc(n int) int { return n + 1 }
