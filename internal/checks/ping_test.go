package checks

import "testing"

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
// success=false, error "no reply".
func TestParsePingOutputWindowsLocalized(t *testing.T) {
	output := "\r\nОбмен пакетами с 1.1.1.1 по 32 байт:\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n" +
		"Ответ от 1.1.1.1: число байт=32 время=2мс TTL=58\r\n\r\n" +
		"Статистика Ping для 1.1.1.1:\r\n" +
		"    Пакетов: отправлено = 4, получено = 4, потеряно = 0\r\n" +
		"    (0% потерь)\r\n"

	_, recv, _ := parsePingOutput(output)
	if recv != 4 {
		t.Errorf("parsePingOutput() recv=%d, want 4 (should fall back to counting TTL= when labels don't match English)", recv)
	}
}

func TestParsePingOutputNoReply(t *testing.T) {
	output := "\r\nPinging 1.1.1.1 with 32 bytes of data:\r\n" +
		"Request timed out.\r\nRequest timed out.\r\nRequest timed out.\r\nRequest timed out.\r\n\r\n" +
		"Ping statistics for 1.1.1.1:\r\n    Packets: Sent = 4, Received = 0, Lost = 4 (100% loss),\r\n"

	_, recv, _ := parsePingOutput(output)
	if recv != 0 {
		t.Errorf("parsePingOutput() recv=%d, want 0 (genuine no-reply must not be miscounted via the TTL= fallback)", recv)
	}
}
