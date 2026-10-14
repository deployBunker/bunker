package agent

// ── GAP-119 unit-sandboxing knob set (guarded/hostile opt-in) ──────────────
//
// WHAT THIS IS
//
// The per-agent rootless-dockerd transient unit (buildRootlessDockerdArgs) can
// carry a systemd sandboxing property set. It is armed per daemon by
// agent.hardened_unit_properties (config) / BUNKERD_AGENT_HARDENED_UNIT_
// PROPERTIES (env) and it is OFF by default: with the flag off the systemd-run
// argv is byte-identical to a daemon without the feature (pinned by
// TestGAP119_FlagOffAddsNoProperties), so no existing deployment changes.
//
// WHY THE SET IS WHAT IT IS — MEASURED, NOT CHOSEN
//
// Every candidate was measured against the REAL rootless start path, in the
// spawn unit's own posture (systemd-run --system --uid=<agent uid>, rootlesskit
// 3.1.0 with the setuid newuidmap mapping path that
// internal/agent/rootless_prereqs.go declares a hard prerequisite, uid holding
// an /etc/subuid range the way configureSubIDs gives every agent one). One
// property per arm, control arm first; the probe is re-runnable:
//
//	probes/knob-safety/probe-h-unit-hardening.sh
//
//	control (no property)             -> IN-NS-uid=0 MAP-OK
//	NoNewPrivileges=yes               -> BREAKS: "failed to setup UID/GID map:
//	     newuidmap ... write to uid_map failed: Operation not permitted"
//	CapabilityBoundingSet=            -> BREAKS: "newuidmap ... open of uid_map
//	     failed: Permission denied"
//	RestrictNamespaces=yes            -> BREAKS: "failed to start the child:
//	     fork/exec /proc/self/exe: operation not permitted"
//	SystemCallFilter=@system-service  -> BREAKS: "child exited: signal: bad
//	     system call (core dumped)" (SIGSYS)
//	ProtectSystem=strict              -> BREAKS: "creating a state directory:
//	     mkdir /tmp/rootlesskit...: read-only file system"; and separately
//	     "mkdir: Read-only file system" for a runtime dir under /run — the
//	     directory the unit must create its docker.sock in
//	ProtectHome=yes                   -> BREAKS the unit's own exec:
//	     "exec /home/kara/.local/bin/go: Permission denied" (the spawn unit's
//	     command is $HOME/bin/dockerd-rootless.sh and dockerd's data root is
//	     $HOME/.local/share/docker)
//	RestrictSUIDSGID=yes              -> SAFE: mapping OK; setuid-file CREATION
//	     denied ("chmod: Operation not permitted"); exec of the pre-existing
//	     setuid newuidmap unaffected
//	ProtectKernelTunables=yes         -> SAFE: mapping OK
//	ProtectKernelModules=yes          -> SAFE: mapping OK
//	ProtectClock=yes                  -> SAFE: mapping OK
//	ProtectHostname=yes               -> SAFE: mapping OK
//
// The composed shipped set was then measured end to end: rootlesskit's uid
// mapping succeeded, --net=slirp4netns came up, and the unit could create its
// own /run socket dir and write $HOME.
//
// WHY THE BREAKERS ARE EXCLUDED AND NOT "SHIPPED WITH A CAVEAT"
//
// They are measured spawn-breakers, not risks: the uid mapping is a hard
// prerequisite of the rootless runtime, rootlesskit refuses to start without
// it, and the spawn then fails at StageDockerdStart — so "the hardened tier can
// not spawn an agent at all". The row's own constraint set says the same for
// ProtectSystem=strict (it also breaks useradd's /etc locking on root units
// with a misleading lock error), and the GAP-114 matrix records ProtectSystem
// as a measured no-op for user-level units, i.e. a property that would
// advertise protection it does not deliver.
//
// NoNewPrivileges deserves its own note because the dispatch brief listed it:
// it is neutralized-by-measurement here, not by taste. NNP makes execve ignore
// the setuid bit (verified: a setuid-root binary in the same unit printed
// euid=0 in the control arm and euid=1000 with the knob), and this daemon's
// rootless uid mapping goes through the setuid newuidmap/newgidmap helpers —
// rootlesskit then dies with the "failed to setup UID/GID map" error above.
// NNP can only be armed on a host whose mapping is set up WITHOUT setuid
// helpers (e.g. a single-uid mapping), which is a rootless-stack change rather
// than a unit knob; until that exists, arming it here would mean "the guarded
// tier cannot spawn".
//
// MemoryDenyWriteExecute is excluded as well, but note the honest scope of the
// usual claim: it forbids W+X memory mappings, which runtimes that JIT or load
// code into anonymous memory need. On this host BOTH a trivial Go binary and
// rootlesskit itself ran under the knob, and systemd 259 did NOT read it as
// implying NoNewPrivileges (NoNewPrivs: 0 was measured under it) — so its cost
// is the JIT/plugin-runtime class, not "every Go binary". It is left out
// because the classes it does break are not ones an agent tier should discover
// at runtime.

// Verdicts of the GAP-119 classification, in the vocabulary the row requires.
const (
	// unitHardeningSafe — measured to leave the rootless start path working.
	unitHardeningSafe = "SAFE"
	// unitHardeningNeedsException — acceptable only with a documented
	// exception (an allow-list window, a ReadWritePaths set, a mapping that
	// does not need setuid helpers). Not armed by the flag.
	unitHardeningNeedsException = "NEEDS-EXCEPTION"
	// unitHardeningBreaks — measured to break the rootless start path of this
	// daemon. Never armed by the flag.
	unitHardeningBreaks = "BREAKS"
)

// unitHardeningProperty is ONE candidate systemd sandboxing property for the
// agent spawn unit. The table below is the single source of the shipped set: a
// property reaches the unit argv only through this table, so adding one means
// classifying it AND writing down the failure mode it was measured against
// (the row's rule: never ship an unclassified knob).
type unitHardeningProperty struct {
	// Name is the systemd property name as written in the unit argv.
	Name string
	// Value is the property value exactly as written (may be empty — the
	// drop-everything spelling of CapabilityBoundingSet).
	Value string
	// Verdict is one of the three constants above.
	Verdict string
	// InSet reports whether the flag-gated set arms this property.
	InSet bool
	// Risk documents what the property can cost a legitimate workload — the
	// line an operator reads before arming the flag.
	Risk string
	// Evidence names the measurement that decided the verdict.
	Evidence string
}

// unitHardeningTable is the GAP-119 classification of every candidate the row
// and the dispatch brief name. Order is emit order for the InSet members.
func unitHardeningTable() []unitHardeningProperty {
	return []unitHardeningProperty{
		{
			Name:    "RestrictSUIDSGID",
			Value:   "yes",
			Verdict: unitHardeningSafe,
			InSet:   true,
			Risk: "An agent workload cannot CREATE setuid/setgid files (measured: " +
				"`chmod 4755` returns EPERM inside the unit). Its own images rarely " +
				"need to, and running a PRE-EXISTING setuid binary is unaffected " +
				"(newuidmap still executes), so the rootless helpers keep working. " +
				"A workload that ships its own setuid helper breaks.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit newuidmap mapping OK; " +
				"setuid creation denied; newuidmap exec OK",
		},
		{
			Name:    "ProtectKernelTunables",
			Value:   "yes",
			Verdict: unitHardeningSafe,
			InSet:   true,
			Risk: "Mounts /proc/sys, /proc/sysrq-trigger, /proc/irq, /proc/fs etc. " +
				"read-only for the unit, so per-network-namespace sysctl writes that a " +
				"workload would make through the agent's own netns (`docker run " +
				"--sysctl net.ipv4.ip_unprivileged_port_start=0`) are denied. The " +
				"daemon and the rootless runtime need none of these.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit newuidmap mapping + " +
				"slirp4netns networking OK with the property armed",
		},
		{
			Name:    "ProtectKernelModules",
			Value:   "yes",
			Verdict: unitHardeningSafe,
			InSet:   true,
			Risk: "Denies module load/unload. Loading a module needs CAP_SYS_MODULE, " +
				"which the unit's non-root uid never holds, so this is a " +
				"defence-in-depth knob with no measured cost; a workload that expects " +
				"to modprobe (it could not before either) fails.",
			Evidence: "probe-h-unit-hardening.sh: mapping OK; accepted by systemd 259 " +
				"in the spawn posture",
		},
		{
			Name:    "ProtectClock",
			Value:   "yes",
			Verdict: unitHardeningSafe,
			InSet:   true,
			Risk: "Blocks writes to /dev/rtc, the system clock (CAP_SYS_TIME) and the " +
				"monotonic/realtime offsets. A non-root uid cannot do any of that " +
				"anyway, so the measured cost is zero; a workload that sets the " +
				"container clock through a privileged container would fail.",
			Evidence: "probe-h-unit-hardening.sh: mapping OK; property available since " +
				"systemd 245 (hosts below that refuse it loudly at systemd-run)",
		},
		{
			Name:    "ProtectHostname",
			Value:   "yes",
			Verdict: unitHardeningSafe,
			InSet:   true,
			Risk: "Blocks changing the host/domain name (CAP_SYS_ADMIN in the initial " +
				"user namespace — the unit's uid never holds it) and the kernel's " +
				"hostname/domainname sysctls. Zero measured cost here; workloads that " +
				"rename the CONTAINER hostname are unaffected (that is the container's " +
				"own UTS namespace).",
			Evidence: "probe-h-unit-hardening.sh: mapping OK; property available since " +
				"systemd 242",
		},
		{
			Name:    "NoNewPrivileges",
			Value:   "yes",
			Verdict: unitHardeningBreaks,
			InSet:   false,
			Risk: "MEASURED SPAWN-BREAKER on this daemon's rootless path. NNP makes " +
				"execve ignore the setuid/setgid bit (measured: a setuid-root binary " +
				"printed euid=0 without the knob and euid=1000 with it), and the " +
				"rootless uid mapping runs through the setuid newuidmap/newgidmap " +
				"helpers that rootless_prereqs.go requires: rootlesskit then dies with " +
				"'failed to setup UID/GID map: newuidmap ... write to uid_map failed: " +
				"Operation not permitted', so dockerd never starts and the spawn fails " +
				"at StageDockerdStart. Arm it only on a host whose mapping needs no " +
				"setuid helper (a rootless-stack change, not a unit knob).",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit mapping fails under NNP; " +
				"setuid gain neutralized (euid 0 -> 1000)",
		},
		{
			Name:    "CapabilityBoundingSet",
			Value:   "",
			Verdict: unitHardeningBreaks,
			InSet:   false,
			Risk: "MEASURED SPAWN-BREAKER. Dropping the bounding set empty removes " +
				"CAP_SETUID/CAP_SETGID from the unit entirely, so the setuid " +
				"newuidmap helper can no longer write a uid_map even though it still " +
				"runs: rootlesskit fails with 'newuidmap ... open of uid_map failed: " +
				"Permission denied'. It is also redundant here — a unit with User= " +
				"running as a non-root uid already has every capability set dropped " +
				"by systemd.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit mapping fails under an " +
				"empty CapabilityBoundingSet",
		},
		{
			Name:    "RestrictNamespaces",
			Value:   "yes",
			Verdict: unitHardeningBreaks,
			InSet:   false,
			Risk: "MEASURED SPAWN-BREAKER, and a design conflict: rootless Docker IS a " +
				"user namespace. With the knob armed rootlesskit fails to start its " +
				"child at all ('fork/exec /proc/self/exe: operation not permitted'), " +
				"and every agent container would need one too. Only an allow-list " +
				"(NamespaceAllowList with the namespaces rootlesskit needs) could " +
				"ever be considered, and that needs its own measurement.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit child fails to start under " +
				"RestrictNamespaces=yes",
		},
		{
			Name:    "SystemCallFilter",
			Value:   "@system-service",
			Verdict: unitHardeningBreaks,
			InSet:   false,
			Risk: "MEASURED SPAWN-BREAKER in the allow-list form: the filter lacks " +
				"syscalls the rootless stack needs and the child dies with SIGSYS " +
				"('child exited: signal: bad system call (core dumped)') — an " +
				"illegible death, not a clean error. The deny-list form is worse: the " +
				"matrix measured a deny-listed agent workload killed at exit 159 " +
				"(128+31) under systemd where the same deny under docker seccomp " +
				"returned a clean EPERM. Any use must start from a SystemCallLog-only " +
				"window on a real workload battery; that is a staged row of its own.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit child dies with SIGSYS " +
				"under @system-service",
		},
		{
			Name:    "ProtectSystem",
			Value:   "strict",
			Verdict: unitHardeningBreaks,
			InSet:   false,
			Risk: "MEASURED SPAWN-BREAKER. strict mounts the whole hierarchy read-only " +
				"except /dev,/proc,/sys, and this unit must CREATE state: its docker " +
				"socket and XDG_RUNTIME_DIR under /run/bunker/<agent>/ and dockerd's " +
				"data root under $HOME (measured: 'mkdir: Read-only file system' for a " +
				"/run path, and rootlesskit's own state directory fails the same way). " +
				"Shipping it would also repeat the documented useradd failure class " +
				"(read-only /etc -> misleading lock-contention error) on root units, " +
				"and the GAP-114 matrix measured it as a NO-OP for user-level units — " +
				"i.e. it would advertise protection it does not deliver. A future row " +
				"could ship strict WITH an explicit ReadWritePaths exception set " +
				"(measured working: ReadWritePaths=/run/<dir> re-opened a strict unit) " +
				"but the exception list must then cover every write path of the " +
				"rootless stack.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit state dir + /run mkdir " +
				"fail with EROFS under ProtectSystem=strict; ReadWritePaths re-opens",
		},
		{
			Name:    "ProtectHome",
			Value:   "yes",
			Verdict: unitHardeningBreaks,
			InSet:   false,
			Risk: "MEASURED SPAWN-BREAKER. The spawn unit's own command lives under " +
				"$HOME ($HOME/bin/dockerd-rootless.sh) and dockerd's data root is " +
				"$HOME/.local/share/docker, so hiding /home makes the unit unable to " +
				"exec its own binary (measured: 'exec /home/...: Permission denied'). " +
				"A future row could recover it with ProtectHome=tmpfs plus " +
				"BindReadOnlyPaths=$HOME/bin and ReadWritePaths=$HOME/.local/share — " +
				"an explicit exception set, not a bare yes.",
			Evidence: "probe-h-unit-hardening.sh: exec of a $HOME binary denied under " +
				"ProtectHome=yes on a --system unit",
		},
		{
			Name:    "MemoryDenyWriteExecute",
			Value:   "yes",
			Verdict: unitHardeningNeedsException,
			InSet:   false,
			Risk: "Forbids memory that is writable and executable at once, which is " +
				"fine for the Go runtime (measured: rootlesskit and a compiled Go " +
				"binary both ran under the knob on systemd 259, and NoNewPrivs stayed " +
				"0 — this systemd does NOT read MDWE as implying NoNewPrivileges) but " +
				"breaks runtimes that JIT or load code into anonymous memory. Left out " +
				"so the guarded tier does not discover that class at runtime; arming " +
				"it is a per-image decision.",
			Evidence: "probe-h-unit-hardening.sh: rootlesskit + Go binary run under " +
				"MemoryDenyWriteExecute=yes (so the exclusion is the JIT/plugin class, " +
				"not this path)",
		},
		{
			Name:    "ProtectControlGroups",
			Value:   "yes",
			Verdict: unitHardeningNeedsException,
			InSet:   false,
			Risk: "Remounts cgroupfs read-only in the unit's namespace (measured: " +
				"'mkdir: Read-only file system'), while the agent's own rootless " +
				"dockerd/containerd write their delegated cgroup subtree. The GAP-114 " +
				"matrix marks it 'audit before enabling' for exactly that reason. " +
				"Needs a container battery at the guarded tier before it can be " +
				"default-armed.",
			Evidence: "probe-h-unit-hardening.sh: cgroup write denied under the knob; " +
				"matrix §Unit-sandbox set classifies it ACCEPTED-but-effect-UNMEASURED",
		},
	}
}

// hardenedUnitKnobs resolves the GAP-119 flag into the unit property set.
// It is the ONLY way the set reaches the unit: buildRootlessDockerdArgs renders
// what this returns, and the spawn stamps the same slice on the agent record so
// the applied set is reportable (a bound that is not reported is not a bound).
//
// Disabled (the default and the shipped state) returns nil — no property, no
// argv element, byte-identical behavior.
func hardenedUnitKnobs(enabled bool) []SystemdKnob {
	if !enabled {
		return nil
	}
	var knobs []SystemdKnob
	for _, p := range unitHardeningTable() {
		if !p.InSet {
			continue
		}
		knobs = append(knobs, SystemdKnob{Name: p.Name, Value: p.Value, Scope: KnobScopeUnit})
	}
	return knobs
}

// hardenedUnitKnobNames is the structured-log view of the same resolution: the
// property names the flag armed, or nil when it is off. Kept next to
// hardenedUnitKnobs so a log line can never disagree with the argv.
func hardenedUnitKnobNames(enabled bool) []string {
	knobs := hardenedUnitKnobs(enabled)
	if len(knobs) == 0 {
		return nil
	}
	names := make([]string, 0, len(knobs))
	for _, k := range knobs {
		names = append(names, k.Name)
	}
	return names
}
