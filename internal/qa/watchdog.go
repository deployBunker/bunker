// Package qa contains the controller-side recovery policy for detached QA
// batteries. The shell harness owns transport and agent lifecycle; this
// package keeps the age/artifact policy and evidence-before-destroy invariant
// testable without a bunker host.
package qa

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Agent describes the persisted part of one detached QA battery.
type Agent struct {
	ID               string
	CreatedAt        time.Time
	Collected        bool
	BatteryArtifacts []string
}

// DetectOrphans returns agents whose battery is still uncollected, has at least
// one artifact, and is at least ttl old. Future timestamps and non-positive TTLs
// are ignored rather than treated as safe-to-destroy candidates.
func DetectOrphans(agents []Agent, now time.Time, ttl time.Duration) []Agent {
	if ttl <= 0 {
		return nil
	}
	orphans := make([]Agent, 0)
	for _, agent := range agents {
		if agent.ID == "" || agent.Collected || len(agent.BatteryArtifacts) == 0 {
			continue
		}
		if agent.CreatedAt.IsZero() || agent.CreatedAt.After(now) {
			continue
		}
		if now.Sub(agent.CreatedAt) >= ttl {
			orphans = append(orphans, agent)
		}
	}
	return orphans
}

// Recovery describes the durable evidence location for a recovered agent.
type Recovery struct {
	AgentID       string
	QuarantineDir string
	Artifacts     []string
}

// RecoverOrphan copies every existing battery artifact into a timestamped
// quarantine directory before calling destroy. If copying fails, destroy is
// not called: evidence preservation is the safety gate for destructive cleanup.
func RecoverOrphan(agent Agent, quarantineRoot string, now time.Time, destroy func(string) error) (Recovery, error) {
	if agent.ID == "" {
		return Recovery{}, fmt.Errorf("recover orphan: agent id is empty")
	}
	if quarantineRoot == "" {
		return Recovery{}, fmt.Errorf("recover orphan %s: quarantine root is empty", agent.ID)
	}
	if destroy == nil {
		return Recovery{}, fmt.Errorf("recover orphan %s: destroy callback is nil", agent.ID)
	}

	dir := filepath.Join(quarantineRoot, fmt.Sprintf("%s-%s", agent.ID, now.UTC().Format("20060102T150405.000000000Z")))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Recovery{}, fmt.Errorf("create quarantine for %s: %w", agent.ID, err)
	}

	copied := make([]string, 0, len(agent.BatteryArtifacts))
	for _, source := range agent.BatteryArtifacts {
		if source == "" {
			continue
		}
		info, err := os.Stat(source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Recovery{}, fmt.Errorf("stat battery artifact %s: %w", source, err)
		}
		if !info.Mode().IsRegular() {
			return Recovery{}, fmt.Errorf("battery artifact %s is not a regular file", source)
		}
		destination := filepath.Join(dir, filepath.Base(source))
		if err := copyFile(source, destination, info.Mode().Perm()); err != nil {
			return Recovery{}, fmt.Errorf("quarantine battery artifact %s: %w", source, err)
		}
		copied = append(copied, destination)
	}
	if len(copied) == 0 {
		return Recovery{}, fmt.Errorf("recover orphan %s: no battery artifacts preserved", agent.ID)
	}
	if err := destroy(agent.ID); err != nil {
		return Recovery{}, fmt.Errorf("destroy orphan %s after quarantine: %w", agent.ID, err)
	}
	return Recovery{AgentID: agent.ID, QuarantineDir: dir, Artifacts: copied}, nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(destination)
		}
		_ = out.Close()
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
