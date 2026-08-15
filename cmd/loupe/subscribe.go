package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/VIGIL-OPS/loupe/internal/store"
	"github.com/VIGIL-OPS/loupe/internal/workspace"
	"github.com/spf13/cobra"
)

func newSubscribeCommand(g *globals) *cobra.Command {
	var label string

	cmd := &cobra.Command{
		Use:   "subscribe <directory>...",
		Short: "Remember a log location and read it by default",
		Long: `Add a directory to the set loupe reads when no path is given.

A subscription is a remembered path, nothing more. There is no daemon and
nothing is written to your log files; loupe still only reads them when you run
it.

Everything subscribed is read onto one timeline, so subscribing to a second
location interleaves it with the first rather than replacing it.`,
		Example: `  loupe subscribe /var/log
  loupe subscribe ~/Downloads/incident-logs --label incident
  loupe                                   # now reads everything subscribed`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			work, err := workspace.Load(g.configDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			}

			for _, path := range args {
				sub, err := work.Subscribe(path, label)
				if err != nil {
					return err
				}
				fmt.Printf("Subscribed to %s\n", sub.Path)
			}

			fmt.Fprintf(os.Stderr, "\n%d location(s) now active. Run `loupe` to read them.\n",
				len(work.Active()))
			return nil
		},
	}

	cmd.Flags().StringVar(&label, "label", "", "display name for this location")
	return cmd
}

func newUnsubscribeCommand(g *globals) *cobra.Command {
	var forget bool

	cmd := &cobra.Command{
		Use:   "unsubscribe <directory>...",
		Short: "Stop reading a location, keeping its history and cache",
		Long: `Stop reading a location without forgetting it.

The entry stays in the list marked inactive, and its cached ingest is kept for
14 days, so re-subscribing during an incident is instant rather than a re-read.
Both the subscribe and the unsubscribe are recorded in the audit trail.

--forget removes the entry from the list as well. The audit trail still keeps
the record: the list is state, the trail is what happened.`,
		Example: `  loupe unsubscribe /var/log
  loupe unsubscribe /var/log --forget`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			work, err := workspace.Load(g.configDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			}

			for _, path := range args {
				if forget {
					if err := work.Forget(path); err != nil {
						return err
					}
					fmt.Printf("Forgot %s\n", path)
					continue
				}
				if err := work.Unsubscribe(path); err != nil {
					return err
				}
				fmt.Printf("Unsubscribed from %s\n", path)
			}

			fmt.Fprintf(os.Stderr,
				"\nCached records are kept for %d days, or until `loupe cache clear`.\n",
				int(store.DefaultRetention.Hours()/24))
			return nil
		},
	}

	cmd.Flags().BoolVar(&forget, "forget", false, "also remove the entry from the list")
	return cmd
}

func newSubscriptionsCommand(g *globals) *cobra.Command {
	var showAudit bool

	cmd := &cobra.Command{
		Use:     "subscriptions",
		Aliases: []string{"subs"},
		Short:   "List the remembered log locations",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			work, err := workspace.Load(g.configDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			}

			if showAudit {
				return printAudit(work)
			}
			return printSubscriptions(work)
		},
	}

	cmd.Flags().BoolVar(&showAudit, "audit", false, "show the trail of subscribe and unsubscribe events")
	return cmd
}

func printSubscriptions(work *workspace.Workspace) error {
	all := work.All()
	if len(all) == 0 {
		fmt.Println("No subscriptions. Add one with `loupe subscribe <directory>`.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tNAME\tPATH\tSINCE")

	for _, sub := range all {
		status := "active"
		since := sub.AddedAt.Local().Format("2006-01-02 15:04")

		if !sub.Active {
			status = "unsubscribed"
			if sub.RemovedAt != nil {
				since = sub.RemovedAt.Local().Format("2006-01-02 15:04")
			}
		}
		// A path that has gone away is worth saying, since it will silently
		// contribute nothing on the next run otherwise.
		if _, err := os.Stat(sub.Path); err != nil {
			status += " (missing)"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", status, sub.Name(), sub.Path, since)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}

	fmt.Printf("\n%d active. Audit trail: %s\n", len(work.Active()), work.AuditPath())
	return nil
}

func printAudit(work *workspace.Workspace) error {
	events, err := work.Audit(0)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Println("No events recorded yet.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tACTION\tPATH\tBY\tNOTE")

	for _, e := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			e.At.Local().Format("2006-01-02 15:04:05"), e.Action, e.Path, e.User, e.Note)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	fmt.Printf("\n%s\n", work.AuditPath())
	return nil
}
