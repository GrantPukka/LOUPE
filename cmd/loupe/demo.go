package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GrantPukka/loupe/internal/blaster"
	"github.com/GrantPukka/loupe/internal/store"
	"github.com/spf13/cobra"
)

func newDemoCommand(g *globals) *cobra.Command {
	var (
		regenerate bool
		print      bool
	)

	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Generate a fake incident and explore it",
		Long: `demo writes a realistic incident to a scratch directory and opens it.

Six services in six formats — JSON lines, logfmt, Nginx, Log4j with stack
traces, Postgres, and syslog — over eighteen minutes, with a Postgres
connection pool exhausting itself two thirds of the way in, the application
erroring behind it, and Nginx returning 502s behind that. The same trace id
runs through all of them, which is the thing worth looking at.

The data is generated from a fixed seed, so it is the same every time, and it
is written under the cache directory rather than the working directory. Nothing
is written next to your own files.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := demoDir(g)
			if err != nil {
				return err
			}

			fresh, err := generateDemo(dir, regenerate)
			if err != nil {
				return err
			}
			if !fresh {
				fmt.Fprintf(os.Stderr, "reusing the demo incident in %s "+
					"(--regenerate for fresh data)\n", dir)
			}
			fmt.Fprintf(os.Stderr, "\nexplore it yourself:  loupe %s 'level:>=error'\n\n", dir)

			if print {
				return runDefault(cmd, g, []string{dir})
			}
			return runServe(cmd, g, []string{dir}, g.uiAddr, false, true)
		},
	}

	cmd.Flags().BoolVar(&regenerate, "regenerate", false,
		"discard the existing demo data and generate it again")
	cmd.Flags().BoolVar(&print, "print", false,
		"print the records to the terminal instead of opening the UI")

	return cmd
}

// demoDir puts the generated logs under the cache directory. loupe is
// read-only with respect to log files and writes only to the cache and to an
// explicit --handoff; a demo that scattered files into the working directory
// would be a third exception, and a surprising one.
func demoDir(g *globals) (string, error) {
	base, err := store.CacheDir(g.cacheDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "demo"), nil
}

// generateDemo writes the demo logs, reporting whether it had to generate them.
// Existing data is reused so that a second `loupe demo` starts instantly and
// the ingest cache still hits.
func generateDemo(dir string, force bool) (bool, error) {
	if !force {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return false, nil
		}
	}
	if force {
		if err := os.RemoveAll(dir); err != nil {
			return false, fmt.Errorf("clear demo dir: %w", err)
		}
	}

	c := blaster.Defaults()
	c.Out = dir
	// The per-file breakdown is the pitch — six formats, one incident — so it
	// is worth showing. On stderr, so --print stays pipeable.
	c.Report = os.Stderr
	if err := blaster.Run(c); err != nil {
		return false, fmt.Errorf("generate demo data: %w", err)
	}
	return true, nil
}
