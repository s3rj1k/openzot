package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/openzot/openzot/configs"
	"github.com/openzot/openzot/internal/config"
	"github.com/openzot/openzot/internal/order"
)

// openInEditor opens a file in the user's editor and waits for it to close, trying $VISUAL, then $EDITOR, then the first of
// nano, vi and vim that is installed. With none it prints the path and says so, since the file is already in place.
func openInEditor(path string) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		for _, candidate := range []string{"nano", "vi", "vim"} {
			if _, err := exec.LookPath(candidate); err == nil {
				editor = candidate

				break
			}
		}
	}

	if editor == "" {
		fmt.Println(path)

		return errors.New("no editor found; set $EDITOR (the file is at the path above)")
	}

	cmd := exec.CommandContext(context.Background(), editor, path) //nolint:gosec // G204: the editor is the operator's own $VISUAL or $EDITOR
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd.Run()
}

// newOrder creates a blank work order under ./.agent/orders (or <dir>/.agent/orders with --dir) and opens it in the editor, the way
// `agent config` opens the config. It takes no prose, so the goal, criteria and constraints are written in the file where they can be
// reviewed. The file is named for its creation moment, so orders sort as written. --dir is for a project elsewhere.
func newOrder(args []string, out io.Writer) error {
	set := pflag.NewFlagSet("new", pflag.ContinueOnError)

	dir := set.String("dir", ".", "project the order is for: it is created under <dir>/"+order.BookDir+"/orders")
	configPath := set.String("config", "", "path to agent config, whose order: is the blank order (default: "+config.DefaultConfigPath()+")")

	if err := set.Parse(args); err != nil {
		return err
	}

	if set.NArg() > 0 {
		return errors.New("agent new takes no arguments: it opens a blank order in your editor - write the objective there")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.Order) == "" {
		return config.ErrNoOrder
	}

	path, err := order.Create(order.OrdersDir(*dir), time.Now(), cfg.Order)
	if err != nil {
		return err
	}

	// The file stays if the editor fails, so an editor that is missing does not
	// cost the operator the order they meant to write.
	if err := openInEditor(path); err != nil {
		return err
	}

	// An order left exactly as made is not an order, and a blank one in .agent/orders would only fail
	// when someone ran it. Nothing was written, so nothing is kept.
	if written, err := os.ReadFile(path); err == nil && string(written) == cfg.Order { //nolint:gosec // G304: the order path is the one the operator named
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the unedited order: %w", err)
		}

		fmt.Fprintln(out, "nothing written, so no order was created")

		return nil
	}

	fmt.Fprintf(out, "wrote %s\n\nrun it with:\n\n  agent %s\n", path, path)

	return nil
}

// editConfig ensures the config file exists - seeding it from the embedded
// template on first run - and opens it in the user's editor. This is the setup
// path. Configure the provider, model and key by editing the file.
func editConfig() error {
	path := config.DefaultConfigPath()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, configs.ExampleConfigYAML, 0o600); err != nil {
			return fmt.Errorf("write config template: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Created %s from the template.\n", path)
	}

	return openInEditor(path)
}
