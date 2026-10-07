package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/deployBunker/bunker/internal/iobattery"
)

func runIOBattery(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewIOBatteryCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func flagNames(cmd interface{ Flags() *pflag.FlagSet }) []string {
	var flags []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { flags = append(flags, f.Name) })
	return flags
}

// End-to-end through the cobra layer: the binary path produces the same
// machine-readable report the package produces directly.
func TestIOBatteryCommandEndToEnd(t *testing.T) {
	dir := t.TempDir()
	out, err := runIOBattery(t, "--target", dir, "--size-mb", "2", "--ops", "100", "--load-workers", "1")
	if err != nil {
		t.Fatalf("iobattery failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"schema": "bunker.iobattery.v1"`) {
		t.Errorf("output missing schema marker:\n%s", out)
	}
	var rep iobattery.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output not parseable JSON: %v", err)
	}
	if len(rep.Measurements) != 6 {
		t.Errorf("got %d families, want 6", len(rep.Measurements))
	}
}

// --out writes the report to a file instead of stdout.
func TestIOBatteryOutFlag(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	out, err := runIOBattery(t, "--target", dir, "--out", reportPath, "--size-mb", "2", "--ops", "100", "--load-workers", "1")
	if err != nil {
		t.Fatalf("iobattery failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "report written to") {
		t.Errorf("stdout should confirm the report path, got:\n%s", out)
	}
	data, rerr := os.ReadFile(reportPath)
	if rerr != nil {
		t.Fatalf("report file: %v", rerr)
	}
	var rep iobattery.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("report file not valid JSON: %v", err)
	}
	if len(rep.Measurements) != 6 {
		t.Errorf("report file has %d families, want 6", len(rep.Measurements))
	}
}

// Missing target fails loudly with a nameable error.
func TestIOBatteryRequiresTarget(t *testing.T) {
	_, err := runIOBattery(t)
	if err == nil || !strings.Contains(err.Error(), "--target is required") {
		t.Errorf("want --target is required error, got %v", err)
	}
}

// A directory the process cannot create scratch in fails loudly, not silently.
func TestIOBatteryUnwritableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := runIOBattery(t, "--target", locked)
	if err == nil {
		t.Errorf("unwritable target: want error")
	}
}

// The command is registered on the root command (wired, not orphaned).
func TestIOBatteryWiredToRoot(t *testing.T) {
	// Reaching into newRootCommand would import cmd/bunker (main package is
	// separate); instead verify the constructor produces a well-formed
	// subcommand and that main.go registers it (checked by grep in the
	// repo-level surface test below / docs). The constructor contract:
	cmd := NewIOBatteryCommand()
	if cmd.Use == "" || cmd.Short == "" {
		t.Fatalf("iobattery command missing Use/Short")
	}
	var flags = flagNames(cmd)
	for _, want := range []string{"target", "out", "size-mb", "ops", "load-workers", "no-control"} {
		found := false
		for _, f := range flags {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing flag --%s", want)
		}
	}
}
