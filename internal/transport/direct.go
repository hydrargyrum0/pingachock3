package transport

import (
	"log/slog"
	"net"
	"net/http"
	"time"
)

func directDialer(localAddr net.IP, timeout time.Duration) *net.Dialer {
	d := &net.Dialer{Timeout: timeout}
	if localAddr != nil {
		d.LocalAddr = &net.TCPAddr{IP: localAddr}
	}
	return d
}

// NewDirect builds a transport that talks straight to the backend's
// domain/IP. If localAddr is set (operator picked a specific network
// interface via `configure`), outgoing connections are bound to it instead
// of whatever the OS would pick as the default route.
func NewDirect(localAddr net.IP, baseURL, nodeSecret string, timeout time.Duration, log *slog.Logger) *HTTPTransport {
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: directDialer(localAddr, timeout).DialContext},
	}
	return NewHTTPTransport(client, baseURL, nodeSecret, "direct", log)
}
