//go:build !windows

package checks

// decodeConsoleOutput is a no-op on non-Windows: Linux/macOS's ping binary
// writes locale text using the process's own locale encoding (UTF-8 on any
// modern Linux/macOS install), which Go's string/regexp machinery already
// handles correctly with no extra conversion step. See ping_windows.go's
// doc comment for why Windows specifically needs one.
func decodeConsoleOutput(raw []byte) string {
	return string(raw)
}
