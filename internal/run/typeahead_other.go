//go:build !linux

package run

import "os"

// flushTTYInput is a no-op where the TCFLSH ioctl is not there to call: only
// the bytes the bufio reader already holds are dropped.
func flushTTYInput(_ *os.File) {}
