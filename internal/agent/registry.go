// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// agentUserPrefix is the Linux username prefix every managed agent uses.
// A system user with this prefix is a managed agent (bunker-<agent_id>).
const agentUserPrefix = "bunker-"

// agentPasswdPath is the user database the host-plane probes read: the
// reconciliation sweep (defaultListSystemAgents) and the residue inventory
// (ResidueInventory) must answer "which managed agents exist on this host?"
// from the SAME file, so the path is declared once. Var because a non-root
// regression has to exercise both against a fixture database instead of the
// real /etc/passwd (production value is /etc/passwd, i.e. unchanged behaviour).
var agentPasswdPath = "/etc/passwd"

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
		Image:            rec.Image,
		// GAP-116: the effective preset and knob set ride the durable record
		// so a replayed/adopted agent keeps reporting what it was spawned
		// with. The property lists convert from the wire type into the
		// registry's plain-JSON form so the durable record stays proto-free.
		SafetyPreset:    rec.SafetyPreset,
		UnitProperties:  protoToRegistryProperties(rec.UnitProperties),
		SliceProperties: protoToRegistryProperties(rec.SliceProperties),
	}
}

// protoToRegistryProperties converts the wire property list into the
// registry's plain-JSON form (nil-safe).
func protoToRegistryProperties(props []*v1.SystemdProperty) []registry.SystemdProperty {
	if len(props) == 0 {
		return nil
	}
	out := make([]registry.SystemdProperty, 0, len(props))
	for _, p := range props {
		out = append(out, registry.SystemdProperty{Name: p.GetName(), Value: p.GetValue()})
	}
	return out
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
		Image:             rec.Image,
		// GAP-116: restore the effective preset/knob reporting from the durable
		// record so a replayed or adopted agent reports the set it was spawned
		// with. The property lists convert from the registry's plain-JSON form
		// back to the wire type.
		SafetyPreset:    rec.SafetyPreset,
		UnitProperties:  registryPropertiesToProto(rec.UnitProperties),
		SliceProperties: registryPropertiesToProto(rec.SliceProperties),
	}
}

// registryPropertiesToProto converts registry SystemdProperty rows into the
// wire type (nil-safe: a pre-GAP-116 record has none and reports nil).
func registryPropertiesToProto(props []registry.SystemdProperty) []*v1.SystemdProperty {
	if len(props) == 0 {
		return nil
	}
	out := make([]*v1.SystemdProperty, 0, len(props))
	for _, p := range props {
		out = append(out, &v1.SystemdProperty{Name: p.Name, Value: p.Value})
	}
	return out
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

// ownerMarkerFilename is the per-agent ownership marker written at spawn time
// next to `.bunker/ports`. It carries the spawning daemon's instance identity
// plus (informationally) the pool geometry that daemon allocated from, so two
// daemons sharing ONE host and OVERLAPPING port pools can still tell each
// other's agents apart (DF-BUNKER-18).
const ownerMarkerFilename = "owner"

// persistedOwnerPath is the per-agent ownership marker path.
func persistedOwnerPath(home string) string {
	return filepath.Join(home, ".bunker", ownerMarkerFilename)
}

// readPersistedOwner reads an agent's daemon-ownership marker.
//
// The file has two lines, newline-terminated:
//
//	<daemon-instance-id>
//	<pool-start>-<pool-end>
//
// ok is true only when line 1 carries a non-empty instance id; an unreadable
// file, an empty file, or an empty line 1 all mean "no marker", and the
// caller then falls back to its legacy (marker-absent) handling. Line 2 is
// parsed leniently and is INFORMATIONAL ONLY — a missing or unparseable pool
// line still yields ok=true with the instance id, because the pool geometry
// deliberately plays no part in the ownership decision.
func readPersistedOwner(home string) (instanceID string, poolStart, poolEnd uint32, ok bool) {
	if home == "" {
		return "", 0, 0, false
	}
	data, err := os.ReadFile(persistedOwnerPath(home))
	if err != nil {
		return "", 0, 0, false
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	id := strings.TrimSpace(lines[0])
	if id == "" {
		return "", 0, 0, false
	}
	if len(lines) > 1 {
		line := strings.TrimSpace(lines[1])
		parts := strings.SplitN(line, "-", 2)
		if len(parts) == 2 {
			s, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 32)
			e, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 32)
			if err1 == nil && err2 == nil {
				poolStart, poolEnd = uint32(s), uint32(e)
			}
		}
	}
	return id, poolStart, poolEnd, true
}

// poolFingerprint renders this daemon's pool geometry ("<start>-<end>") for
// the ownership marker written at spawn time. It falls back to the configured
// range when no allocator is active. The fingerprint is recorded and logged
// for operators only: it NEVER takes part in the ownership decision.
func (m *AgentManager) poolFingerprint() string {
	if m.portAlloc != nil {
		start, end := m.portAlloc.Bounds()
		return fmt.Sprintf("%d-%d", start, end)
	}
	return fmt.Sprintf("%d-%d", m.cfg.Agent.PortRangeStart, m.cfg.Agent.PortRangeEnd)
}

// instanceIDFile is the daemon-scoped file under agent.base_data_dir holding
// this daemon's instance identity.
const instanceIDFile = "instance"

// daemonInstanceIDPath is this daemon's instance identity path.
func daemonInstanceIDPath(baseDataDir string) string {
	return filepath.Join(baseDataDir, instanceIDFile)
}

// loadOrCreateDaemonInstanceID resolves this daemon's restart-stable instance
// identity: the identity file is CREATED once (0600, random 32-hex-char id)
// and READ on every later start. Regenerating the id on each start is
// deliberately forbidden — a fresh id every restart would make this daemon's
// OWN previously-spawned agents read as foreign and leak forever.
//
// The creation is O_CREATE|O_EXCL so two daemons starting concurrently cannot
// each walk away with a different id (the loser reads the winner's file).
// Every failure is returned, never fatal: the caller logs a warning and keeps
// the identity empty, which disables the ownership check (every marker is then
// treated as absent, i.e. today's behaviour). Failure must never fail closed
// into destroying more, and never fail daemon start.
func loadOrCreateDaemonInstanceID(baseDataDir string, logger *slog.Logger) (string, error) {
	if strings.TrimSpace(baseDataDir) == "" {
		return "", errors.New("no agent base_data_dir configured")
	}
	path := daemonInstanceIDPath(baseDataDir)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("instance identity file %s is empty", path)
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("read instance identity %s: %w", path, err)
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(baseDataDir, 0o755); err != nil {
		return "", fmt.Errorf("create agent base data dir %s: %w", baseDataDir, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the race (or a file appeared between the read and the
			// open): whoever wrote it first owns the identity.
			if data, rerr := os.ReadFile(path); rerr == nil {
				if existing := strings.TrimSpace(string(data)); existing != "" {
					return existing, nil
				}
			}
		}
		return "", fmt.Errorf("create instance identity %s: %w", path, err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("write instance identity %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close instance identity %s: %w", path, err)
	}
	// The mode is asserted by the caller's tests, not assumed: a pre-existing
	// file keeps its own mode, and umask can only narrow the requested one.
	logger.Debug("daemon instance identity created", "path", path)
	return id, nil
}

// defaultListSystemAgents enumerates managed agents present on the host by
// reading agentPasswdPath (/etc/passwd) for bunker-* users. It is the production
// probe behind AgentManager.listSystemAgents (tests inject their own).
func defaultListSystemAgents() ([]SystemAgent, error) {
	f, err := os.Open(agentPasswdPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", agentPasswdPath, err)
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
