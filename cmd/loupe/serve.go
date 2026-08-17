package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/VIGIL-OPS/loupe/internal/server"
	"github.com/VIGIL-OPS/loupe/internal/workspace"
	"github.com/spf13/cobra"
)

func newServeCommand(g *globals) *cobra.Command {
	var (
		addr        string
		verbose     bool
		openBrowser bool
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
			return runServe(cmd, g, args, addr, verbose, openBrowser)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", server.DefaultAddr, "loopback address to listen on")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log every request")
	cmd.Flags().BoolVar(&openBrowser, "open", false, "open the UI in a browser once it is listening")

	return cmd
}

func runServe(cmd *cobra.Command, g *globals, args []string, addr string, verbose, openBrowser bool) error {
	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	if filter != "" {
		return fmt.Errorf("serve takes a directory, not a filter — "+
			"filtering happens in the UI. Did you mean `loupe %s %q`?",
			strings.Join(given, " "), filter)
	}

	paths, note := resolvePaths(g, given)

	sess, err := g.open(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	if note != "" {
		fmt.Fprintf(os.Stderr, "%s\n", note)
	}
	statusLine(os.Stderr, sess)

	// The workspace lets the UI browse and change what is subscribed. A
	// failure to read it is not fatal: the API still serves what is loaded.
	work, err := workspace.Load(g.configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}

	opts := server.Options{Addr: addr}
	if verbose {
		opts.Logger = log.New(os.Stderr, "", log.Ltime)
	}

	srv := server.New(sess, work, opts)

	// Bind before announcing, so a port clash or a non-loopback address fails
	// with an error rather than a URL that does not work.
	ln, err := srv.Listen()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s", ln.Addr())

	fmt.Fprintf(os.Stderr, "\nListening on %s\n", url)
	if server.UIAvailable() {
		fmt.Fprintf(os.Stderr, "  open %s\n", url)
	} else {
		// Saying so up front beats sending someone to a page that explains it.
		fmt.Fprintln(os.Stderr, "  (no web UI in this binary — run `make web && make build`)")
	}
	fmt.Fprintf(os.Stderr, "  curl -s %s/api/schema | jq\n", url)
	fmt.Fprintln(os.Stderr, "Ctrl-C to stop.")

	if openBrowser {
		go launchBrowser(url)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Stopped.")
	return nil
}

// launchBrowser opens the UI.
//
// This runs a local command, not a network request: loupe still makes no
// outbound connections of its own. A failure is silent because the URL is
// already printed and the user can click it.
func launchBrowser(url string) {
	var command string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		command = "xdg-open"
	}

	// Best effort. The URL is already on screen, so a machine with no browser
	// or no opener loses nothing by this failing quietly.
	_ = exec.Command(command, append(args, url)...).Start()
}
