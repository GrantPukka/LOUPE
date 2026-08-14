// Command loupe is a single-binary log explorer.
//
// Point it at a directory of log files in mixed formats and get a searchable
// timeline, a filter language, and a local UI, with no external services.
//
//	loupe ./logs
//	loupe ./logs 'level:>=error last:15m'
//	loupe sql "SELECT level, count(*) FROM logs GROUP BY 1"
package main

import (
	"errors"
	"fmt"
	"os"
)

// version is overwritten at release time via -ldflags.
var version = "dev"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		// Cobra has already printed the message for usage errors.
		if !errors.Is(err, errPrinted) {
			fmt.Fprintln(os.Stderr, "loupe:", err)
		}
		os.Exit(1)
	}
}

// errPrinted marks an error whose message has already reached the user, so the
// top level does not print it twice.
var errPrinted = errors.New("error already reported")
