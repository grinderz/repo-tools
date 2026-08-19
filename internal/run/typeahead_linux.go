//go:build linux

package run

import (
	"os"
	"syscall"
)

// tcflsh is the TCFLSH ioctl; the syscall package stopped exporting the
// termios constants, and pulling x/sys in for one number is not worth it.
const (
	tcflsh   = 0x540B
	tciflush = 0
)

// flushTTYInput empties the kernel's input queue of a terminal, the part of
// the type-ahead a bufio reader never saw.
func flushTTYInput(f *os.File) {
	//nolint:dogsled // the ioctl is fire-and-forget: a failed flush just keeps the type-ahead
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tcflsh, tciflush)
}
