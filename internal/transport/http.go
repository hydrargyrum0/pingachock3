package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// HTTPTransport speaks the two-endpoint agent protocol (poll/results) over
// a given *http.Client - the client is what actually differs between Direct
// and Fronted (see direct.go / fronted.go). label identifies which one this
// is ("direct"/"fronted") for logging and for the status menu.
type HTTPTransport struct {
	client     *http.Client
	baseURL    string
	nodeSecret string
	label      string
	log        *slog.Logger
}

func NewHTTPTransport(client *http.Client, baseURL, nodeSecret, label string, log *slog.Logger) *HTTPTransport {
	return &HTTPTransport{client: client, baseURL: strings.TrimRight(baseURL, "/"), nodeSecret: nodeSecret, label: label, log: log}
}

func (t *HTTPTransport) Name() string { return t.label }

func (t *HTTPTransport) Poll(ctx context.Context, agentVersion string) ([]Job, error) {
	body, err := json.Marshal(map[string]string{"agent_version": agentVersion, "platform": runtime.GOOS})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Jobs []Job `json:"jobs"`
	}
	if err := t.do(ctx, "/api/v1/agent/poll", body, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

func (t *HTTPTransport) PostResults(ctx context.Context, results []ResultSubmission) error {
	if len(results) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return err
	}
	return t.do(ctx, "/api/v1/agent/results", body, nil)
}

func (t *HTTPTransport) do(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.nodeSecret)

	start := time.Now()
	resp, err := t.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.logDebug("http request failed", "url", t.baseURL+path, "elapsed_ms", elapsed.Milliseconds(), "error", err)
		return err
	}
	defer resp.Body.Close()
	t.logDebug("http request", "url", t.baseURL+path, "elapsed_ms", elapsed.Milliseconds(), "status", resp.StatusCode)

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, string(b))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *HTTPTransport) logDebug(msg string, args ...any) {
	if t.log == nil {
		return
	}
	t.log.Debug(msg, append([]any{"transport", t.label}, args...)...)
}
