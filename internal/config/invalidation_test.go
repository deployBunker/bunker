package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// writeInvalidationConfig writes a config file with one server.invalidation block
// and returns its path.
func writeInvalidationConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	full := "server:\n  grpc_addr: \":9999\"\n" + body
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoad_InvalidationAbsentTakesDeclaredDefaults: no block is a fact about the
// file, and every knob keeps the value the table declares.
func TestLoad_InvalidationAbsentTakesDeclaredDefaults(t *testing.T) {
	cfg, err := Load(writeInvalidationConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Invalidation != nil {
		t.Fatalf("an absent block must stay nil, got %+v", cfg.Server.Invalidation)
	}
	v, err := cfg.InvalidationValues()
	if err != nil {
		t.Fatalf("InvalidationValues: %v", err)
	}
	if v != invalidation.DefaultValues() {
		t.Fatalf("a deployment with no invalidation block resolved to %+v, want the declared defaults", v)
	}
}

// TestLoad_InvalidationBlockIsReadAndObeyed: a written key reaches the resolved
// surface, and the keys that were NOT written keep the declared defaults.
func TestLoad_InvalidationBlockIsReadAndObeyed(t *testing.T) {
	cfg, err := Load(writeInvalidationConfig(t, `  invalidation:
    watch:
      enabled: true
      heartbeat_ms: 5000
      install_headroom: 64
    push:
      write_deadline_ms: 1000
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v, err := cfg.InvalidationValues()
	if err != nil {
		t.Fatalf("InvalidationValues: %v", err)
	}
	if !v.Watch.Enabled {
		t.Fatal("watch.enabled: true did not reach the resolved surface")
	}
	if v.Watch.HeartbeatMS != 5000 || v.Watch.InstallHeadroom != 64 {
		t.Fatalf("write keys not obeyed: %+v", v.Watch)
	}
	if v.Push.WriteDeadlineMS != 1000 {
		t.Fatalf("push.write_deadline_ms = %d, want 1000", v.Push.WriteDeadlineMS)
	}
	if v.Watch.ScanLimit != invalidation.DefaultWatchScanLimit {
		t.Fatalf("an unwritten key resolved to %d, want the declared default %d", v.Watch.ScanLimit, invalidation.DefaultWatchScanLimit)
	}
}

// TestLoad_InvalidationInvalidValueFailsBeforeListen is the row's loud-failure law
// at the layer an operator actually hits: a typo'd limit stops the boot with the
// knob named, and is NEVER replaced by the default.
func TestLoad_InvalidationInvalidValueFailsBeforeListen(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		knob    string
		notWant string // a value that must NOT appear as the resolved value
	}{
		{
			name: "a zeroed headroom",
			body: "  invalidation:\n    watch:\n      install_headroom: 0\n",
			knob: invalidation.KnobWatchHeadroom,
		},
		{
			name: "a heartbeat above the declared bound",
			body: "  invalidation:\n    watch:\n      heartbeat_ms: 60000\n",
			knob: invalidation.KnobWatchHeartbeatMS,
		},
		{
			name: "a buffer below the floor",
			body: "  invalidation:\n    push:\n      subscriber_buffer_bytes: 4096\n",
			knob: invalidation.KnobPushBufferBytes,
		},
		{
			name: "a write deadline at the heartbeat",
			body: "  invalidation:\n    push:\n      write_deadline_ms: 30000\n",
			knob: invalidation.KnobPushWriteDeadlineMS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeInvalidationConfig(t, tc.body))
			if err != nil {
				// A decode-level refusal is loud too, but this table is about
				// values that decode and then must be refused by the table.
				t.Fatalf("Load refused the file outright: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted: a written value outside its declared range must stop the boot, not fall back to the default", tc.knob)
			}
			if !strings.Contains(err.Error(), tc.knob) {
				t.Fatalf("the refusal must name the knob %s: %v", tc.knob, err)
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Fatalf("the refusal must say the value is refused rather than replaced: %v", err)
			}
		})
	}
}

// TestLoad_InvalidationUnknownKeyIsAcceptedGap pins a HOLE this row found, measured
// rather than assumed, and did not close: config keys are not strict, so a typo'd
// knob NAME is silently ignored while a typo'd VALUE is refused loudly. The row's
// law is about values ("a server that quietly ignores a typo'd limit is how a bound
// stops being a bound") and the value case IS closed; the key case belongs to the
// loader's strictness, which is wider than this surface (every section of the file
// has the same behaviour) and is reported as residual R-2 in
// docs/evidence/BFS-043-*.md rather than fixed here.
//
// Both directions of a change are caught: if the loader grows strict key handling
// this cell fails and must be inverted (the residual then closes); if an unknown key
// ever starts producing a value, that is a new defect and this cell says so.
func TestLoad_InvalidationUnknownKeyIsAcceptedGap(t *testing.T) {
	cfg, err := Load(writeInvalidationConfig(t, "  invalidation:\n    watch:\n      heartbeet_ms: 5000\n"))
	if err != nil {
		t.Fatalf("the loader REFUSED an unknown key, which closes residual R-2: invert this cell and update the evidence doc (err=%v)", err)
	}
	v, err := cfg.InvalidationValues()
	if err != nil {
		t.Fatalf("InvalidationValues: %v", err)
	}
	if v.Watch.HeartbeatMS != invalidation.DefaultWatchHeartbeatMS {
		t.Fatalf("heartbeat_ms = %d: the typo'd key produced a value, which contradicts the gap this cell pins", v.Watch.HeartbeatMS)
	}
}

// TestLoad_InvalidationNonBooleanFailsAtDecode: `enabled:` is a bool or a load
// error. A string that a lenient decoder read as false would be a knob that
// silently does not do what it says.
func TestLoad_InvalidationNonBooleanFailsAtDecode(t *testing.T) {
	_, err := Load(writeInvalidationConfig(t, "  invalidation:\n    watch:\n      enabled: \"yes\"\n"))
	if err == nil {
		t.Fatal("enabled: \"yes\" was accepted: a non-boolean must be refused at load, never read as false")
	}
	if !strings.Contains(err.Error(), "enabled") && !strings.Contains(err.Error(), "Server.Invalidation") {
		t.Fatalf("the decode refusal must name the field it could not read: %v", err)
	}
}
