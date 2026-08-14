package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/store"
	"github.com/spf13/cobra"
)

func newCacheCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Show or clear the ingest cache",
		Long: `loupe caches an ingested directory so a re-run over unchanged files skips
reading them again.

The cache lives in ~/.cache/loupe by default and is keyed on the paths, sizes,
and modification times of the source files, plus the options that change what
gets ingested. Changing any of those produces a different entry rather than
serving stale data.

A file that is still being written to changes on every run, so an active log
directory re-ingests each time. The cache pays off on archived and rotated logs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCacheList(g)
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "clear",
		Short: "Delete every cached ingest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			removed, freed, err := store.ClearCache(g.cacheDir)
			if err != nil {
				return err
			}
			fmt.Printf("removed %d cached ingest(s), freed %s\n", removed, humanBytes(freed))
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the cache directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := store.CacheDir(g.cacheDir)
			if err != nil {
				return err
			}
			fmt.Println(dir)
			return nil
		},
	})

	return cmd
}

func runCacheList(g *globals) error {
	dir, err := store.CacheDir(g.cacheDir)
	if err != nil {
		return err
	}

	entries, err := store.ListCache(g.cacheDir)
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Printf("no cached ingests in %s\n", dir)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ENTRY\tSIZE\tLAST USED")

	var total int64
	for _, e := range entries {
		total += e.Size
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			trimExt(e.Path), humanBytes(e.Size), age(e.Modified))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}

	fmt.Printf("\n%d entr%s, %s total in %s (cap %s)\n",
		len(entries), plural(len(entries), "y", "ies"),
		humanBytes(total), dir, humanBytes(store.DefaultCacheLimit))
	return nil
}

func trimExt(path string) string {
	base := path
	if i := lastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := lastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return base
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
