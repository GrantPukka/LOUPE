package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/VIGIL-OPS/loupe/internal/server"
	"github.com/spf13/cobra"
)

func newServeCommand(g *globals) *cobra.Command {
	var (
		addr    string
		verbose bool
	)

	cmd := &cobra.Command{
		Use:   "serve [directory] [filter]",
		Short: "Serve the loaded logs over a local HTTP API",
		Long: `Ingest a directory and expose it over HTTP for the web UI.

Binds loopback only, and refuses anything else. loupe has no authentication
because it is a local tool reading local files; binding it to a reachable
interface would publish your logs to the network. Use an SSH tunnel if you need
it from elsewhere.

No outbound connections are made, by this command or any other.

Endpoints:

    GET  /api/schema      columns, sources, and the display timezone
    POST /api/query       {filter|sql, limit, offset, sort} -> records
    POST /api/histogram   {filter, buckets} -> counts over time
    GET  /api/sources     per-file formats and timezone provenance
    GET  /api/health`,
		Example: `  loupe serve ./logs
  loupe serve ./logs --addr 127.0.0.1:9000
  curl -s localhost:7717/api/schema | jq`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd, g, args, addr, verbose)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", server.DefaultAddr, "loopback address to listen on")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log every request")

	return cmd
}

func runServe(cmd *cobra.Command, g *globals, args []string, addr string, verbose bool) error {
	path, filter := resolveArgs(args)
	if filter != "" {
		return fmt.Errorf("serve takes a directory, not a filter — "+
			"filtering happens in the UI. Did you mean `loupe %s %q`?", path, filter)
	}

	sess, err := g.open(cmd.Context(), path)
	if err != nil {
		return err
	}
	defer sess.Close()

	statusLine(os.Stderr, sess)

	opts := server.Options{Addr: addr}
	if verbose {
		opts.Logger = log.New(os.Stderr, "", log.Ltime)
	}

	srv := server.New(sess, opts)

	// Bind before announcing, so a port clash or a non-loopback address fails
	// with an error rather than a URL that does not work.
	ln, err := srv.Listen()
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nListening on http://%s\n", ln.Addr())
	fmt.Fprintf(os.Stderr, "  curl -s http://%s/api/schema | jq\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "Ctrl-C to stop.")

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Stopped.")
	return nil
}
