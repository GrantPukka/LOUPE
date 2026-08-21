//go:build unix

package source

import "syscall"

// makeFIFO creates a named pipe, which is the cheapest thing to hand that is
// neither a regular file nor a directory. The walk must keep skipping it.
//
// syscall rather than golang.org/x/sys: the latter is in the build graph as an
// indirect dependency, and importing it here would promote it to a direct one —
// which CLAUDE.md says to ask about, over a two-line test helper.
func makeFIFO(path string) error { return syscall.Mkfifo(path, 0o600) }
