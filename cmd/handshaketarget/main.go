// cmd/handshaketarget is a placeholder WebSocket-upgrading TCP listener -
// see docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md. It
// exists purely as something for a user's own TLS-terminating relay to
// forward decrypted traffic to when measuring TLS handshake speed against
// that relay (internal/checks/tls.go's TLSChecker, dispatched from the
// bot's "TLS Handshake" flow) - it has no real function beyond completing
// the WebSocket opening handshake and holding the connection open. No TLS
// here: whatever fronts this already terminated TLS and forwards
// plaintext.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"strings"
)

const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func handleUpgrade(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected a websocket upgrade request", http.StatusBadRequest)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	sum := sha1.Sum([]byte(key + wsMagicGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := buf.Flush(); err != nil {
		return
	}

	// Hold the connection open, discarding whatever arrives, until the
	// peer closes it - there is nothing else for this placeholder to do.
	discard := make([]byte, 4096)
	for {
		if _, err := conn.Read(discard); err != nil {
			return
		}
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "1343"
	}
	log.Printf("handshake-target listening on :%s", port)
	if err := http.ListenAndServe(":"+port, http.HandlerFunc(handleUpgrade)); err != nil {
		log.Fatal(err)
	}
}
