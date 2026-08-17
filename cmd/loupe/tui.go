package main

import (
	"fmt"
	"os"

	"github.com/VIGIL-OPS/loupe/internal/render"
	"github.com/VIGIL-OPS/loupe/internal/tui"
	"github.com/spf13/cobra"
)

func newTUICommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "tui [directory] [filter]",
		Short: "Explore the logs in a full-screen terminal interface",
		Long: `The same screen as the web UI, in the terminal.

For the common case of being on a box with no browser and no way to copy a
four-gigabyte log directory off it.

Keys:

    /            edit the filter          enter   apply, or expand a record
    j k ↑ ↓      move                     f       filter by the selected source
    ctrl-d/u     half a page              esc     clear the filter
    g G          top, bottom              q       quit`,
		Example: `  loupe tui ./logs
  loupe tui ./logs 'level:>=error'`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd, g, args)
		},
	}
}

func runTUI(cmd *cobra.Command, g *globals, args []string) error {
	// A full-screen interface needs a terminal to be full-screen in. Failing
	// with a clear reason beats emitting escape codes into a pipe.
	if !render.IsTerminal(os.Stdout) {
		return fmt.Errorf("loupe tui needs a terminal; " +
			"for a pipe or a script use `loupe` with --format ndjson")
	}

	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	paths, _ := resolvePaths(g, given)

	sess, err := g.open(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	return tui.Run(cmd.Context(), sess, filter)
}
