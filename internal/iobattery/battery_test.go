package iobattery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All families must run against a plain local directory (the fallback mode)
// and produce a complete, machine-readable report.
func TestRunFullBatteryLocalFallback(t *testing.T) {
	dir := t.TempDir()
	rep, err := Run(Options{Target: dir, SizeMB: 4, Ops: 200, LoadWorkers: 2, Command: "bunker iobattery --test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"throughput", "latency_under_load", "metadata_ops", "iops", "cpu_per_byte", "negative_control"}
	if len(rep.Measurements) != len(want) {
		t.Fatalf("got %d families, want %d: %+v", len(rep.Measurements), len(want), familyNames(rep))
	}
	for i, w := range want {
		if rep.Measurements[i].Name != w {
			t.Errorf("family[%d] = %q, want %q", i, rep.Measurements[i].Name, w)
		}
	}

	for _, m := range rep.Measurements {
		if m.Command == "" {
			t.Errorf("%s: empty reproduction command", m.Name)
		}
		if m.ElapsedMs <= 0 {
			t.Errorf("%s: elapsed_ms = %v, want > 0", m.Name, m.ElapsedMs)
		}
		if m.Count <= 0 && m.Error == "" {
			t.Errorf("%s: zero count with no error", m.Name)
		}
		switch m.Name {
		case "throughput":
			if m.DerivedUnit != "MB/s" || m.Derived <= 0 {
				t.Errorf("throughput derived = %v %v", m.Derived, m.DerivedUnit)
			}
			if m.Detail["write_bytes"].(int64) == 0 || m.Detail["read_mb_s"].(float64) <= 0 {
				t.Errorf("throughput detail incomplete: %+v", m.Detail)
			}
		case "latency_under_load":
			for _, k := range []string{"p50_ms", "p95_ms", "p99_ms"} {
				if _, ok := m.Detail[k]; !ok {
					t.Errorf("latency detail missing %s", k)
				}
			}
			if p50 := m.Detail["p50_ms"].(float64); p50 > m.Derived {
				t.Errorf("p50 %v > p99 %v", p50, m.Derived)
			}
		case "metadata_ops":
			if m.DerivedUnit != "ops/s" || m.Derived <= 0 {
				t.Errorf("metadata derived = %v %v", m.Derived, m.DerivedUnit)
			}
		case "iops":
			if m.DerivedUnit != "IOPS" || m.Derived <= 0 {
				t.Errorf("iops derived = %v %v", m.Derived, m.DerivedUnit)
			}
		case "cpu_per_byte":
			if m.Derived <= 0 {
				t.Errorf("cpu_per_byte derived = %v, want bytes/cpu-second > 0", m.Derived)
			}
		case "negative_control":
			d := m.Detail
			if d["lever"] != "readahead" {
				t.Errorf("negative control lever = %v, want readahead", d["lever"])
			}
			if d["bdi_read_ahead_kb"] == "" {
				// permitted: not resolvable on every fs, but must say so
				if _, ok := d["bdi_read_ahead_kb"]; !ok {
					t.Errorf("negative control missing bdi_read_ahead_kb key")
				}
			}
			if _, ok := d["odirect_supported"]; !ok {
				t.Errorf("negative control missing odirect_supported")
			}
		}
	}

	// Machine-readable: valid JSON with the expected schema field.
	data, err := rep.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if doc["schema"] != "bunker.iobattery.v1" {
		t.Errorf("schema = %v", doc["schema"])
	}

	// No leftovers: the battery cleans its scratch dir.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("scratch not cleaned: %v", names)
	}
}

// The negative control must not fake a green result when one arm is
// unavailable (e.g. tmpfs rejects O_DIRECT): it records the reason.
func TestNegativeControlRecordsUnavailableArm(t *testing.T) {
	dir := t.TempDir()
	m := measureNegativeControl(dir, Options{SizeMB: 2, Ops: 10, LoadWorkers: 1})
	if m.Detail["lever"] != "readahead" {
		t.Fatalf("lever = %v", m.Detail["lever"])
	}
	if m.Detail["odirect_supported"] == false && m.Error == "" {
		t.Errorf("odirect arm failed but Error is empty — control silently incomplete")
	}
	if m.Detail["odirect_error"] == "" && m.Detail["odirect_supported"] == false {
		t.Errorf("odirect unsupported but no odirect_error recorded")
	}
}

// Validation: missing/invalid target must fail loudly.
func TestRunTargetValidation(t *testing.T) {
	if _, err := Run(Options{}); err == nil {
		t.Error("empty target: want error")
	}
	if _, err := Run(Options{Target: "/nonexistent-bfs057"}); err == nil {
		t.Error("missing target: want error")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := Run(Options{Target: file}); err == nil {
		t.Error("file target: want error (must be a directory)")
	}
}

// Derivations are pure arithmetic — table-driven per repo convention.
func TestDerivedMetrics(t *testing.T) {
	cases := []struct {
		name    string
		got     float64
		want    float64
		deltaOK bool
	}{
		{"mbps 64MiB in 2s", mbps(64<<20, seconds(2)), 32.0, false},
		{"ops/s 100 in 4s", opsPerSec(100, seconds(4)), 25.0, false},
		{"mbps zero duration", mbps(64<<20, 0), 0, false},
		{"percentile p50 of 1..100", percentile(ints(1, 100), 0.50), 50, false},
		{"percentile p99 of 1..100", percentile(ints(1, 100), 0.99), 99, false},
		{"percentile empty", percentile(nil, 0.5), 0, false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The reproduction command must name the exact target and knobs.
func TestReproducibilityCommands(t *testing.T) {
	dir := t.TempDir()
	rep, err := Run(Options{Target: dir, SizeMB: 2, Ops: 100, LoadWorkers: 1, Command: "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range rep.Measurements {
		if !strings.Contains(m.Command, dir) {
			t.Errorf("%s: command %q does not name target %q", m.Name, m.Command, dir)
		}
	}
}

func familyNames(r *Report) []string {
	out := []string{}
	for _, m := range r.Measurements {
		out = append(out, m.Name)
	}
	return out
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

func ints(lo, hi int) []float64 {
	out := []float64{}
	for i := lo; i <= hi; i++ {
		out = append(out, float64(i))
	}
	return out
}
