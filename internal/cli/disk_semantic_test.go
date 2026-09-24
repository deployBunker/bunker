// disk_semantic_test.go — DF-BUNKER-54 semantic pin.
//
// agent.default_disk_bytes is applied on the host as systemd LimitFSIZE
// (RLIMIT_FSIZE): a PER-FILE size cap. Nothing in bunker caps an agent's TOTAL
// on-disk usage (per-user filesystem quotas are GAP-161 and unimplemented), so
// before this row the CLI asserted an enforcement that does not exist —
// `bunker list` printed `218% (43.6 GB/20.0 GB)`, comparing measured usage
// against a number no host mechanism enforces.
//
// These tests are the pin the board row asks for: they fail if a later change
// re-presents the number as a total-disk limit, brings the used-vs-cap
// percentage back, or emits a knob that is not the honest per-file one.
package cli

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// p54CapText is the exact label vocabulary a surface must carry so a reader of
// ANY single surface cannot mistake the cap for a total-disk quota.
const (
	p54CapLabel     = "Max File Size"
	p54CapQualifier = "per-file cap"
)

// assertNoDiskLimitClaim fails when a rendered surface claims the agent number
// is a disk limit/quota. Every surface that shows the cap must be checked
// against this: the defect was a labelling defect, so the labels are the
// contract.
func assertNoDiskLimitClaim(t *testing.T, surface, out string) {
	t.Helper()
	for _, forbidden := range []string{
		"Disk Limit",       // the old info/metrics label
		"disk limit:",      //
		"Disk usage limit", //
		"Disk Quota",       // the old proto/server wording
		"disk quota:",      //
		"Total Disk",       // any total-usage framing
		"total-disk limit",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s renders %q, which claims the per-file cap is a total-disk limit; "+
				"the cap is LimitFSIZE/RLIMIT_FSIZE and nothing enforces total usage (DF-BUNKER-54)\noutput:\n%s",
				surface, forbidden, out)
		}
	}
}

// TestListCommand_DiskCapIsLabelledPerFile pins the `bunker list` table: usage
// and the cap are two separately labelled facts, and NO percentage is printed
// for the agent row (the old "218% (43.6 GB/20.0 GB)" cell claimed a quota).
func TestListCommand_DiskCapIsLabelledPerFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{Hostname: "bunker-disk", Version: "v0.2.0"},
		},
		listResp: &v1.ListAgentsResponse{
			Agents: []*v1.AgentSummary{
				{
					AgentId:       "agent-abc",
					Status:        "running",
					DiskUsedBytes: 80 * 1024 * 1024 * 1024, // 80 GB
					CreatedAt:     "2026-07-28T10:00:00Z",
					Limits: &v1.ResourceLimits{
						DiskMaxBytes: 200 * 1024 * 1024 * 1024, // 200 GB
					},
				},
				{
					AgentId:       "agent-nocap",
					Status:        "running",
					DiskUsedBytes: 50 * 1024 * 1024 * 1024, // 50 GB
					CreatedAt:     "2026-07-27T12:00:00Z",
				},
			},
			TotalCount: 2,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	// Usage and cap are separate columns, each named for its semantic.
	if !strings.Contains(output, diskUsedHeader) {
		t.Errorf("list output missing the %q column header, got:\n%s", diskUsedHeader, output)
	}
	if !strings.Contains(output, p54CapLabel) {
		t.Errorf("list output missing the %q column header, got:\n%s", p54CapLabel, output)
	}
	// The cap column carries the qualifier, so the number is never read as a quota.
	if !strings.Contains(output, p54CapQualifier) {
		t.Errorf("list output missing the %q qualifier, got:\n%s", p54CapQualifier, output)
	}
	// The measured usage and the cap both render, as values.
	if !strings.Contains(output, "80.0 GB") {
		t.Errorf("list output missing measured usage 80.0 GB, got:\n%s", output)
	}
	if !strings.Contains(output, "200.0 GB") {
		t.Errorf("list output missing the per-file cap 200.0 GB, got:\n%s", output)
	}
	// DEFECT REGRESSION: the used-vs-cap ratio must not come back. The row
	// prints "80.0 GB" and "200.0 GB"; a percentage would mean real usage is
	// being compared against an unenforced number again.
	for _, forbidden := range []string{"40%", "(80.0 GB/200.0 GB)", "0% (50.0 GB/0 B)"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("list output renders the removed usage-vs-cap ratio %q; the cap is a "+
				"per-file cap, so usage must never be divided by it (DF-BUNKER-54)\noutput:\n%s",
				forbidden, output)
		}
	}
	// No per-agent percentage cell at all in this table.
	if strings.Contains(output, "% (") {
		t.Errorf("list output contains a percentage cell (%%) for the agent cap, got:\n%s", output)
	}
	// An agent with no configured cap says so explicitly rather than showing a
	// 0-byte cap (0 = the knob is not emitted = the documented-good state).
	if !strings.Contains(output, noCapLabel) {
		t.Errorf("list output should mark an unset cap as %q, got:\n%s", noCapLabel, output)
	}
	assertNoDiskLimitClaim(t, "`bunker list`", output)
}

// TestInfoCommand_DiskCapIsLabelledPerFile pins the `bunker info` limit block.
func TestInfoCommand_DiskCapIsLabelledPerFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &infoMockServer{
		agent: &v1.AgentSummary{
			AgentId:   "p54-info",
			Status:    "running",
			CreatedAt: "2026-07-01T12:00:00Z",
			Limits: &v1.ResourceLimits{
				CpuQuota:            2.0,
				MemoryMaxBytes:      2 * 1024 * 1024 * 1024,
				DiskMaxBytes:        20 * 1024 * 1024 * 1024,
				MaxDockerContainers: 10,
			},
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers:      map[string]ServerEntry{"test": {URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"--server", "test", "p54-info"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, p54CapLabel) {
		t.Errorf("info output missing the %q label, got:\n%s", p54CapLabel, output)
	}
	if !strings.Contains(output, p54CapQualifier) {
		t.Errorf("info output missing the %q qualifier, got:\n%s", p54CapQualifier, output)
	}
	if !strings.Contains(output, "20.0 GB") {
		t.Errorf("info output missing the cap value 20.0 GB, got:\n%s", output)
	}
	// MemoryMax IS a real ceiling and keeps its own label — the fix must not
	// have over-applied and made every limit sound unenforced.
	if !strings.Contains(output, "Memory Limit:") {
		t.Errorf("info output lost the real Memory Limit label, got:\n%s", output)
	}
	assertNoDiskLimitClaim(t, "`bunker info`", output)
}

// TestMetricsCommand_DiskCapIsLabelledPerFile pins the per-agent metrics block
// (`bunker metrics <id>`), which renders the wire field disk_limit_bytes.
func TestMetricsCommand_DiskCapIsLabelledPerFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(TLSInsecureAckEnv, "1")

	mock := &metricsMockServer{
		agentMetrics: &v1.AgentMetricsResponse{
			AgentId:          "p54-metrics",
			Status:           "running",
			MemoryUsedBytes:  1024 * 1024 * 256,
			MemoryLimitBytes: 1024 * 1024 * 1024,
			DiskUsedBytes:    1024 * 1024 * 1024 * 2,
			DiskLimitBytes:   1024 * 1024 * 1024 * 10,
			Uptime:           "3h42m",
		},
	}
	ts := newMetricsTestServer(t, mock)
	defer ts.Close()

	if err := RegisterServer("test", ts.URL, "", true); err != nil {
		t.Fatalf("RegisterServer: %v", err)
	}

	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{"--server", "default", "p54-metrics", "--server", "test"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, p54CapLabel) {
		t.Errorf("metrics output missing the %q label, got:\n%s", p54CapLabel, output)
	}
	if !strings.Contains(output, p54CapQualifier) {
		t.Errorf("metrics output missing the %q qualifier, got:\n%s", p54CapQualifier, output)
	}
	// disk_used_bytes is still reported as its own, measured fact.
	if !strings.Contains(output, "Disk Used:") {
		t.Errorf("metrics output lost the measured Disk Used line, got:\n%s", output)
	}
	assertNoDiskLimitClaim(t, "`bunker metrics <id>`", output)
}

// TestFormatPerFileCap_NoRatio pins the formatter itself: the agent cap
// renders as a bare value (or an explicit absence), never as a ratio.
func TestFormatPerFileCap_NoRatio(t *testing.T) {
	tests := []struct {
		name     string
		capBytes uint64
		want     string
	}{
		{"unset cap is an explicit absence, not a 0-byte cap", 0, noCapLabel},
		{"one GiB", 1024 * 1024 * 1024, "1.0 GB"},
		{"20 GiB", 20 * 1024 * 1024 * 1024, "20.0 GB"},
		{"sub-kilobyte", 512, "512 B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPerFileCap(tt.capBytes)
			if got != tt.want {
				t.Errorf("formatPerFileCap(%d) = %q, want %q", tt.capBytes, got, tt.want)
			}
			if strings.Contains(got, "%") {
				t.Errorf("formatPerFileCap(%d) = %q contains a percentage; the cap bounds ONE file "+
					"and nothing measures total usage against it (DF-BUNKER-54)", tt.capBytes, got)
			}
		})
	}
}

// TestFormatDisk_RemainsHostFilesystemOnly documents and pins the split: the
// percentage-carrying formatter stays, but only for the HOST filesystem
// surface (bunker status), where the denominator is a size the kernel really
// enforces. The agent cap must not route through it.
func TestFormatDisk_RemainsHostFilesystemOnly(t *testing.T) {
	// Host filesystem: used/total of a real mount → a ratio is truthful.
	if got := formatDisk(80*1024*1024*1024, 200*1024*1024*1024); got != "40% (80.0 GB/200.0 GB)" {
		t.Errorf("formatDisk host-filesystem output = %q, want %q", got, "40% (80.0 GB/200.0 GB)")
	}
	// The agent-cap surface must NOT produce that shape. If someone rewires
	// list/info/metrics back through formatDisk, this fails: the cap formatter
	// output is only ever a value.
	if got := formatPerFileCap(200 * 1024 * 1024 * 1024); strings.Contains(got, "(") || strings.Contains(got, "%") {
		t.Errorf("agent cap formatter produced the host-filesystem shape %q — the two surfaces "+
			"must stay separate (DF-BUNKER-54)", got)
	}
}

// TestDiskVocabularyDoesNotClaimQuota pins the shared label vocabulary: the
// words an operator sees must describe a per-file cap, and the qualifier must
// explicitly deny the quota reading (so a reworded label can't quietly
// reassert one).
func TestDiskVocabularyDoesNotClaimQuota(t *testing.T) {
	if !strings.Contains(perFileCapQualifier, "not a total-disk quota") {
		t.Errorf("perFileCapQualifier = %q must explicitly deny the total-disk reading", perFileCapQualifier)
	}
	// The denial itself mentions "total-disk quota" — remove the sanctioned
	// negation before scanning, or the check flags the very wording that makes
	// the label honest.
	const denial = "not a total-disk quota"
	for name, v := range map[string]string{
		"diskUsedHeader":      diskUsedHeader,
		"perFileCapHeader":    perFileCapHeader,
		"maxFileSizeLabel":    maxFileSizeLabel,
		"perFileCapQualifier": perFileCapQualifier,
	} {
		low := strings.ToLower(strings.ReplaceAll(v, denial, ""))
		for _, claim := range []string{"disk limit", "disk quota", "disk cap", "total disk"} {
			if strings.Contains(low, claim) {
				t.Errorf("%s = %q claims %q; the number is a LimitFSIZE per-file cap (DF-BUNKER-54)", name, v, claim)
			}
		}
	}
	// The usage header must name usage, not a limit.
	if diskUsedHeader != "Disk Used" {
		t.Errorf("diskUsedHeader = %q, want %q (a measured fact, not a bound)", diskUsedHeader, "Disk Used")
	}
}
