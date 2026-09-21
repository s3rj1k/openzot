package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/skills"
	"github.com/openzot/openzot/internal/tui"
)

// taskKickoff is the user message that starts a run. The goal is in the
// instructions. This only has to get the agent moving.
const taskKickoff = "Begin working on your task. Start by calling the tasks tool to list the work, then carry it through to completion."

// runOptions configures a run beyond the configuration itself.
type runOptions struct {
	// SessionPath is the log this run is appended to. One file per task, so a
	// run of the same task again adds to it. Empty disables recording.
	SessionPath string

	// A short label for the work, shown in the viewer instead of the task text. A work order's title or its
	// file name. Presentation only, and never sent to the model. The goal is the contract.
	Title string

	// Project is the instructions found in the AGENTS.md files of the config
	// directory and the project, for the config's prompt to use as .Project.
	Project string

	// Skills are the skills loaded at startup, offered to the model through the
	// skills tool.
	Skills []skills.Skill

	// Viewer shows the run. Nil is the full-screen viewer. It is a field so that
	// what needs a terminal can be replaced by what does not.
	Viewer func(context.Context, tui.Meta, *loop.Options) (loop.Result, error)
}

// runOrder executes one autonomous coding task, rendering the agent's activity in the read-only TUI. The tools operate on the
// current working directory, so the caller chdirs into the project first. It blocks until the user quits the viewer or the
// run errors.
func runOrder(ctx context.Context, cfg *config.Config, o order.Order, options runOptions) error {
	config.ScrubProviderSecrets(cfg)

	client, opts, err := resolve(ctx, cfg, options.Skills)
	if err != nil {
		return err
	}

	workdir, _ := os.Getwd()

	// The config's prompt, filled in from the order, is the system prompt. The goal, criteria and constraints survive
	// trimming there, and the opening user message only gets the agent moving. Rendered after the secrets leave the environment.

	// There is no way to open a run with a prompt of the caller's own. Agent takes a work order, not a
	// conversation, and anything worth saying to the agent belongs in the order, where it is durable.
	prompt, err := o.Render(cfg.Prompt, orderEnv(cfg, client, &opts, workdir, options.SessionPath, options.Project))
	if err != nil {
		return fmt.Errorf("order %s: %w", cmp.Or(o.Path, "(unsaved)"), err)
	}

	opts.Instructions = prompt
	opts.Messages = []conversation.Message{{Type: conversation.TypeUser, Text: taskKickoff}}

	task := o.Objective

	// The session log is not optional. It is the run's record and the agent's long-term memory, so a run
	// that cannot be recorded is rejected rather than run without either.
	if options.SessionPath == "" {
		return errors.New("no session log: a run is always recorded")
	}

	writer, err := session.Open(options.SessionPath, session.Meta{
		Task:     task,
		Model:    client.Config().Model,
		Provider: cfg.Provider.Label(),
		Workdir:  workdir,
	})
	if err != nil {
		return fmt.Errorf("session log: %w", err)
	}

	defer writer.Close()

	// A log that stops being writable ends the run. What it cannot record it
	// should not go on doing.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	recorder := newRecorder(writer, func(error) { stop() })

	// The seed is recorded before the run starts so a session that dies in its
	// first turn still says what it was asked to do.
	recorder.Conversation(opts.Messages)

	opts.OnConversation = recorder.Conversation
	opts.OnEvent = recorder.Event

	meta := viewerMeta(cfg, task, workdir, &opts)
	meta.Title = options.Title

	viewer := options.Viewer
	if viewer == nil {
		viewer = tui.Run
	}

	result, err := viewer(ctx, meta, &opts)

	// A run that never began, or was abandoned still going, has no ending to write
	// down or to report.
	if result.Reason != "" {
		recorder.Result(&result)

		printDigest(os.Stderr, writer.Path(), &result)
	}

	if failed := recorder.Err(); failed != nil {
		return fmt.Errorf("session log: %w", failed)
	}

	return err
}
