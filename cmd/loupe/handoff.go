package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

// runHandoff writes an extract instead of printing records.
//
// docs/HANDOFF.md: the artefact a receiving engineer needs to reproduce and
// trust a finding without a conversation, because the person who found it is
// going back to fixing the outage.
func runHandoff(cmd *cobra.Command, g *globals, sess *session.Session, plan session.Plan) error {
	format, err := render.HandoffFormatFor(g.handoff)
	if err != nil {
		return err
	}

	extract, err := sess.Handoff(cmd.Context(), plan, session.HandoffOptions{
		Limit:   handoffLimit(g),
		Redact:  g.redact,
		Version: version,
		Command: invocation(),
	})
	if err != nil {
		return err
	}

	if g.handoff == "-" {
		return render.WriteHandoff(os.Stdout, extract, format)
	}

	// Write to a temporary file and rename, so an interrupted run does not
	// leave a half-written extract that looks complete.
	target := g.handoff + ".partial"
	file, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create %s: %w", g.handoff, err)
	}

	if err := render.WriteHandoff(file, extract, format); err != nil {
		file.Close()
		os.Remove(target)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(target)
		return fmt.Errorf("close %s: %w", g.handoff, err)
	}
	if err := os.Rename(target, g.handoff); err != nil {
		os.Remove(target)
		return fmt.Errorf("write %s: %w", g.handoff, err)
	}

	summarise(sess, extract, g.handoff)
	return nil
}

// handoffLimit resolves how many records the extract includes.
//
// The default is the handoff's own cap rather than the display limit: 200 rows
// is right for a ticket even when the terminal was showing fewer.
func handoffLimit(g *globals) int {
	if g.limit > 0 && g.limit != defaultDisplayLimit {
		return g.limit
	}
	return session.DefaultHandoffLimit
}

// summarise tells the operator what was written and what it admits to, so the
// caveats are visible before the file is sent rather than after.
func summarise(sess *session.Session, extract session.Handoff, path string) {
	stat, _ := os.Stat(path)
	size := int64(0)
	if stat != nil {
		size = stat.Size()
	}

	fmt.Fprintf(os.Stderr, "\nWrote %s (%d bytes)\n", path, size)
	fmt.Fprintf(os.Stderr, "  %d of %d matched records\n",
		extract.Counts.Shown, extract.Counts.Matched)

	if extract.Truncated {
		fmt.Fprintf(os.Stderr, "  truncated — the extract says so, and states the full count\n")
	}
	if extract.Counts.ExcludedNoTimestamp > 0 {
		fmt.Fprintf(os.Stderr, "  %d record(s) excluded for having no timestamp, and noted in the extract\n",
			extract.Counts.ExcludedNoTimestamp)
	}
	if assumed := extract.AssumedSources(); len(assumed) > 0 {
		names := make([]string, len(assumed))
		for i, s := range assumed {
			names[i] = filepath.Base(s.File)
		}
		fmt.Fprintf(os.Stderr, "  timezone assumed for %s, and flagged in the extract\n",
			strings.Join(names, ", "))
	}
	if len(extract.Redacted) > 0 {
		fmt.Fprintf(os.Stderr, "  redacted: %s\n", strings.Join(extract.Redacted, ", "))
	} else {
		// Worth saying out loud before somebody sends it to a vendor.
		fmt.Fprintf(os.Stderr, "  nothing redacted — pass --redact to mask field values\n")
	}
}

// invocation reconstructs the command line for the provenance footer.
func invocation() string {
	parts := make([]string, 0, len(os.Args))
	for _, arg := range os.Args {
		if strings.ContainsAny(arg, " \t\"'") {
			arg = fmt.Sprintf("%q", arg)
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}
