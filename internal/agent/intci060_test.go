package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestSpawnUIDCandidateRetry_SixRefusalsThenSucceeds(t *testing.T) {
	t.Setenv("BUNKER_UID_COLLISION_MAX_RETRIES", "8")
	h := newUIDWalkHarness(t, "1029", func(uid uint32) ([]userProcess, error) {
		if uid < 1035 {
			return []userProcess{collisionProcess}, nil
		}
		return nil, nil
	})
	if err := h.spawn(t); err == nil || !strings.Contains(err.Error(), "failed at stage "+StageKeygen) {
		t.Fatalf("expected clean seventh candidate to reach keygen; got %v", err)
	}
	if len(h.scanned) != 7 || h.scanned[6] != 1035 {
		t.Fatalf("scanned candidates = %v; want 1029 through 1035", h.scanned)
	}
}

func TestSpawnUIDCandidateRetry_EnvOverrideAndMalformedFallback(t *testing.T) {
	t.Setenv("BUNKER_UID_COLLISION_MAX_RETRIES", "2")
	if got := uidCollisionCandidateRetryLimit(); got != 2 {
		t.Fatalf("override limit = %d; want 2", got)
	}
	for _, value := range []string{"bad", "-1"} {
		t.Setenv("BUNKER_UID_COLLISION_MAX_RETRIES", value)
		if got := uidCollisionCandidateRetryLimit(); got != uidCollisionCandidateRetriesDefault {
			t.Errorf("value %q limit = %d; want default %d", value, got, uidCollisionCandidateRetriesDefault)
		}
	}
	t.Setenv("BUNKER_UID_COLLISION_MAX_RETRIES", "")
	if got := uidCollisionCandidateRetryLimit(); got != uidCollisionCandidateRetriesDefault {
		t.Fatalf("unset limit = %d; want default %d", got, uidCollisionCandidateRetriesDefault)
	}
}

func TestStatusFileOwnedByUID_OnlyRealAndSavedSetUIDs(t *testing.T) {
	status := []byte("Name:\ttest\nUid:\t1001\t2002\t3003\t4004\n")
	for _, tc := range []struct {
		uid  uint32
		want bool
	}{{1001, true}, {2002, false}, {3003, true}, {4004, false}, {5005, false}} {
		if got := statusFileOwnedByUID(status, tc.uid); got != tc.want {
			t.Errorf("uid %d ownership = %v; want %v", tc.uid, got, tc.want)
		}
	}
	if got := statusOwnerUIDs(status); fmt.Sprint(got) != "[1001 3003]" {
		t.Errorf("batch owner uids = %v; want [1001 3003]", got)
	}
}
