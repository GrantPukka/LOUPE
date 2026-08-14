//go:build linux || darwin

package render

import (
	"os"
	"syscall"
	"unsafe"
)

// ttyWidth asks the terminal how wide it is via the TIOCGWINSZ ioctl.
//
// Done with the standard library rather than a terminal package: it is a dozen
// lines, and the allowed dependency set is deliberately small.
func ttyWidth() int {
	var ws struct {
		rows, cols, xpixel, ypixel uint16
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0
	}
	return int(ws.cols)
}
