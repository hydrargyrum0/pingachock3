package transport

import (
	"net"
	"net/http"
	"time"
)

// NewDirect builds a transport that talks straight to the backend's
// domain/IP. If localAddr is set (operator picked a specific network
// interface via `configure`), outgoing connections are bound to it instead
// of whatever the OS would pick as the default route.
func NewDirect(localAddr net.IP, baseURL, nodeSecret string, timeout time.Duration) *HTTPTransport {
	dialer := &net.Dialer{Timeout: timeout}
	if localAddr != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: localAddr}
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	return NewHTTPTransport(client, baseURL, nodeSecret)
}
