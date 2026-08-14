// Command loupe is a single-binary log explorer.
//
// Point it at a directory of log files in mixed formats and get a searchable
// timeline, a filter language, and a local UI, with no external services.
//
//	loupe ./logs
//	loupe ./logs 'level:>=error last:15m'
//	loupe ./logs --ui
package main

import (
	"fmt"
	"os"
)

// version is overwritten at release time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loupe:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return fmt.Errorf("not implemented yet (version %s)", version)
}
