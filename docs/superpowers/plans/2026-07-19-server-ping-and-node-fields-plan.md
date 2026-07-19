# server-ping эндпоинт + nodes.blocked/platform — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a synchronous `POST /api/v1/server-ping` endpoint (ICMP+TCP directly from
the backend, no node involved) and small `nodes.blocked`/`nodes.platform` additions,
per `docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md`.

**Architecture:** Backend-only, Part 1 of the spec. Ships and is fully testable/usable
standalone (curl, same as every manual test done against this backend so far) — the
Telegram bot integration (Part 2) is a separate follow-up plan once this is live.
`server-ping` is deliberately decoupled from `checks`/`check_runs`/`nodes` entirely: no
DB access, no shared state between requests, reuses the existing `internal/checks`
package the node agent already uses. `nodes.blocked`/`nodes.platform` follow the exact
pattern already used for `agent_version` (store column + `SET*` method + poll payload).

**Tech Stack:** Go 1.26, `chi` router, Postgres (`pgx`), existing `internal/checks`
(`PingChecker`/`TCPChecker`), existing custom migration runner
(`internal/store.RunMigrations` — numeric-prefixed `*.up.sql`/`*.down.sql`, no external
library).

---

## Before starting

Local dev stack must be up (see `README.md`):

```sh
docker compose up -d
```

Postgres is reachable at `localhost:5433`. The server applies migrations automatically
on startup. `ADMIN_TOKEN` below is whatever you export for local dev — pick any string,
e.g. `dev-admin-token`.

---

### Task 1: Migration — `nodes.blocked` / `nodes.platform`

**Files:**
- Create: `migrations/0002_node_blocked_platform.up.sql`
- Create: `migrations/0002_node_blocked_platform.down.sql`

- [ ] **Step 1: Write the migration**

`migrations/0002_node_blocked_platform.up.sql`:
```sql
ALTER TABLE nodes ADD COLUMN blocked boolean NOT NULL DEFAULT false;
ALTER TABLE nodes ADD COLUMN platform text NOT NULL DEFAULT '';
```

`migrations/0002_node_blocked_platform.down.sql`:
```sql
ALTER TABLE nodes DROP COLUMN platform;
ALTER TABLE nodes DROP COLUMN blocked;
```

- [ ] **Step 2: Apply it and verify**

```sh
DATABASE_URL="postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable" \
ADMIN_TOKEN="dev-admin-token" \
go run ./cmd/server
```

Expected in the log: `migrations applied` with no error, then `listening addr=:8080`.
Leave it running (used by later tasks) or Ctrl-C and confirm the columns landed:

```sh
docker compose exec postgres psql -U pingachock -c "\d nodes"
```

Expected: `blocked` (`boolean`, not null, default `false`) and `platform` (`text`, not
null, default `''`) appear in the column list.

- [ ] **Step 3: Commit**

```sh
git add migrations/0002_node_blocked_platform.up.sql migrations/0002_node_blocked_platform.down.sql
git commit -m "Add nodes.blocked and nodes.platform columns"
```

---

### Task 2: Store layer — read/write `blocked`/`platform`

**Files:**
- Modify: `internal/store/models.go`
- Modify: `internal/store/nodes.go`

- [ ] **Step 1: Add the fields to `Node`**

In `internal/store/models.go`, `Node` struct — add `Blocked` and `Platform`:

```go
type Node struct {
	ID              uuid.UUID
	Name            string
	ISP             string
	City            string
	Country         string
	AgentVersion    string
	Platform        string
	LastHeartbeatAt *time.Time
	SecretHash      string
	Blocked         bool
	Tags            json.RawMessage
	Metadata        json.RawMessage
	CreatedAt       time.Time
}
```

- [ ] **Step 2: Update `nodeColumns` and both scan functions**

In `internal/store/nodes.go`, the column list and scan order must match exactly.
Replace:

```go
const nodeColumns = `id, name, isp, city, country, agent_version, last_heartbeat_at, secret_hash, tags, metadata, created_at`
```

with:

```go
const nodeColumns = `id, name, isp, city, country, agent_version, platform, last_heartbeat_at, secret_hash, blocked, tags, metadata, created_at`
```

Update `scanNode`:

```go
func scanNode(row interface {
	Scan(dest ...any) error
}) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.Name, &n.ISP, &n.City, &n.Country, &n.AgentVersion, &n.Platform, &n.LastHeartbeatAt, &n.SecretHash, &n.Blocked, &n.Tags, &n.Metadata, &n.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	return n, err
}
```

Update `scanNodes` (same field order, inside the `for rows.Next()` loop):

```go
func scanNodes(rows *sql.Rows) ([]Node, error) {
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.ISP, &n.City, &n.Country, &n.AgentVersion, &n.Platform, &n.LastHeartbeatAt, &n.SecretHash, &n.Blocked, &n.Tags, &n.Metadata, &n.CreatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
```

`CreateNode`/`GetNode`/`ListNodes`/etc. don't need changes beyond this - they all build
on `nodeColumns` + `scanNode`/`scanNodes` already.

- [ ] **Step 3: Add `SetNodePlatform` and `SetNodeBlocked`**

In `internal/store/nodes.go`, right after the existing `SetNodeAgentVersion`:

```go
func (s *Store) SetNodePlatform(ctx context.Context, id uuid.UUID, platform string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE nodes SET platform = $2 WHERE id = $1`, id, platform)
	return err
}

func (s *Store) SetNodeBlocked(ctx context.Context, id uuid.UUID, blocked bool) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE nodes SET blocked = $2 WHERE id = $1`, id, blocked)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: Build**

```sh
go build ./... && go vet ./...
```

Expected: no errors (there's no automated test for this layer yet in the codebase -
`internal/store` has zero existing test files, since `*store.Store` wraps `*sql.DB`
directly with no interface seam for a fake. Verification happens end-to-end in Task 5).

- [ ] **Step 5: Commit**

```sh
git add internal/store/models.go internal/store/nodes.go
git commit -m "Add Node.Blocked/Platform and their store setters"
```

---

### Task 3: Dispatch — skip blocked nodes

**Files:**
- Modify: `internal/dispatch/dispatch.go`

- [ ] **Step 1: Exclude blocked nodes from `node_ids` selection, with a warning**

In `internal/dispatch/dispatch.go`, inside `Resolve`'s `case len(sel.NodeIDs) > 0:`
branch, the loop currently reads:

```go
		for _, id := range sel.NodeIDs {
			n, ok := found[id]
			if !ok {
				warnings = append(warnings, fmt.Sprintf("node %s not found", id))
				continue
			}
			ids = append(ids, id)
			if !n.Online(onlineThreshold) {
				warnings = append(warnings, fmt.Sprintf("node %s (%s) is currently offline, run will wait for it to reconnect", n.ID, n.Name))
			}
		}
```

Change to:

```go
		for _, id := range sel.NodeIDs {
			n, ok := found[id]
			if !ok {
				warnings = append(warnings, fmt.Sprintf("node %s not found", id))
				continue
			}
			if n.Blocked {
				warnings = append(warnings, fmt.Sprintf("node %s (%s) is blocked, skipped", n.ID, n.Name))
				continue
			}
			ids = append(ids, id)
			if !n.Online(onlineThreshold) {
				warnings = append(warnings, fmt.Sprintf("node %s (%s) is currently offline, run will wait for it to reconnect", n.ID, n.Name))
			}
		}
```

- [ ] **Step 2: Exclude blocked nodes from `all`/`tags` selection, silently**

Rename `filterOnline` to `filterAvailable` (it now checks more than online-ness) and add
the blocked check. Replace:

```go
func filterOnline(nodes []store.Node, includeOffline bool, threshold time.Duration) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		if includeOffline || n.Online(threshold) {
			ids = append(ids, n.ID)
		}
	}
	return ids
}
```

with:

```go
// filterAvailable returns the IDs of nodes eligible for new dispatch: never
// blocked, and either online or includeOffline is set. Blocked nodes are
// excluded unconditionally here - unlike node_ids (explicit choice, gets a
// warning), all/tags selection is "what's available right now", so a
// blocked node just doesn't show up, same as it not existing.
func filterAvailable(nodes []store.Node, includeOffline bool, threshold time.Duration) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		if n.Blocked {
			continue
		}
		if includeOffline || n.Online(threshold) {
			ids = append(ids, n.ID)
		}
	}
	return ids
}
```

Update both call sites (`case sel.All:` and `case len(sel.Tags) > 0:`) from
`filterOnline(...)` to `filterAvailable(...)`.

- [ ] **Step 3: Build**

```sh
go build ./... && go vet ./...
```

- [ ] **Step 4: Commit**

```sh
git add internal/dispatch/dispatch.go
git commit -m "Exclude blocked nodes from check dispatch"
```

---

### Task 4: Agent reports its platform on every poll

**Files:**
- Modify: `internal/transport/http.go`
- Modify: `internal/api/agent/handler.go`

- [ ] **Step 1: Agent sends `platform` in the poll body**

In `internal/transport/http.go`, add `"runtime"` to the imports, then change `Poll`:

```go
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
```

- [ ] **Step 2: Backend persists it**

In `internal/api/agent/handler.go`, add `Platform` to `pollRequest`:

```go
type pollRequest struct {
	AgentVersion string `json:"agent_version,omitempty"`
	Platform     string `json:"platform,omitempty"`
}
```

In `Poll`, right after the existing `agent_version` handling:

```go
	if req.AgentVersion != "" {
		_ = h.Store.SetNodeAgentVersion(r.Context(), nodeID, req.AgentVersion)
	}
	if req.Platform != "" {
		_ = h.Store.SetNodePlatform(r.Context(), nodeID, req.Platform)
	}
```

- [ ] **Step 3: Build**

```sh
go build ./... && go vet ./...
```

- [ ] **Step 4: Commit**

```sh
git add internal/transport/http.go internal/api/agent/handler.go
git commit -m "Agent reports its OS platform on every poll"
```

---

### Task 5: Public API — expose blocked/platform, add `PUT /nodes/{id}`

**Files:**
- Modify: `internal/api/public/nodes.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add fields to `nodeResponse`**

In `internal/api/public/nodes.go`:

```go
type nodeResponse struct {
	ID           uuid.UUID       `json:"id"`
	Name         string          `json:"name"`
	ISP          string          `json:"isp"`
	City         string          `json:"city"`
	Country      string          `json:"country"`
	AgentVersion string          `json:"agent_version"`
	Platform     string          `json:"platform,omitempty"`
	Online       bool            `json:"online"`
	Blocked      bool            `json:"blocked"`
	LastSeenAt   *time.Time      `json:"last_seen_at,omitempty"`
	Tags         json.RawMessage `json:"tags"`
	CreatedAt    time.Time       `json:"created_at"`
}

func toNodeResponse(n store.Node, threshold time.Duration) nodeResponse {
	return nodeResponse{
		ID: n.ID, Name: n.Name, ISP: n.ISP, City: n.City, Country: n.Country,
		AgentVersion: n.AgentVersion, Platform: n.Platform, Online: n.Online(threshold), Blocked: n.Blocked,
		LastSeenAt: n.LastHeartbeatAt, Tags: n.Tags, CreatedAt: n.CreatedAt,
	}
}
```

- [ ] **Step 2: Add the `UpdateNode` handler**

Append to `internal/api/public/nodes.go`:

```go
type updateNodeRequest struct {
	Blocked *bool `json:"blocked,omitempty"`
}

// UpdateNode handles PUT /nodes/{id} (admin-only). Currently only supports
// toggling `blocked`: excludes the node from new dispatch (see
// internal/dispatch) without deleting it, so its check history is kept.
func (h *Handler) UpdateNode(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req updateNodeRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Blocked == nil {
		api.WriteError(w, http.StatusBadRequest, "blocked is required")
		return
	}
	if err := h.Store.SetNodeBlocked(r.Context(), id, *req.Blocked); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.WriteError(w, http.StatusNotFound, "node not found")
			return
		}
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := h.Store.GetNode(r.Context(), id)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, toNodeResponse(n, h.OnlineThreshold))
}
```

- [ ] **Step 3: Register the route**

In `cmd/server/main.go`, in the `RequireAdminToken` group (same tier as `CreateNode`):

```go
			r.Post("/nodes", publicH.CreateNode)
			r.Put("/nodes/{id}", publicH.UpdateNode)
```

- [ ] **Step 4: Build**

```sh
go build ./... && go vet ./...
```

- [ ] **Step 5: End-to-end manual verification (covers Tasks 1-5 together)**

With the server running (Task 1, Step 2) and `ADMIN_TOKEN=dev-admin-token`:

```sh
# Create a node, confirm blocked=false and platform="" by default
NODE=$(curl -sS -X POST http://localhost:8080/api/v1/nodes \
  -H "Authorization: Bearer dev-admin-token" -H "Content-Type: application/json" \
  -d '{"name":"plan-test-node","isp":"test","city":"test"}')
echo "$NODE"
# Expect: "blocked":false, "platform":"" somewhere in the JSON (platform omitted if
# empty, since it's `omitempty` - that's fine, it means the same thing)

NODE_ID=$(echo "$NODE" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

# Block it
curl -sS -X PUT "http://localhost:8080/api/v1/nodes/$NODE_ID" \
  -H "Authorization: Bearer dev-admin-token" -H "Content-Type: application/json" \
  -d '{"blocked":true}'
# Expect: 200, "blocked":true in the response

# Create an account + api_key to test dispatch through the public API
ACCOUNT_ID=$(curl -sS -X POST http://localhost:8080/api/v1/accounts \
  -H "Authorization: Bearer dev-admin-token" -H "Content-Type: application/json" \
  -d '{"name":"plan-test-account"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
API_KEY=$(curl -sS -X POST "http://localhost:8080/api/v1/accounts/$ACCOUNT_ID/api-keys" \
  -H "Authorization: Bearer dev-admin-token" -H "Content-Type: application/json" \
  -d '{"label":"plan-test"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

# Explicit node_ids selection of the blocked node -> must warn and create no runs
curl -sS -X POST http://localhost:8080/api/v1/checks \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d "{\"type\":\"ping\",\"target\":\"1.1.1.1\",\"node_selector\":{\"node_ids\":[\"$NODE_ID\"]}}"
# Expect: 422 "node_selector matched no available nodes" (the only requested node is
# blocked, so nothing eligible remains)

# {"all":true} selection -> the blocked node must not appear even implicitly
curl -sS -X POST http://localhost:8080/api/v1/checks \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"type":"ping","target":"1.1.1.1","node_selector":{"all":true,"include_offline":true}}'
# Expect: either 422 (if this is the only node) or, if you have other real nodes from
# earlier testing, a normal 201 whose runs never include $NODE_ID

# Unblock and clean up
curl -sS -X PUT "http://localhost:8080/api/v1/nodes/$NODE_ID" \
  -H "Authorization: Bearer dev-admin-token" -H "Content-Type: application/json" \
  -d '{"blocked":false}'
```

- [ ] **Step 6: Commit**

```sh
git add internal/api/public/nodes.go cmd/server/main.go
git commit -m "Expose nodes.blocked/platform and add PUT /nodes/{id}"
```

---

### Task 6: `POST /api/v1/server-ping` — handler and unit tests

**Files:**
- Create: `internal/api/public/serverping.go`
- Create: `internal/api/public/serverping_test.go`

This handler deliberately does not touch `h.Store` — it reuses `internal/checks`
directly (the same `PingChecker`/`TCPChecker` the node agent runs), with a zero-value
`checks.NetConfig{}` (system default route, unbound - exactly what "ping from the
server itself" means). No DB access means it's fully unit-testable with `httptest` and
no Postgres connection, unlike Tasks 1-5.

- [ ] **Step 1: Write the failing tests**

`internal/api/public/serverping_test.go` — uses only TCP checks against local
listeners (never invokes the real OS `ping` binary, so it's fast and has no
OS/privilege dependency; see Task 7 for a manual ICMP check):

```go
package public

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
// safe to call from a non-main test goroutine (Task 7's concurrency test
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
```

- [ ] **Step 2: Run to verify it fails**

```sh
go test ./internal/api/public/... -run TestServerPing -v
```

Expected: build failure - `serverPingResponse`, `serverPingMaxTargets`, `Handler.ServerPing`
etc. don't exist yet.

- [ ] **Step 3: Implement**

`internal/api/public/serverping.go`:

```go
// ServerPing (POST /api/v1/server-ping) runs ICMP/TCP checks directly from
// this backend - no node involved, synchronous. See
// docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md.
package public

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"pingachock/internal/api"
	"pingachock/internal/checks"
)

const (
	serverPingMaxTargets = 50
	serverPingTimeout    = 20 * time.Second
)

type serverPingRequest struct {
	Targets []string `json:"targets"`
	Ports   []string `json:"ports"`
}

type serverPingICMPResult struct {
	Success   bool   `json:"success"`
	LatencyMs *int   `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type serverPingTargetResult struct {
	Target     string                 `json:"target"`
	ResolvedIP string                 `json:"resolved_ip,omitempty"`
	ICMP       *serverPingICMPResult  `json:"icmp,omitempty"`
	Ports      map[string]string      `json:"ports,omitempty"`
}

type serverPingResponse struct {
	Results []serverPingTargetResult `json:"results"`
}

// ServerPing does not touch h.Store: it never creates checks/check_runs, so
// every request is fully self-contained with zero state shared across
// concurrent requests. This is deliberate - see the spec's "Требование
// корректности" section, written after a real incident in a prior system
// where one user's ping response leaked into another's.
func (h *Handler) ServerPing(w http.ResponseWriter, r *http.Request) {
	var req serverPingRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Targets) == 0 {
		api.WriteError(w, http.StatusBadRequest, "targets must not be empty")
		return
	}
	if len(req.Targets) > serverPingMaxTargets {
		api.WriteError(w, http.StatusBadRequest, fmt.Sprintf("targets: max %d per request", serverPingMaxTargets))
		return
	}

	ports := req.Ports
	if len(ports) == 0 {
		ports = []string{"icmp"}
	}
	for _, p := range ports {
		if p == "icmp" {
			continue
		}
		if _, err := strconv.Atoi(p); err != nil {
			api.WriteError(w, http.StatusBadRequest, "ports: each entry must be \"icmp\" or a numeric port, got "+p)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), serverPingTimeout)
	defer cancel()

	results := make([]serverPingTargetResult, len(req.Targets))
	var wg sync.WaitGroup
	for i, target := range req.Targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			results[i] = runServerPingTarget(ctx, target, ports)
		}(i, target)
	}
	wg.Wait()

	api.WriteJSON(w, http.StatusOK, serverPingResponse{Results: results})
}

// runServerPingTarget runs every requested port check (plus ICMP, if asked
// for) against one target concurrently, and assembles them into one result.
// mu guards the shared `out` value - the WaitGroup in ServerPing already
// keeps different *targets* from touching each other's result slot, this
// mutex is only about the ports *within* a single target overlapping.
func runServerPingTarget(ctx context.Context, target string, ports []string) serverPingTargetResult {
	out := serverPingTargetResult{Target: target}
	var netCfg checks.NetConfig
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, p := range ports {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()

			if p == "icmp" {
				checker, _ := checks.Get("ping")
				res := checker.Run(ctx, netCfg, target, json.RawMessage(`{"count":1,"timeout_ms":3000}`))
				icmp := &serverPingICMPResult{Success: res.Success, LatencyMs: res.LatencyMs}
				if !res.Success && res.ErrorMessage != nil {
					icmp.Error = *res.ErrorMessage
				}
				mu.Lock()
				out.ICMP = icmp
				if out.ResolvedIP == "" {
					out.ResolvedIP = resolvedIPFromRaw(res.Raw)
				}
				mu.Unlock()
				return
			}

			checker, _ := checks.Get("tcp")
			// tcpParams.Port is an int, not a string - must convert before
			// marshaling. Getting this wrong (marshaling the raw string p
			// straight into {"port": p}) doesn't fail loudly: tcpParams'
			// own json.Unmarshal error is ignored by internal/checks/tcp.go,
			// so a type mismatch there silently leaves Port at its zero
			// value, which then defaults to 443 - every check would run
			// against port 443 regardless of what was actually requested.
			portNum, err := strconv.Atoi(p)
			if err != nil {
				return
			}
			params, _ := json.Marshal(map[string]any{"port": portNum})
			res := checker.Run(ctx, netCfg, target, params)
			status := "closed"
			if res.Success {
				status = "open"
			}
			mu.Lock()
			if out.Ports == nil {
				out.Ports = make(map[string]string)
			}
			out.Ports[p] = status
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

func resolvedIPFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		ResolvedTarget string `json:"resolved_target"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.ResolvedTarget
}
```

- [ ] **Step 4: Run to verify it passes**

```sh
go test ./internal/api/public/... -run TestServerPing -v
```

Expected: all `TestServerPing*` tests `PASS`.

- [ ] **Step 5: Commit**

```sh
git add internal/api/public/serverping.go internal/api/public/serverping_test.go
git commit -m "Add POST /api/v1/server-ping handler"
```

---

### Task 7: Concurrency safety test, route registration, docs, live smoke test

**Files:**
- Modify: `internal/api/public/serverping_test.go`
- Modify: `cmd/server/main.go`
- Modify: `internal/api/openapi.yaml`

- [ ] **Step 1: Write the failing concurrency test**

This is the test that encodes the spec's explicit correctness requirement (past
incident: one user's ping response leaked into another's). Append to
`internal/api/public/serverping_test.go`:

```go
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
```

Add `"fmt"` and `"sync"` to the test file's imports.

- [ ] **Step 2: Run to verify it fails or passes**

```sh
go test ./internal/api/public/... -run TestServerPingConcurrentRequestsDoNotCrossTalk -v -race -count=5
```

`-race` requires a working C toolchain (`CGO_ENABLED=1` plus `gcc`/`clang` on `PATH`);
if that's unavailable in your environment, it fails with `cgo: C compiler "gcc" not
found` before running anything - in that case drop `-race` and rely on `-count=5`
(or higher) alone to catch flakiness instead.

Given the design (no shared state between targets, indexed slice writes, mutex-guarded
per-target map), this is expected to **PASS** immediately, repeated runs included. If it
fails, or `-race` reports a data race, stop and fix `ServerPing`/`runServerPingTarget`
before continuing - this test is the whole point of the task, do not weaken it to make
it pass.

- [ ] **Step 3: Register the route**

In `cmd/server/main.go`, in the existing `RequireAPIKey` group (same tier as
`checks`/`nodes` GET):

```go
			r.Post("/checks", publicH.CreateCheck)
			r.Get("/checks", publicH.ListChecks)
			r.Get("/checks/{id}", publicH.GetCheck)
			r.Delete("/checks/{id}", publicH.CancelCheck)
			r.Get("/nodes", publicH.ListNodes)
			r.Get("/nodes/{id}", publicH.GetNode)
			r.Post("/server-ping", publicH.ServerPing)
```

- [ ] **Step 4: Document it in the OpenAPI spec**

In `internal/api/openapi.yaml`, add a new path (matching the existing style - see
`/api/v1/checks` for the pattern):

```yaml
  /api/v1/server-ping:
    post:
      summary: Пинг напрямую с бекенда (без узла)
      description: >
        ICMP и/или TCP-порты выполняются прямо на сервере, синхронно - результат
        приходит в этом же ответе, без диспетчеризации на узел. Не создаёт checks
        /check_runs.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [targets]
              properties:
                targets:
                  type: array
                  items: { type: string }
                  maxItems: 50
                ports:
                  type: array
                  description: '"icmp" и/или номера портов строками. По умолчанию ["icmp"].'
                  items: { type: string }
      responses:
        '200':
          description: Результаты по каждой цели
          content:
            application/json:
              schema:
                type: object
                properties:
                  results:
                    type: array
                    items:
                      type: object
                      properties:
                        target: { type: string }
                        resolved_ip: { type: string }
                        icmp:
                          type: object
                          properties:
                            success: { type: boolean }
                            latency_ms: { type: integer }
                            error: { type: string }
                        ports:
                          type: object
                          additionalProperties: { type: string }
        '400':
          $ref: '#/components/responses/Error'
```

- [ ] **Step 5: Build and run the full suite**

```sh
go build ./... && go vet ./... && go test ./...
```

Expected: `BUILD OK`, `VET OK`, all packages `ok`.

- [ ] **Step 6: Live smoke test against the real dev stack**

With the local server running (Task 1, Step 2) and a real `API_KEY` from Task 5's
Step 5 (or mint a fresh one the same way):

```sh
curl -sS -X POST http://localhost:8080/api/v1/server-ping \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"targets":["1.1.1.1","8.8.8.8"],"ports":["icmp","443"]}'
```

Expected: `200`, a `results` array with two entries, each with `icmp.success` and
`ports."443"` (`"open"` for both `1.1.1.1` and `8.8.8.8` on port 443, which both serve
HTTPS). This is the one manual check exercising the real ICMP path end-to-end (the
automated tests deliberately avoid shelling out to the OS `ping` binary - see Task 6).

Also confirm the guardrails:

```sh
curl -sS -X POST http://localhost:8080/api/v1/server-ping \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"targets":[]}'
# Expect 400 "targets must not be empty"

curl -sS -X POST http://localhost:8080/api/v1/server-ping \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d '{"targets":["1.1.1.1"],"ports":["not-a-port"]}'
# Expect 400 mentioning the bad port value
```

- [ ] **Step 7: Commit**

```sh
git add internal/api/public/serverping_test.go cmd/server/main.go internal/api/openapi.yaml
git commit -m "Add server-ping concurrency test, route, and OpenAPI docs"
```

---

## After this plan

Backend Part 1 is complete and independently shippable/testable. The Telegram bot
integration (spec sections B-D: `bot/` directory, `api-client.ts` rewrite, orchestration
layer, `docker-compose.prod.yml` service) is a separate follow-up plan, written once
this one is merged and deployed - it depends on `server-ping`'s and the node
`blocked`/`platform` fields' final, live behavior to build and test against.
