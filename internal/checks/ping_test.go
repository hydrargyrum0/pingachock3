package checks

import (
	"context"
	"net"
	"pingachock/internal/netiface"
	"runtime"
	"strings"
	"testing"
)

func TestParsePingOutputWindowsEnglish(t *testing.T) {
	output := "\r\nPinging 1.1.1.1 with 32 bytes of data:\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=2ms TTL=58\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=2ms TTL=58\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=2ms TTL=58\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=2ms TTL=58\r\n\r\n" +
		"Ping statistics for 1.1.1.1:\r\n" +
		"    Packets: Sent = 4, Received = 4, Lost = 0 (0% loss),\r\n" +
		"Approximate round trip times in milli-seconds:\r\n" +
		"    Minimum = 2ms, Maximum = 2ms, Average = 2ms\r\n"

	sent, recv, avg := parsePingOutput(output)
	if sent != 4 || recv != 4 || avg != 2 {
		t.Errorf("parsePingOutput() = sent=%d recv=%d avg=%v, want sent=4 recv=4 avg=2", sent, recv, avg)
	}
}

// TestParsePingOutputWindowsLocalized guards against a real false negative
// hit on a Russian-locale Windows node (OEMCP 866 - "language for
// non-Unicode programs" = Russian): ping.exe itself succeeded (4/4 replies,
// process exit code 0), but its "Sent =" / "Received =" / "Average =" labels
// come out as Russian text, so windowsStatsRe/windowsAvgRe never match and
// recv silently stayed 0 - flipping a genuinely successful check to
// success=false, error "no reply". The average used to have the same bug:
// "Average = 2ms" never matches "Среднее = 2мс", silently leaving avgMs at
// 0 and falling through to a several-*second* wall-clock fallback for the
// whole 4-ping run instead of a real ~2ms RTT.
func TestParsePingOutputWindowsLocalized(t *testing.T) {
	output := "\r\nОбмен пакетами с 1.1.1.1 по 32 байт:\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n\r\n" +
		"Статистика Ping для 1.1.1.1:\r\n" +
		"    Пакетов: отправлено = 4, получено = 4, потеряно = 0\r\n" +
		"    (0% потерь)\r\n"

	_, recv, avg := parsePingOutput(output)
	if recv != 4 {
		t.Errorf("parsePingOutput() recv=%d, want 4 (should fall back to counting TTL= when labels don't match English)", recv)
	}
	if avg != 2 {
		t.Errorf("parsePingOutput() avg=%v, want 2 (per-reply RTT anchored on TTL=, not the localized Average= line)", avg)
	}
}

func TestParsePingOutputNoReply(t *testing.T) {
	output := "\r\nPinging 1.1.1.1 with 32 bytes of data:\r\n" +
		"Request timed out.\r\nRequest timed out.\r\nRequest timed out.\r\nRequest timed out.\r\n\r\n" +
		"Ping statistics for 1.1.1.1:\r\n    Packets: Sent = 4, Received = 0, Lost = 4 (100% loss),\r\n"

	_, recv, avg := parsePingOutput(output)
	if recv != 0 {
		t.Errorf("parsePingOutput() recv=%d, want 0 (genuine no-reply must not be miscounted via the TTL= fallback)", recv)
	}
	if avg != 0 {
		t.Errorf("parsePingOutput() avg=%v, want 0 - no replies means nothing to average", avg)
	}
}

// TestParsePingOutputWindowsPartialLossAveragesOnlyReceived: 3 of 4 replies
// came back with different RTTs (2ms, 4ms, 6ms) - the average must be over
// just those three (4ms), not skewed by the missing fourth packet, and recv
// must reflect the real 3, not 4.
func TestParsePingOutputWindowsPartialLossAveragesOnlyReceived(t *testing.T) {
	output := "\r\nPinging 1.1.1.1 with 32 bytes of data:\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=2ms TTL=58\r\n" +
		"Request timed out.\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=4ms TTL=58\r\n" +
		"Reply from 1.1.1.1: bytes=32 time=6ms TTL=58\r\n\r\n" +
		"Ping statistics for 1.1.1.1:\r\n" +
		"    Packets: Sent = 4, Received = 3, Lost = 1 (25% loss),\r\n" +
		"Approximate round trip times in milli-seconds:\r\n" +
		"    Minimum = 2ms, Maximum = 6ms, Average = 4ms\r\n"

	sent, recv, avg := parsePingOutput(output)
	if sent != 4 || recv != 3 {
		t.Errorf("parsePingOutput() sent=%d recv=%d, want sent=4 recv=3", sent, recv)
	}
	if avg != 4 {
		t.Errorf("parsePingOutput() avg=%v, want 4 (average of 2, 4, 6 - the three actually-received replies)", avg)
	}
}

// TestParsePingOutputWindowsSubMillisecond: Windows spells sub-millisecond
// replies "time<1ms" rather than "time=Nms" - must not be dropped/miscounted.
func TestParsePingOutputWindowsSubMillisecond(t *testing.T) {
	output := "\r\nPinging 127.0.0.1 with 32 bytes of data:\r\n" +
		"Reply from 127.0.0.1: bytes=32 time<1ms TTL=128\r\n" +
		"Reply from 127.0.0.1: bytes=32 time<1ms TTL=128\r\n\r\n" +
		"Ping statistics for 127.0.0.1:\r\n" +
		"    Packets: Sent = 2, Received = 2, Lost = 0 (0% loss),\r\n"

	sent, recv, avg := parsePingOutput(output)
	if sent != 2 || recv != 2 {
		t.Errorf("parsePingOutput() sent=%d recv=%d, want sent=2 recv=2", sent, recv)
	}
	if avg != 1 {
		t.Errorf("parsePingOutput() avg=%v, want 1 (rounds \"<1ms\" up to the nearest whole ms)", avg)
	}
}

// TestParsePingOutputWindowsIPv6NoTTL: Windows never prints "TTL=" for IPv6
// replies (ping.exe just omits hop-limit reporting), so the TTL-anchored
// windowsReplyTimeRe alone finds nothing and averaging must fall back to
// windowsReplyTimeNoTTLRe instead of silently returning avg=0 (which the
// caller would then read as "fall back to several-second wall-clock time").
func TestParsePingOutputWindowsIPv6NoTTL(t *testing.T) {
	output := "\r\nPinging 2606:4700:4700::1111 with 32 bytes of data:\r\n" +
		"Reply from 2606:4700:4700::1111: time=15ms\r\n" +
		"Reply from 2606:4700:4700::1111: time=14ms\r\n" +
		"Reply from 2606:4700:4700::1111: time=16ms\r\n" +
		"Reply from 2606:4700:4700::1111: time=15ms\r\n\r\n" +
		"Ping statistics for 2606:4700:4700::1111:\r\n" +
		"    Packets: Sent = 4, Received = 4, Lost = 0 (0% loss),\r\n" +
		"Approximate round trip times in milli-seconds:\r\n" +
		"    Minimum = 14ms, Maximum = 16ms, Average = 15ms\r\n"

	sent, recv, avg := parsePingOutput(output)
	if sent != 4 || recv != 4 {
		t.Errorf("parsePingOutput() sent=%d recv=%d, want sent=4 recv=4", sent, recv)
	}
	if avg != 15 {
		t.Errorf("parsePingOutput() avg=%v, want 15 (real RTT average of 15,14,16,15), not 0 falling back to wall-clock time", avg)
	}
}

func TestClassifyPingError(t *testing.T) {
	cases := []struct {
		name             string
		cmdCtxErr        error
		resolutionFailed bool
		recv             int
		want             string
	}{
		{"deadline exceeded wins over everything else", context.DeadlineExceeded, true, 0, "timeout"},
		{"dns resolution failed", nil, true, 0, "dns resolution failed"},
		{"no reply, resolution fine", nil, false, 0, "no reply"},
		{"generic failure with some replies received", nil, false, 2, "ping failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPingError(tc.cmdCtxErr, tc.resolutionFailed, tc.recv); got != tc.want {
				t.Errorf("classifyPingError(%v, %v, %d) = %q, want %q", tc.cmdCtxErr, tc.resolutionFailed, tc.recv, got, tc.want)
			}
		})
	}
}

func TestPingArgsLinuxUsesInterfaceNameNotAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pingArgs' Linux branch only runs its interface-name behavior on GOOS=linux")
	}
	ifc := netiface.Interface{Name: "eth-test", Addrs: []net.IP{net.ParseIP("192.168.1.50")}}
	args := pingArgs("203.0.113.5", 4, 5000, ifc)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-I eth-test") {
		t.Errorf("pingArgs args = %v, want \"-I eth-test\" (bind by interface name, not address)", args)
	}
	if strings.Contains(joined, "192.168.1.50") {
		t.Errorf("pingArgs args = %v, want the interface's address NOT to appear - -I takes the name directly on Linux", args)
	}
}

func TestPingArgsNoInterfacePinnedOmitsBindFlag(t *testing.T) {
	args := pingArgs("203.0.113.5", 4, 5000, netiface.Interface{})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-S") || strings.Contains(joined, "-I") {
		t.Errorf("pingArgs args = %v, want no -S/-I flag when no interface is pinned", args)
	}
}
