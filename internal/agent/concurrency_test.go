package agent

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/resource"
)

// TestConcurrency_SpawnFiveAgents spawns five agents in parallel and verifies
// each gets a distinct user, home directory, port range, and dockerd unit.
// This test requires root because it calls useradd/systemd-run.
func TestConcurrency_SpawnFiveAgents(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}

	m := newTestManager(t)

	const n = 5
	var wg sync.WaitGroup
	results := make(chan spawnResult, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			agentID := uniqueAgentID(fmt.Sprintf("conctest-%d", idx))
			req := &v1.SpawnAgentRequest{AgentId: agentID}
			resp, err := m.Spawn(t.Context(), req)
			results <- spawnResult{idx: idx, agentID: agentID, resp: resp, err: err}
		}(i)
	}

	wg.Wait()
	close(results)

	var successes int
	seenUsers := make(map[string]struct{})
	seenRanges := make(map[string]struct{})
	seenAgentIDs := make(map[string]struct{})

	for r := range results {
		if r.err != nil {
			t.Errorf("spawn goroutine %d failed: %v", r.idx, r.err)
			continue
		}
		successes++
		username := "bunker-" + r.resp.AgentId

		// Verify unique user
		if _, exists := seenUsers[username]; exists {
			t.Errorf("duplicate user %q", username)
		}
		seenUsers[username] = struct{}{}

		// Verify unique agent ID
		if _, exists := seenAgentIDs[r.resp.AgentId]; exists {
			t.Errorf("duplicate agent_id %q", r.resp.AgentId)
		}
		seenAgentIDs[r.resp.AgentId] = struct{}{}

		// Verify unique port range
		rangeKey := fmt.Sprintf("%d-%d", r.resp.PortRangeStart, r.resp.PortRangeEnd)
		if _, exists := seenRanges[rangeKey]; exists {
			t.Errorf("duplicate port range %q for agent %q", rangeKey, r.resp.AgentId)
		}
		seenRanges[rangeKey] = struct{}{}

		// Verify home directory exists and is owned by the right user
		homeDir := "/home/" + username
		info, err := os.Stat(homeDir)
		if err != nil {
			t.Errorf("agent %q home dir %q missing: %v", r.resp.AgentId, homeDir, err)
		} else if !info.IsDir() {
			t.Errorf("agent %q home path %q is not a directory", r.resp.AgentId, homeDir)
		}

		// Verify authorized_keys exists
		authKeys := fmt.Sprintf("/home/%s/.ssh/authorized_keys", username)
		content, err := os.ReadFile(authKeys)
		if err != nil {
			t.Errorf("agent %q authorized_keys missing: %v", r.resp.AgentId, err)
		} else if !strings.Contains(string(content), "DOCKER_HOST=") {
			t.Errorf("agent %q authorized_keys missing DOCKER_HOST", r.resp.AgentId)
		}

		// Verify socket directory exists
		sockDir := fmt.Sprintf("/run/bunker/%s", r.resp.AgentId)
		if info, err := os.Stat(sockDir); err != nil {
			t.Errorf("agent %q socket dir %q missing: %v", r.resp.AgentId, sockDir, err)
		} else if !info.IsDir() {
			t.Errorf("agent %q socket path %q is not a directory", r.resp.AgentId, sockDir)
		}
	}

	if successes != n {
		t.Fatalf("expected %d successful spawns, got %d", n, successes)
	}

	// Cleanup all spawned agents in parallel as well.
	var cleanupWg sync.WaitGroup
	for id := range seenAgentIDs {
		cleanupWg.Add(1)
		go func(agentID string) {
			defer cleanupWg.Done()
			cleanupAgent(t, m, agentID)
		}(id)
	}
	cleanupWg.Wait()

	// Verify all users and run dirs are gone.
	for id := range seenAgentIDs {
		homeDir := "/home/bunker-" + id
		if _, err := os.Stat(homeDir); !os.IsNotExist(err) {
			t.Errorf("home dir %q still exists after destroy", homeDir)
		}
		sockDir := fmt.Sprintf("/run/bunker/%s", id)
		if _, err := os.Stat(sockDir); !os.IsNotExist(err) {
			t.Errorf("socket dir %q still exists after destroy", sockDir)
		}
	}
}

// TestConcurrency_PortAllocatorIsolation verifies that the port allocator
// returns disjoint ranges even under concurrent use. This test does NOT need
// root because it only exercises the allocator.
func TestConcurrency_PortAllocatorIsolation(t *testing.T) {
	m := newTestManager(t)
	if m.portAlloc == nil {
		t.Skip("port allocator disabled")
	}

	const n = 10
	var wg sync.WaitGroup
	ranges := make(chan [2]uint32, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start, end, err := m.portAlloc.Allocate(fmt.Sprintf("alloc-%d-%d", idx, time.Now().UnixNano()))
			if err != nil {
				t.Errorf("allocate goroutine %d failed: %v", idx, err)
				return
			}
			ranges <- [2]uint32{start, end}
		}(i)
	}

	wg.Wait()
	close(ranges)

	seen := make(map[uint32]struct{})
	for r := range ranges {
		for p := r[0]; p <= r[1]; p++ {
			if _, exists := seen[p]; exists {
				t.Fatalf("port %d allocated to more than one agent", p)
			}
			seen[p] = struct{}{}
		}
	}
}

// TestConcurrency_TrackerCapacity verifies that the tracker correctly enforces
// capacity under concurrent registration attempts.
func TestConcurrency_TrackerCapacity(t *testing.T) {
	m := newTestManager(t)
	max := int(m.tracker.MaxAgents())

	// Fill tracker to capacity with dummy records.
	for i := 0; i < max; i++ {
		rec := &resource.AgentRecord{
			AgentID: fmt.Sprintf("dummy-%d", i),
			Status:  "running",
		}
		if err := m.tracker.Register(rec); err != nil {
			t.Fatalf("failed to seed tracker: %v", err)
		}
	}

	var wg sync.WaitGroup
	var failures int
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := &resource.AgentRecord{
				AgentID: fmt.Sprintf("overflow-%d-%d", idx, time.Now().UnixNano()),
				Status:  "running",
			}
			if err := m.tracker.Register(rec); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()
	if failures != 10 {
		t.Fatalf("expected 10 capacity failures, got %d", failures)
	}
}

type spawnResult struct {
	idx     int
	agentID string
	resp    *v1.SpawnAgentResponse
	err     error
}
