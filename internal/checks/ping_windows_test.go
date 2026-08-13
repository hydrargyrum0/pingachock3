//go:build windows

package checks

import "testing"

// TestDecodeCodePage866RealReplyLine uses the exact CP866 bytes a real
// ping.exe reply line produces on a Russian-locale Windows box - captured
// empirically (see decodeConsoleOutput's doc comment), not a synthetic
// guess at what CP866 might look like.
func TestDecodeCodePage866RealReplyLine(t *testing.T) {
	raw := []byte{
		0x8e, 0xe2, 0xa2, 0xa5, 0xe2, 0x20, 0xae, 0xe2, 0x20, 0x31, 0x2e, 0x31,
		0x2e, 0x31, 0x2e, 0x31, 0x3a, 0x20, 0xe7, 0xa8, 0xe1, 0xab, 0xae, 0x20,
		0xa1, 0xa0, 0xa9, 0xe2, 0x3d, 0x33, 0x32, 0x20, 0xa2, 0xe0, 0xa5, 0xac,
		0xef, 0x3d, 0x32, 0xac, 0xe1, 0x20, 0x54, 0x54, 0x4c, 0x3d, 0x35, 0x38,
	}
	const cp866 = 866

	got := decodeCodePage(raw, cp866)
	want := "Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58"
	if got != want {
		t.Errorf("decodeCodePage(cp866 bytes) = %q, want %q", got, want)
	}
}

func TestDecodeCodePageEmptyInput(t *testing.T) {
	if got := decodeCodePage(nil, 866); got != "" {
		t.Errorf("decodeCodePage(nil) = %q, want empty string", got)
	}
}

// TestDecodeConsoleOutputFixesRegexMatching is the actual bug, made
// concrete: before this fix, parsePingOutput's Cyrillic-anchored regexes
// never matched raw CP866 bytes misread as UTF-8 at all, silently leaving
// avgMs at 0 for every single ping on a Russian-locale Windows node.
func TestDecodeConsoleOutputFixesRegexMatching(t *testing.T) {
	raw := []byte{
		0x8e, 0xe2, 0xa2, 0xa5, 0xe2, 0x20, 0xae, 0xe2, 0x20, 0x31, 0x2e, 0x31,
		0x2e, 0x31, 0x2e, 0x31, 0x3a, 0x20, 0xe7, 0xa8, 0xe1, 0xab, 0xae, 0x20,
		0xa1, 0xa0, 0xa9, 0xe2, 0x3d, 0x33, 0x32, 0x20, 0xa2, 0xe0, 0xa5, 0xac,
		0xef, 0x3d, 0x32, 0xac, 0xe1, 0x20, 0x54, 0x54, 0x4c, 0x3d, 0x35, 0x38,
	}

	decoded := decodeCodePage(raw, 866)
	_, _, avgMs := parsePingOutput(decoded)
	if avgMs != 2 {
		t.Errorf("parsePingOutput on decoded CP866 reply: avgMs = %v, want 2 (the real per-packet time, not 0 falling back to multi-second wall-clock elapsed)", avgMs)
	}

	// The bug, demonstrated directly: without decoding, the exact same
	// bytes naively treated as UTF-8 (which is what out.String() used to
	// do) never match any regex, and avgMs stays 0.
	_, _, undecodedAvgMs := parsePingOutput(string(raw))
	if undecodedAvgMs != 0 {
		t.Errorf("parsePingOutput on raw undecoded CP866 bytes: avgMs = %v, want 0 - this is the bug this fix closes, demonstrated for contrast", undecodedAvgMs)
	}
}
