//go:build windows

package checks

import (
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var procGetOEMCP = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")

// decodeConsoleOutput converts raw bytes captured from a Windows console
// program's redirected stdout/stderr (exactly what os/exec.Cmd.Stdout
// captures for ping.exe) into a proper UTF-8 Go string. This is not
// optional plumbing: a console program with its output redirected to a
// pipe (not a real console) writes using the system's OEM codepage - CP866
// on a default Russian-locale Windows install - regardless of the active
// console's own chcp setting (confirmed empirically: a real ping.exe reply
// line captured exactly the way ping.go captures it comes back as raw
// CP866 bytes even with chcp reporting 65001 in an interactive session).
//
// Every Cyrillic regex literal in parsePingOutput was comparing against
// these raw OEM-codepage bytes while Go's string/regexp machinery treated
// them as UTF-8; since CP866-encoded Cyrillic is byte-for-byte nothing
// like UTF-8-encoded Cyrillic, none of those regexes could ever match on a
// Russian-locale Windows node. That silently left avgMs at 0 for every
// single ping, falling back to elapsedMs - several *seconds* of
// wall-clock time (ping.exe's own ~1s-per-packet pacing across p.Count
// packets, not the real per-packet round trip) being reported as latency
// instead of the true, much smaller, per-packet time. See
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
// Section B - that fix added the Cyrillic literals but assumed the
// captured text would already be UTF-8, which turned out not to be true.
func decodeConsoleOutput(raw []byte) string {
	cp, _, _ := procGetOEMCP.Call()
	return decodeCodePage(raw, uint32(cp))
}

// decodeCodePage is decodeConsoleOutput's actual conversion logic, split
// out so it's unit-testable against a known, fixed codepage rather than
// whatever GetOEMCP happens to return on the machine running the test -
// mirrors this package's other split-pure-logic-from-syscall-plumbing
// pattern (e.g. internal/checks/tls.go's classifyUnreachable vs
// diagnosticPingReceived). Falls back to the raw bytes reinterpreted as-is
// if the conversion itself fails for any reason - better a possibly-
// mangled string than losing the output entirely.
func decodeCodePage(raw []byte, codePage uint32) string {
	if len(raw) == 0 {
		return ""
	}
	n, err := windows.MultiByteToWideChar(codePage, 0, &raw[0], int32(len(raw)), nil, 0)
	if err != nil || n == 0 {
		return string(raw)
	}
	buf := make([]uint16, n)
	n, err = windows.MultiByteToWideChar(codePage, 0, &raw[0], int32(len(raw)), &buf[0], n)
	if err != nil || n == 0 {
		return string(raw)
	}
	return string(utf16.Decode(buf[:n]))
}
