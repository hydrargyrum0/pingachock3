package dispatch

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"pingachock/internal/store"
)

func idsOf(nodes ...store.Node) []uuid.UUID {
	ids := make([]uuid.UUID, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}

func TestFilterAvailableExcludesVirtualNode(t *testing.T) {
	now := time.Now()
	real := store.Node{ID: uuid.New(), LastHeartbeatAt: &now}
	virtual := store.Node{ID: uuid.New(), IsVirtual: true, LastHeartbeatAt: &now}

	got := filterAvailable([]store.Node{real, virtual}, false, time.Minute)

	want := idsOf(real)
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("filterAvailable() = %v, want only the real node %v (virtual node must never be picked up by all/tags selection)", got, want)
	}
}

func TestFilterAvailableExcludesBlocked(t *testing.T) {
	now := time.Now()
	ok := store.Node{ID: uuid.New(), LastHeartbeatAt: &now}
	blocked := store.Node{ID: uuid.New(), Blocked: true, LastHeartbeatAt: &now}

	got := filterAvailable([]store.Node{ok, blocked}, false, time.Minute)

	if len(got) != 1 || got[0] != ok.ID {
		t.Errorf("filterAvailable() = %v, want only the non-blocked node %v", got, ok.ID)
	}
}

func TestFilterAvailableOfflineExcludedUnlessIncludeOffline(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	offline := store.Node{ID: uuid.New(), LastHeartbeatAt: &stale}

	if got := filterAvailable([]store.Node{offline}, false, time.Minute); len(got) != 0 {
		t.Errorf("filterAvailable(includeOffline=false) = %v, want empty - node is offline", got)
	}
	if got := filterAvailable([]store.Node{offline}, true, time.Minute); len(got) != 1 {
		t.Errorf("filterAvailable(includeOffline=true) = %v, want the offline node included", got)
	}
}
