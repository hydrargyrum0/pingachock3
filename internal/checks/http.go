package checks

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type HTTPChecker struct{}

type httpParams struct {
	Method          string `json:"method"`
	FollowRedirects bool   `json:"follow_redirects"`
	TimeoutMs       int    `json:"timeout_ms"`
	ExpectStatus    int    `json:"expect_status"`
}

func (HTTPChecker) Run(ctx context.Context, netCfg NetConfig, target string, rawParams json.RawMessage) Result {
	var p httpParams
	if len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &p)
	}
	if p.Method == "" {
		p.Method = http.MethodGet
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 10000
	}

	url := target
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	dialer := &net.Dialer{Resolver: netCfg.Resolver, LocalAddr: localAddr("tcp", netCfg.LocalAddr)}
	client := &http.Client{
		Timeout:   time.Duration(p.TimeoutMs) * time.Millisecond,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	if !p.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	req, err := http.NewRequestWithContext(ctx, p.Method, url, nil)
	if err != nil {
		msg := err.Error()
		return Result{Success: false, ErrorMessage: &msg}
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := int(time.Since(start).Milliseconds())
	if err != nil {
		msg := err.Error()
		return Result{Success: false, LatencyMs: &elapsed, ErrorMessage: &msg}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	statusCode := strconv.Itoa(resp.StatusCode)
	success := resp.StatusCode < 400
	if p.ExpectStatus != 0 {
		success = resp.StatusCode == p.ExpectStatus
	}

	return Result{
		Success: success, LatencyMs: &elapsed, StatusCode: &statusCode,
		Raw: mustJSON(map[string]any{"final_url": resp.Request.URL.String()}),
	}
}
