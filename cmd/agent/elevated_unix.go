//go:build !windows

package main

import "os"

// isElevated reports whether the current process is root - on
// macOS/Linux, that's what installing/controlling a system service
// (launchd/systemd) and writing to the standard install locations
// actually requires.
func isElevated() bool {
	return os.Geteuid() == 0
}
