package checks

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMatchSwitchingProtocols(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"real 101", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n", true},
		{"400", "HTTP/1.1 400 Bad Request\r\n\r\n", false},
		{"200", "HTTP/1.1 200 OK\r\n\r\n", false},
		{"empty", "", false},
		{"garbage", "not even http", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchSwitchingProtocols([]byte(tc.body)); got != tc.want {
				t.Errorf("matchSwitchingProtocols(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestBuildUpgradeRequestWebsocket(t *testing.T) {
	req := string(buildUpgradeRequest("example.com", "websocket"))
	for _, want := range []string{
		"GET / HTTP/1.1\r\n",
		"Host: example.com\r\n",
		"Connection: Upgrade\r\n",
		"Upgrade: websocket\r\n",
		"Sec-WebSocket-Version: 13\r\n",
		"Sec-WebSocket-Key: ",
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request missing %q:\n%s", want, req)
		}
	}
	if !strings.HasSuffix(req, "\r\n\r\n") {
		t.Errorf("request must end with a blank line, got:\n%q", req)
	}
}

// startRawServer runs handle for every accepted connection on a fresh
// 127.0.0.1 port, until closeFn is called.
func startRawServer(t *testing.T, handle func(net.Conn)) (port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(portStr)
	return port, func() { ln.Close() }
}

func TestUpgradeCheckerMatchesRealSwitchingProtocols(t *testing.T) {
	port, closeFn := startRawServer(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // drain the request
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	})
	defer closeFn()

	params, _ := json.Marshal(map[string]any{"port": port})
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if !res.Success {
		errMsg := "nil"
		if res.ErrorMessage != nil {
			errMsg = *res.ErrorMessage
		}
		t.Fatalf("Success = false, want true (error: %s)", errMsg)
	}
}

func TestUpgradeCheckerDoesNotMatchNormalResponse(t *testing.T) {
	port, closeFn := startRawServer(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
	})
	defer closeFn()

	params, _ := json.Marshal(map[string]any{"port": port})
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if res.Success {
		t.Fatal("Success = true, want false - server answered 400, not 101")
	}
	if res.ErrorMessage != nil {
		t.Errorf("ErrorMessage = %v, want nil - a normal non-101 response isn't a transport error", *res.ErrorMessage)
	}
}

func TestUpgradeCheckerConnectionRefused(t *testing.T) {
	// Port 1 is reserved (tcpmux) and nothing should ever be listening on
	// it - dial should fail fast and consistently across environments.
	params, _ := json.Marshal(map[string]any{"port": 1, "timeout_ms": 1000})
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)

	if res.Success {
		t.Fatal("Success = true, want false - nothing is listening")
	}
	if res.ErrorMessage == nil {
		t.Fatal("ErrorMessage is nil, want a connection error")
	}
}

func TestUpgradeCheckerTimeout(t *testing.T) {
	port, closeFn := startRawServer(t, func(conn net.Conn) {
		defer conn.Close()
		time.Sleep(2 * time.Second) // accept, then just sit on it - never respond
	})
	defer closeFn()

	params, _ := json.Marshal(map[string]any{"port": port, "timeout_ms": 300})

	start := time.Now()
	res := UpgradeChecker{}.Run(context.Background(), NetConfig{}, "127.0.0.1", params)
	elapsed := time.Since(start)

	if res.Success {
		t.Fatal("Success = true, want false - server never responded")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %v, want it bounded by the ~300ms timeout_ms, not hanging until the server closes", elapsed)
	}
}
