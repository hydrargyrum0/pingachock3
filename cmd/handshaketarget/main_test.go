package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dialAndUpgrade(t *testing.T, addr string, key string) (*bufio.Reader, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	req := "GET / HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return bufio.NewReader(conn), conn
}

func TestHandleUpgradeCompletesRealHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(handleUpgrade))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	reader, conn := dialAndUpgrade(t, addr, key)
	defer conn.Close()

	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", statusLine)
	}

	var acceptHeader string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Sec-WebSocket-Accept:") {
			acceptHeader = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Accept:"))
		}
	}

	sum := sha1.Sum([]byte(key + wsMagicGUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if acceptHeader != want {
		t.Errorf("Sec-WebSocket-Accept = %q, want %q", acceptHeader, want)
	}
}

func TestHandleUpgradeRejectsNonUpgradeRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(handleUpgrade))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}
