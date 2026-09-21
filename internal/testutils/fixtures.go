package testutils

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/order"
	"github.com/openzot/openzot/internal/tui"
)

// HeadlessViewer is tui.Run without the screen. It prints the task, the streamed tokens and the final message, and
// reports endings the way the viewer does. An error as itself, otherwise an agent-declared failure as an AgentExitError.
func HeadlessViewer(ctx context.Context, meta tui.Meta, opts *loop.Options) (loop.Result, error) {
	engine, err := loop.New(opts)
	if err != nil {
		return loop.Result{}, err
	}

	fmt.Println(meta.Task)

	result := engine.Run(ctx, func(event loop.Event) {
		if event.Kind == loop.EventToken {
			fmt.Print(event.Text)
		}
	})

	fmt.Println(result.Message)

	switch {
	case result.Err != nil:
		return result, result.Err
	case result.ExitCode() != 0:
		return result, &tui.AgentExitError{Code: result.ExitCode(), Message: result.Message}
	}

	return result, nil
}

// Declared is the model list a provider needs to run the named models, each with a context window since a model
// without one cannot run.
func Declared(names ...string) map[string]config.ModelConfig {
	models := make(map[string]config.ModelConfig, len(names))

	for _, name := range names {
		models[name] = config.ModelConfig{Context: 100_000}
	}

	return models
}

// Defaults is the built-in configuration plus what it lacks by design, the model to run and a prompt that is just the
// goal.
func Defaults(model string) *config.Config {
	cfg := config.Defaults()
	cfg.Agent.Model = model
	cfg.Prompt = "{{ .Objective }}"

	return &cfg
}

// TestOrder is an order for a run that never was a file.
func TestOrder(objective string) order.Order {
	return order.Order{Objective: objective}
}

// StarterPrompt is the prompt of the starter config `agent config` seeds, which is the one agent ships.
func StarterPrompt(t *testing.T) string {
	t.Helper()

	cfg, err := config.Load(WriteConfig(t, string(configs.ExampleConfigYAML)))
	require.NoError(t, err)

	return cfg.Prompt
}
