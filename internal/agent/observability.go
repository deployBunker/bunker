package agent

import (
	"os"
	"path/filepath"
)

// LifecycleMetrics is an immutable snapshot of lifecycle state for /metrics.
type LifecycleMetrics struct {
	SpawnOK, SpawnError                          uint64
	DestroyOK, DestroyRefused, DestroyError      uint64
	DestroyArchived, DestroySkippedArchive       uint64
	RefusedLiveProcesses, RefusedHomeRetained    uint64
	RefusedUserdelFailed                         uint64
	ArchiveFailuresArchive, ArchiveFailuresPrune uint64
	ArchiveFailuresVerify                        uint64
	ArchiveBytes, RegistryEvents, PortAvailable  uint64
	PortpoolDriftReleased                        uint64
	RegistryDestroyRefused                       map[string]uint64
}

// LifecycleMetrics returns counters and bounded in-memory lifecycle gauges.
// It does not inspect Docker or enumerate agents except for the existing
// registry's in-memory refusal state.
func (m *AgentManager) LifecycleMetrics() LifecycleMetrics {
	v := LifecycleMetrics{
		SpawnOK: m.spawnOK.Load(), SpawnError: m.spawnError.Load(),
		DestroyOK: m.destroyOK.Load(), DestroyRefused: m.destroyRefused.Load(),
		DestroyError: m.destroyError.Load(), DestroyArchived: m.destroyArchived.Load(),
		DestroySkippedArchive:  m.destroySkippedArchive.Load(),
		RefusedLiveProcesses:   m.refusedLiveProcesses.Load(),
		RefusedHomeRetained:    m.refusedHomeRetained.Load(),
		RefusedUserdelFailed:   m.refusedUserdelFailed.Load(),
		ArchiveFailuresArchive: m.archiveFailuresArchive.Load(),
		ArchiveFailuresPrune:   m.archiveFailuresPrune.Load(),
		ArchiveFailuresVerify:  m.archiveFailuresVerify.Load(),
		PortpoolDriftReleased:  m.portpoolDriftReleased.Load(),
		RegistryDestroyRefused: map[string]uint64{"live_processes": 0, "home_retained": 0, "userdel_failed": 0},
	}
	if m.portAlloc != nil {
		v.PortAvailable = uint64(m.portAlloc.Available())
	}
	if m.registry != nil {
		v.RegistryEvents = uint64(m.registry.EventCount())
		for _, rec := range m.registry.Live() {
			if rec.Refusal == nil {
				continue
			}
			switch rec.Refusal.Status {
			case StatusLiveProcesses:
				v.RegistryDestroyRefused["live_processes"]++
			case StatusHomeRetained:
				v.RegistryDestroyRefused["home_retained"]++
			case StatusUserdelFailed:
				v.RegistryDestroyRefused["userdel_failed"]++
			}
		}
	}
	v.ArchiveBytes = archiveDirectoryBytes(m.cfg.Agent.DestroyArchiveDirOrDefault())
	return v
}

func archiveDirectoryBytes(dir string) uint64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total uint64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".gz" {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Size() > 0 {
			total += uint64(info.Size())
		}
	}
	return total
}
