package transport

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// NewFronted builds a transport that TLS-connects to frontDomain (the
// disguise SNI, e.g. a popular domain that isn't blocked) while sending the
// actual Cloud Run proxy's hostname as the Host header - the domain
// fronting trick described in docs/ARCHITECTURE.md. Everything below the
// TLS handshake is identical to Direct. localAddr behaves as in NewDirect.
func NewFronted(localAddr net.IP, frontDomain, realHost, nodeSecret string, timeout time.Duration, log *slog.Logger) *HTTPTransport {
	dialer := &net.Dialer{Timeout: timeout}
	if localAddr != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: localAddr}
	}
	rt := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// Deliberately ignore the addr the stdlib derives from the
			// request URL (which is realHost) - always dial frontDomain.
			addr := net.JoinHostPort(frontDomain, "443")
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				if log != nil {
					log.Debug("fronted dial failed", "front_domain", frontDomain, "addr", addr, "error", err)
				}
				return nil, err
			}
			tlsConn := tls.Client(conn, &tls.Config{ServerName: frontDomain})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				conn.Close()
				if log != nil {
					log.Debug("fronted TLS handshake failed", "front_domain", frontDomain, "error", err)
				}
				return nil, err
			}
			if log != nil {
				log.Debug("fronted TLS handshake ok", "front_domain", frontDomain, "real_host", realHost)
			}
			return tlsConn, nil
		},
	}
	client := &http.Client{Transport: rt, Timeout: timeout}
	return NewHTTPTransport(client, "https://"+realHost, nodeSecret, "fronted", log)
}
