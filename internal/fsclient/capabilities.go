package fsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
		Watch struct {
			Modes       map[string]string `json:"modes"`
			HeartbeatMS int               `json:"heartbeat_ms"`
			MaxPaths    int               `json:"max_paths_per_event"`
		} `json:"watch"`
		Ops struct {
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
