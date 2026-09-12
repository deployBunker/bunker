// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// agentUserPrefix is the Linux username prefix every managed agent uses.
// A system user with this prefix is a managed agent (bunker-<agent_id>).
const agentUserPrefix = "bunker-"

// persistedPortsPath is the per-agent file written at spawn time holding the
// agent's allocated port sub-range ("<start>-<end>"). It is the durable
// metadata reconciliation uses to restore an orphan's EXACT reservation.
func persistedPortsPath(home string) string {
	return filepath.Join(home, ".bunker", "ports")
}

// recordToRegistry converts an in-memory agent record into the durable
// registry form (full current state, so replay can rebuild it verbatim).
func recordToRegistry(rec *resource.AgentRecord) *registry.Record {
	if rec == nil {
		return nil
	}
	return &registry.Record{
		AgentID:          rec.AgentID,
		Status:           rec.Status,
		CreatedAt:        rec.CreatedAt,
		ExpiresAt:        rec.ExpiresAt,
		PortStart:        rec.PortRangeStart,
		PortEnd:          rec.PortRangeEnd,
		Limits:           rec.Limits,
		SSHKeyPath:       rec.SshPrivateKeyPath,
		SSHFSMount:       rec.SshfsMount,
		DockerHostTunnel: rec.DockerHostTunnel,
		PublicURL:        rec.PublicURL,
		TailnetIP:        rec.TailnetIP,
	}
}

// registryToRecord converts a replayed registry record into a tracker record.
func registryToRecord(rec *registry.Record) *resource.AgentRecord {
	if rec == nil {
		return nil
	}
	status := rec.Status
	if status == "" {
		status = "running"
	}
	return &resource.AgentRecord{
		AgentID:           rec.AgentID,
		Status:            status,
		Limits:            rec.Limits,
		CreatedAt:         rec.CreatedAt,
		ExpiresAt:         rec.ExpiresAt,
		PortRangeStart:    rec.PortStart,
		PortRangeEnd:      rec.PortEnd,
		PublicURL:         rec.PublicURL,
		SshPrivateKeyPath: rec.SSHKeyPath,
		TailnetIP:         rec.TailnetIP,
		SshfsMount:        rec.SSHFSMount,
		DockerHostTunnel:  rec.DockerHostTunnel,
	}
}

// readPersistedPortRange reads an agent's persisted port sub-range from its
// home directory. Returns ok=false when the file is missing or malformed —
// the caller then adopts the agent without a pinned reservation.
func readPersistedPortRange(home string) (start, end uint32, ok bool) {
	if home == "" {
		return 0, 0, false
	}
	data, err := os.ReadFile(persistedPortsPath(home))
	if err != nil {
		return 0, 0, false
	}
	line := strings.TrimSpace(string(data))
	parts := strings.SplitN(line, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	s, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 32)
	e, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uint32(s), uint32(e), true
}

// defaultListSystemAgents enumerates managed agents present on the host by
// reading /etc/passwd for bunker-* users. It is the production probe behind
// AgentManager.listSystemAgents (tests inject their own).
func defaultListSystemAgents() ([]SystemAgent, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, fmt.Errorf("read /etc/passwd: %w", err)
	}
	defer f.Close()

	var out []SystemAgent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 6 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, agentUserPrefix) {
			continue
		}
		id := strings.TrimPrefix(name, agentUserPrefix)
		if id == "" || !validAgentID.MatchString(id) {
			continue
		}
		out = append(out, SystemAgent{AgentID: id, Username: name, Home: fields[5]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan /etc/passwd: %w", err)
	}
	return out, nil
}
