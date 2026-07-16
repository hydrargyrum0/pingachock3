//go:build windows

package main

import "golang.org/x/sys/windows"

// isElevated reports whether the current process token has admin rights -
// the standard way to check this on Windows short of actually attempting a
// privileged operation.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
