package public

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// openListener starts a TCP listener on 127.0.0.1 that accepts and
// immediately closes every connection, and returns its port.
func openListener(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port, func() { ln.Close() }
}

// closedPort returns a port on 127.0.0.1 nothing is listening on, so
// dialing it is refused.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	return port
}

// doServerPing does NOT call t.Fatal - it returns errors instead, so it's
// safe to call from a non-main test goroutine (the concurrency test below
// does exactly that; t.Fatal from a spawned goroutine is a testing bug).
func doServerPing(h *Handler, body any) (*httptest.ResponseRecorder, serverPingResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, serverPingResponse{}, err
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/server-ping", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServerPing(rec, req)

	var resp serverPingResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			return rec, serverPingResponse{}, err
		}
	}
	return rec, resp, nil
}

func TestServerPingOpenPort(t *testing.T) {
	port, closeFn := openListener(t)
	defer closeFn()

	h := &Handler{}
	rec, resp, err := doServerPing(h, map[string]any{
		"targets": []string{"127.0.0.1"},
		"ports":   []string{port},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Results))
	}
	if got := resp.Results[0].Ports[port]; got != "open" {
		t.Errorf("port %s = %q, want %q", port, got, "open")
	}
}

func TestServerPingClosedPort(t *testing.T) {
	port := closedPort(t)

	h := &Handler{}
	_, resp, err := doServerPing(h, map[string]any{
		"targets": []string{"127.0.0.1"},
		"ports":   []string{port},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Results[0].Ports[port]; got != "closed" {
		t.Errorf("port %s = %q, want %q", port, got, "closed")
	}
}

func TestServerPingMultipleTargetsAndPorts(t *testing.T) {
	openPort, closeFn := openListener(t)
	defer closeFn()
	shutPort := closedPort(t)

	h := &Handler{}
	_, resp, err := doServerPing(h, map[string]any{
		"targets": []string{"127.0.0.1", "127.0.0.1"},
		"ports":   []string{openPort, shutPort},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	for _, r := range resp.Results {
		if r.Ports[openPort] != "open" {
			t.Errorf("target %s: port %s = %q, want open", r.Target, openPort, r.Ports[openPort])
		}
		if r.Ports[shutPort] != "closed" {
			t.Errorf("target %s: port %s = %q, want closed", r.Target, shutPort, r.Ports[shutPort])
		}
	}
}

func TestServerPingEmptyTargets(t *testing.T) {
	h := &Handler{}
	rec, _, err := doServerPing(h, map[string]any{"targets": []string{}, "ports": []string{"80"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServerPingTooManyTargets(t *testing.T) {
	targets := make([]string, serverPingMaxTargets+1)
	for i := range targets {
		targets[i] = "127.0.0.1"
	}
	h := &Handler{}
	rec, _, err := doServerPing(h, map[string]any{"targets": targets, "ports": []string{"80"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServerPingInvalidPortValue(t *testing.T) {
	h := &Handler{}
	rec, _, err := doServerPing(h, map[string]any{
		"targets": []string{"127.0.0.1"},
		"ports":   []string{"not-a-port"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestServerPingConcurrentRequestsDoNotCrossTalk fires several concurrent
// requests, each asking about a *different* port, and checks each response
// only ever contains its own request's port - never another goroutine's.
// This is the regression test for the cross-talk bug described in
// docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md.
func TestServerPingConcurrentRequestsDoNotCrossTalk(t *testing.T) {
	const n = 8
	ports := make([]string, n)
	closers := make([]func(), n)
	for i := 0; i < n; i++ {
		port, closeFn := openListener(t)
		ports[i] = port
		closers[i] = closeFn
	}
	defer func() {
		for _, c := range closers {
			c()
		}
	}()

	h := &Handler{}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, resp, err := doServerPing(h, map[string]any{
				"targets": []string{"127.0.0.1"},
				"ports":   []string{ports[i]},
			})
			if err != nil {
				errs[i] = fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if len(resp.Results) != 1 {
				errs[i] = fmt.Errorf("goroutine %d: got %d results, want 1", i, len(resp.Results))
				return
			}
			got, ok := resp.Results[0].Ports[ports[i]]
			if !ok || got != "open" {
				errs[i] = fmt.Errorf("goroutine %d: requested port %s, response ports=%v - a different request's result leaked in", i, ports[i], resp.Results[0].Ports)
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
