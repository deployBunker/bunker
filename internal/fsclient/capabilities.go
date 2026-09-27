package fsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CapabilityWatch is the capability document's `extensions.watch` block — what
// the SERVER can honestly claim about watching this target, and what it has
// counted while doing it (SPEC-watcher-capability §8.2). The client REPORTS this
// block rather than re-deriving it: the server is the only side that can measure
// its own watcher, so a client that invented these figures would be guessing at
// another process's state. Every nullable field is a pointer, because the
// document publishes `null` WITH a reason and a decoded zero would erase the
// difference between "absent" and "zero" (BFS-045's null rule).
type CapabilityWatch struct {
	Name        string            `json:"name"`
	V           int               `json:"v"`
	Mode        string            `json:"mode"`
	Modes       map[string]string `json:"modes"`
	HeartbeatMS int               `json:"heartbeat_ms"`
	MaxPaths    int               `json:"max_paths_per_event"`
	// State is the runtime state vocabulary: watching|overflow|lost|absent
	// (§3.2). `absent` is a fact about the TARGET, not a failure of the client.
	State string `json:"state"`
	// BlocksPush is the server's own verdict on whether the absence of the push
	// form blocks the channel. nil when the document does not publish it.
	BlocksPush *bool  `json:"blocks_push"`
	Backend    string `json:"backend"`
	// Reason is the named reason the watcher is absent (§4: watch_unsupported_
	// platform, watch_limit_exhausted, watch_target_netbacked, watch_partial_
	// coverage, watch_install_failed, watch_lost, watch_boundary_split). The
	// document publishes it as null when there is none, so it is a pointer.
	Reason *string `json:"reason"`
	Detail string  `json:"detail"`
	Target struct {
		MountType     string `json:"mount_type"`
		MountPoint    string `json:"mount_point"`
		NetworkBacked bool   `json:"network_backed"`
		// BoundarySplit is §4.8's THREE-VALUED field: the document publishes the
		// JSON boolean true/false or the STRING "unknown" (triBool, so a silent
		// false is never used for an unmeasurable fact). It is decoded as raw
		// JSON because a typed bool would refuse "unknown" and a typed string
		// would refuse true — and a decode failure here would take the whole
		// document with it.
		BoundarySplit  json.RawMessage `json:"boundary_split"`
		BoundaryReason string          `json:"boundary_split_reason"`
	} `json:"target"`
	Coverage *struct {
		DirectoriesDesired int      `json:"directories_desired"`
		DirectoriesWatched int      `json:"directories_watched"`
		MissingCount       int      `json:"missing_count"`
		Missing            []string `json:"missing"`
		MissingReason      string   `json:"missing_reason"`
		Complete           bool     `json:"complete"`
		Headroom           int      `json:"headroom"`
	} `json:"coverage"`
	// Absence of the whole coverage/counters group is reported with its reason
	// (§3.3 rule 2: fields that cannot be measured are null WITH a reason).
	CoverageReason string `json:"coverage_reason"`
	Counters       *struct {
		OverflowsTotal        int64  `json:"overflows_total"`
		UnvouchedTotal        int64  `json:"unvouched_total"`
		RescansTotal          int64  `json:"rescans_total"`
		InstallFailuresTotal  int64  `json:"install_failures_total"`
		BackendErrorsTotal    int64  `json:"backend_errors_total"`
		OverflowDroppedEvents *int64 `json:"overflow_dropped_events"`
		OverflowDroppedReason string `json:"overflow_dropped_reason"`
		UnvouchedReason       string `json:"unvouched_reason"`
		LastEventAgeMS        *int64 `json:"last_event_age_ms"`
		ChangedSince          string `json:"changed_since"`
		HeartbeatsTotal       int64  `json:"heartbeats_total"`
		EventLoopTicks        int64  `json:"event_loop_ticks"`
		WatchesAdded          int64  `json:"watches_added"`
	} `json:"counters"`
	CountersReason string `json:"counters_reason"`
	Liveness       *struct {
		Vouched        bool   `json:"vouched"`
		Stalled        bool   `json:"stalled"`
		LivenessSource string `json:"liveness_source"`
	} `json:"liveness"`
}

// Present reports whether the document carried a watch block at all. A build
// that predates the block sends nothing, and "nothing was published" is a
// different fact from "the watcher is absent" — the client must be able to say
// which one it is looking at (BFS-045's null reasons).
func (w *CapabilityWatch) Present() bool {
	return w != nil && (w.Name != "" || w.State != "")
}

// Capabilities is the decoded capability document (BFS-004 §4.2). A consumer
// that does not know DocumentVersion must fail CLOSED rather than proceed
// (§4.2 rule 3), which is why the version is checked in Handshake.
type Capabilities struct {
	Surface    string   `json:"surface"`
	Document   int      `json:"document_version"`
	Build      string   `json:"build"`
	ReadOnly   bool     `json:"read_only"`
	Methods    []string `json:"methods"`
	Protocols  []string `json:"protocols"`
	Extensions struct {
		Watch CapabilityWatch `json:"watch"`
		Ops   struct {
			Catalogue []string `json:"catalogue"`
			Live      []string `json:"live"`
		} `json:"op"`
		Identity struct {
			Hash string `json:"hash"`
			ETag string `json:"etag"`
		} `json:"identity"`
		Snapshot struct {
			DepthInfinity bool `json:"depth_infinity"`
			IncludeHash   bool `json:"include_hash"`
		} `json:"snapshot"`
		// Rev is the revision declaration (BFS-004 §4.2's `extensions.rev`).
		// Only `kind` is decoded, and it is the ONE fact that says what the
		// served revision token can move for (SPEC-watcher-capability §7.1):
		// the client reports its coverage from this field, never from the
		// token's shape or from a config flag (§8.1).
		Rev struct {
			Kind string `json:"kind"`
		} `json:"rev"`
	} `json:"extensions"`
	Limits struct {
		MaxRequestBytes int64 `json:"max_request_bytes"`
		MaxResultBytes  int64 `json:"max_result_bytes"`
	} `json:"limits"`
	Degradations []struct {
		Capability string `json:"capability"`
		Scope      string `json:"scope"`
		Mode       string `json:"mode"`
		Detail     string `json:"detail"`
	} `json:"degradations"`
	// Raw keeps the undecoded document: a field this client does not know is
	// not a reason to lose the rest of what the server said.
	Raw json.RawMessage `json:"-"`
}

// SurfaceInfo is what one OPTIONS + capability handshake learns.
type SurfaceInfo struct {
	Allow        []string
	DAV          string
	Extensions   string
	Tree         string
	Rev          string
	Proto        string
	DocVersion   int
	Capabilities *Capabilities
	// WatcherAvailable reports whether the push channel can be established at
	// all: `watch` answering anything other than capability_unavailable (or the
	// capability document naming a live watcher).
	WatcherAvailable bool
	// PollOpAvailable reports whether the poll form (`X-Bunker-Op: events`) is
	// served by this build.
	PollOpAvailable bool
	// Degradation is the server's own structured refusal for the watcher, when
	// it sent one: capability + scope + mode, verbatim.
	Degradation *OpError
}

// Handshake performs the bind-time capability exchange under the 5 s bind
// deadline (§7.2): OPTIONS for the verb set and the extension header, then the
// `capabilities` op for the document. A failure here refuses the mount.
func (c *Client) Handshake(ctx context.Context) (*SurfaceInfo, *OpError) {
	bindCtx, cancel := context.WithTimeout(ctx, c.opt.BindTimeout)
	defer cancel()

	req, err := c.newRequest(bindCtx, http.MethodOptions, "", nil)
	if err != nil {
		return nil, &OpError{Op: "OPTIONS", Errno: ErrnoENOTCONN, Cause: CauseUnreachableConnect, Err: err}
	}
	resp, oerr := c.do(bindCtx, req)
	if oerr != nil {
		return nil, oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.failureFrom("OPTIONS", "", resp, nil)
	}
	info := &SurfaceInfo{
		DAV:        resp.Header.Get("DAV"),
		Extensions: resp.Header.Get("X-Bunker-Extensions"),
		Tree:       resp.Header.Get("X-Bunker-Tree"),
		Rev:        resp.Header.Get("X-Bunker-Rev"),
		Proto:      resp.Header.Get("X-Bunker-Proto"),
	}
	for _, m := range strings.Split(resp.Header.Get("Allow"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			info.Allow = append(info.Allow, m)
		}
	}
	if v := resp.Header.Get("X-Bunker-Capabilities"); v != "" {
		fmt.Sscanf(v, "%d", &info.DocVersion)
	}

	// The document itself: the handshake every other op is discovered through.
	// A build without it is still usable over the standard surface, so its
	// absence is recorded rather than fatal.
	caps, capErr := c.CapabilitiesDoc(bindCtx)
	if capErr != nil {
		info.Degradation = capErr
		return info, nil
	}
	info.Capabilities = caps
	info.DocVersion = caps.Document
	// Fail closed on an unknown document version (§4.2 rule 3): an unknown
	// surface is never "proceed anyway".
	if caps.Document != 0 && caps.Document != 1 {
		return info, &OpError{
			Op: "capabilities", Errno: ErrnoEOPNOTSUPP, Cause: CauseLocalCapability, Verdict: VerdictCapabilityUnavailable,
			Detail: fmt.Sprintf("unknown capability document version %d; failing closed rather than proceeding", caps.Document),
		}
	}
	// The watcher's availability is probed, never assumed (§4.2 E-6): the
	// declared degradation is what tells us, and it carries the mode in force.
	watchErr := c.probeOp(bindCtx, "watch")
	info.WatcherAvailable = watchErr == nil
	if watchErr != nil && watchErr.Verdict == VerdictCapabilityUnavailable {
		info.Degradation = watchErr
	}
	eventsErr := c.probeOp(bindCtx, "events")
	info.PollOpAvailable = eventsErr == nil
	return info, nil
}

// probeOp sends a minimal op call and returns the OpError, if any. A refused op
// is information, not a failure of the handshake.
func (c *Client) probeOp(ctx context.Context, op string) *OpError {
	_, err := c.Op(ctx, op, map[string]any{"paths": []string{}}, nil)
	return err
}

// CapabilitiesDoc fetches and decodes the capability document.
func (c *Client) CapabilitiesDoc(ctx context.Context) (*Capabilities, *OpError) {
	var out struct {
		Capabilities Capabilities `json:"capabilities"`
	}
	env, err := c.Op(ctx, "capabilities", nil, &out)
	if err != nil {
		return nil, err
	}
	caps := out.Capabilities
	caps.Raw = env.Raw
	c.capsMu.Lock()
	c.caps = &caps
	c.capsAt = timeNow()
	c.capsMu.Unlock()
	return &caps, nil
}

// Heartbeat returns the server's declared heartbeat period, defaulting to the
// ≤ 30 s the surface pins when the document does not name one.
func (caps *Capabilities) Heartbeat() time.Duration {
	if caps != nil && caps.Extensions.Watch.HeartbeatMS > 0 {
		return time.Duration(caps.Extensions.Watch.HeartbeatMS) * time.Millisecond
	}
	return 30 * time.Second
}

// MaxPathsPerEvent returns the declared cap on one event's path list. A longer
// list is an overflow (drop everything, re-snapshot), never a partial drop.
func (caps *Capabilities) MaxPathsPerEvent() int {
	if caps != nil && caps.Extensions.Watch.MaxPaths > 0 {
		return caps.Extensions.Watch.MaxPaths
	}
	return 4096
}

// RevKind is the revision kind the document DECLARES ("git" | "counter", or ""
// when the document names none). It is read, never inferred.
//
// This is the fact the invalidator's coverage report is built from: the kind is
// what says which class of change a served revision token moves for
// (SPEC-watcher-capability §7.1 — `git` moves when the served tree's HEAD ref
// moves, `counter` on mutations through the surface), and §8.1 forbids learning
// a capability from anything but the running process's own declaration. A
// document that names no kind leaves the client reporting that it claims no
// coverage, rather than guessing from the token's shape.
func (caps *Capabilities) RevKind() string {
	if caps == nil {
		return ""
	}
	return caps.Extensions.Rev.Kind
}
