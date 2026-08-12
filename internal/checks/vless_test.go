package checks

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPatchInboundAddsSocksWhenNoneExists(t *testing.T) {
	input := json.RawMessage(`{"outbounds":[{"protocol":"vless"}]}`)
	patched, err := patchInbound(input, 12345)
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}

	inbounds, ok := doc["inbounds"].([]any)
	if !ok || len(inbounds) != 1 {
		t.Fatalf("inbounds = %v, want exactly one entry", doc["inbounds"])
	}
	entry := inbounds[0].(map[string]any)
	if entry["protocol"] != "socks" {
		t.Errorf("inbound protocol = %v, want socks", entry["protocol"])
	}
	if entry["listen"] != "127.0.0.1" {
		t.Errorf("inbound listen = %v, want 127.0.0.1", entry["listen"])
	}
	if int(entry["port"].(float64)) != 12345 {
		t.Errorf("inbound port = %v, want 12345", entry["port"])
	}

	// outbounds must survive untouched
	outbounds, ok := doc["outbounds"].([]any)
	if !ok || len(outbounds) != 1 {
		t.Fatalf("outbounds = %v, want the original single entry preserved", doc["outbounds"])
	}
}

func TestPatchInboundReplacesExistingInbounds(t *testing.T) {
	input := json.RawMessage(`{"inbounds":[{"protocol":"http","port":8080}],"outbounds":[{"protocol":"vless"}]}`)
	patched, err := patchInbound(input, 55555)
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}

	inbounds := doc["inbounds"].([]any)
	if len(inbounds) != 1 {
		t.Fatalf("inbounds = %d entries, want exactly 1 (the caller's http inbound must be discarded)", len(inbounds))
	}
	entry := inbounds[0].(map[string]any)
	if entry["protocol"] != "socks" {
		t.Errorf("inbound protocol = %v, want socks (the caller's original http inbound must not survive)", entry["protocol"])
	}
	if int(entry["port"].(float64)) != 55555 {
		t.Errorf("inbound port = %v, want 55555", entry["port"])
	}
}

// TestPatchInboundOverridesLogConfig: the reported bug was a client-exported
// config's log.access pointing at a path only the exporting app's own OS
// sandbox has (e.g. an iOS App Group container) - xray-core fails to open
// it at startup and exits before it can even log why, showing up as "xray
// failed to start (no output)". patchInbound must discard whatever "log"
// the caller supplied, the same way it already discards "inbounds".
func TestPatchInboundOverridesLogConfig(t *testing.T) {
	input := json.RawMessage(`{
		"log": {"access": "/private/var/mobile/Containers/Shared/AppGroup/whatever/access.log", "dnsLog": true, "loglevel": "Error"},
		"outbounds": [{"protocol": "vless"}]
	}`)
	patched, err := patchInbound(input, 12345)
	if err != nil {
		t.Fatalf("patchInbound() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}

	logCfg, ok := doc["log"].(map[string]any)
	if !ok {
		t.Fatalf("log = %v (%T), want an object", doc["log"], doc["log"])
	}
	if _, hasAccess := logCfg["access"]; hasAccess {
		t.Errorf("log.access = %v, want it stripped - the caller's path doesn't exist on this node", logCfg["access"])
	}
	if _, hasError := logCfg["error"]; hasError {
		t.Errorf("log.error = %v, want it stripped", logCfg["error"])
	}
}

func TestPatchInboundRejectsInvalidJSON(t *testing.T) {
	_, err := patchInbound(json.RawMessage(`not json`), 1234)
	if err == nil {
		t.Fatal("patchInbound() error = nil, want an error for invalid JSON")
	}
}

func TestFreePortReturnsUsablePorts(t *testing.T) {
	a, err := freePort()
	if err != nil {
		t.Fatalf("freePort() error = %v", err)
	}
	if a <= 0 {
		t.Errorf("freePort() = %d, want a positive port number", a)
	}
	b, err := freePort()
	if err != nil {
		t.Fatalf("freePort() error = %v", err)
	}
	if b <= 0 {
		t.Errorf("freePort() = %d, want a positive port number", b)
	}
}

func TestClassifyXrayStartupError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{"empty", "", "xray failed to start (no output)"},
		{"single line", "config: invalid protocol", "xray config error: config: invalid protocol"},
		{"multi line takes last", "line one\nline two\nreal error here", "xray config error: real error here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyXrayStartupError(tc.stderr); got != tc.want {
				t.Errorf("classifyXrayStartupError(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

func TestEnsureXrayBinaryInWritesAndReusesCache(t *testing.T) {
	// Swap the package-level embedded bytes for a small fake payload so
	// this test doesn't depend on (or exercise) the real ~35MB binary -
	// ensureXrayBinaryIn only cares about the byte slice's identity/length,
	// never its actual content.
	original := embeddedXrayBinary
	embeddedXrayBinary = []byte("fake-xray-binary-for-testing")
	defer func() { embeddedXrayBinary = original }()

	dir := t.TempDir()

	path1, err := ensureXrayBinaryIn(dir)
	if err != nil {
		t.Fatalf("ensureXrayBinaryIn() error = %v", err)
	}
	info1, err := os.Stat(path1)
	if err != nil {
		t.Fatalf("stat %s: %v", path1, err)
	}
	if info1.Size() != int64(len(embeddedXrayBinary)) {
		t.Errorf("written file size = %d, want %d", info1.Size(), len(embeddedXrayBinary))
	}

	// Second call must reuse the same file, not rewrite it - if it
	// rewrites, the mtime would move forward; sleep briefly so a
	// wrongly-rewritten file would show a detectably later mtime.
	time.Sleep(20 * time.Millisecond)
	path2, err := ensureXrayBinaryIn(dir)
	if err != nil {
		t.Fatalf("ensureXrayBinaryIn() second call error = %v", err)
	}
	if path2 != path1 {
		t.Errorf("second call path = %q, want the same path %q", path2, path1)
	}
	info2, err := os.Stat(path2)
	if err != nil {
		t.Fatalf("stat %s: %v", path2, err)
	}
	if !info2.ModTime().Equal(info1.ModTime()) {
		t.Errorf("mtime changed between calls (%v -> %v) - ensureXrayBinaryIn rewrote a file it should have reused", info1.ModTime(), info2.ModTime())
	}
}
