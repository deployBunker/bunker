package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/go-chi/chi/v5"
)

func TestRPCMetricsFamiliesAndLabelLaw(t *testing.T) {
	audit.RPCRequestsTotal.Reset()
	audit.RPCDurationSeconds.Reset()
	audit.RPCRequestsTotal.WithLabelValues("SpawnAgent", "ok").Inc()
	audit.RPCRequestsTotal.WithLabelValues("DestroyAgent", "not_found").Inc()
	audit.RPCDurationSeconds.WithLabelValues("SpawnAgent").Observe(0.2)

	tracker := resource.NewTracker(1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := newMetricsHandler(tracker, "test", "127.0.0.1:8080", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := res.Body.String()
	for _, want := range []string{
		`bunker_rpc_requests_total{code="ok",op="SpawnAgent"} 1`,
		`bunker_rpc_requests_total{code="not_found",op="DestroyAgent"} 1`,
		`bunker_rpc_duration_seconds_bucket{op="SpawnAgent",le="0.25"} 1`,
		`# HELP bunker_rpc_requests_total `,
		`# HELP bunker_rpc_duration_seconds `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("RPC metrics output missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"agent_id=", "user=", "session_id=", "remote_addr=", "error="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("RPC metrics output contains forbidden label %q:\n%s", forbidden, body)
		}
	}
}

func TestLifecycleMetricsFamiliesAndBoundedLabels(t *testing.T) {
	tracker := resource.NewTracker(7, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := tracker.Register(&resource.AgentRecord{AgentID: "agent-secret"}); err != nil {
		t.Fatal(err)
	}
	metrics := agent.LifecycleMetrics{
		SpawnOK: 2, SpawnError: 1, DestroyOK: 3, DestroyRefused: 1, DestroyError: 1,
		DestroyArchived: 2, DestroySkippedArchive: 1, RefusedLiveProcesses: 1,
		RefusedHomeRetained: 2, RefusedUserdelFailed: 3,
		ArchiveFailuresArchive: 4, ArchiveFailuresPrune: 5, ArchiveFailuresVerify: 6,
		ArchiveBytes: 1234, RegistryEvents: 17, PortAvailable: 9, PortpoolDriftReleased: 8,
		RegistryDestroyRefused: map[string]uint64{"live_processes": 1, "home_retained": 2, "userdel_failed": 3},
	}
	handler := newMetricsHandler(tracker, "test-version", "127.0.0.1:8080", func() agent.LifecycleMetrics { return metrics })
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := res.Body.String()
	for _, sample := range []string{
		`bunker_destroy_refused_total{reason="live_processes"} 1`,
		`bunker_registry_destroy_refused{reason="live_processes"} 1`,
		`bunker_registry_destroy_refused{reason="home_retained"} 2`,
		`bunker_archive_failures_total{reason="verify"} 6`,
		`bunker_archive_bytes 1234`,
		`bunker_registry_events 17`,
		`bunker_portpool_available 9`,
		`bunker_portpool_drift_released_total 8`,
		`bunker_agents_current 1`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("metrics output missing %q:\n%s", sample, body)
		}
	}
	for _, forbidden := range []string{"agent_id=", "user=", "session_id=", "remote_addr=", "error="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics output contains forbidden high-cardinality label %q:\n%s", forbidden, body)
		}
	}
}

func TestWebDAVMetricsFamiliesAndDisabledMode(t *testing.T) {
	tracker := resource.NewTracker(1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot := webdavMetricsSnapshot{
		Enabled:        true,
		OverflowsTotal: 2, UnvouchedTotal: 3, RescansTotal: 4,
		InstallFailuresTotal: 5, BackendErrorsTotal: 6, WatchedChangesTotal: 7,
		HeartbeatsTotal: 8, LoopTicksTotal: 9, HeartbeatAgeSeconds: 1.5,
		PushActive: 2, PushActiveMax: 3, PushDropsTotal: 4, PushGapsTotal: 5,
	}

	enabled := newMetricsHandler(tracker, "test-version", "127.0.0.1:8080", nil,
		func() webdavMetricsSnapshot { return snapshot })
	res := httptest.NewRecorder()
	enabled.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := res.Body.String()
	for _, sample := range []string{
		`bunker_webdav_enabled 1`,
		`bunker_webdav_watch_overflows_total 2`,
		`bunker_webdav_watch_install_failures_total 5`,
		`bunker_webdav_watch_heartbeat_age_seconds 1.5`,
		`bunker_webdav_push_active 2`,
		`bunker_webdav_push_drops_total 4`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("WebDAV metrics output missing %q:\n%s", sample, body)
		}
	}
	if n := strings.Count(body, "\nbunker_webdav_watch_"); n < 8 {
		t.Errorf("expected at least 8 bunker_webdav_watch_ series, got %d", n)
	}

	// No snapshot (WebDAV disabled): the families must still be present, at 0,
	// and bunker_webdav_enabled must be the discriminator that says so —
	// OBS-BUNKER-009. heartbeat_age_seconds is 0 in BOTH modes, which is why
	// the enabled gauge is the only way to tell "off" from "healthy".
	disabled := newMetricsHandler(tracker, "test-version", "127.0.0.1:8080", nil)
	res = httptest.NewRecorder()
	disabled.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body = res.Body.String()
	for _, sample := range []string{
		`bunker_webdav_enabled 0`,
		`bunker_webdav_watch_overflows_total 0`,
		`bunker_webdav_watch_heartbeat_age_seconds 0`,
		`bunker_webdav_push_active 0`,
		`bunker_webdav_push_gaps_total 0`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("disabled-mode WebDAV metrics missing %q:\n%s", sample, body)
		}
	}
	// An enabled snapshot with a healthy watcher also reports age 0: the
	// enabled gauge is the only series that separates "off" from "healthy"
	// (asserted above via the exact sample line; the HELP prose also contains
	// the words "1 when", so a bare substring test here would false-positive).
}

func TestMetricsRouteEnabledAndDisabled(t *testing.T) {
	tracker := resource.NewTracker(7, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, rec := range []*resource.AgentRecord{
		{AgentID: "soon-1", ExpiresAt: time.Now().Add(5 * time.Minute)},
		{AgentID: "soon-2", ExpiresAt: time.Now().Add(10 * time.Minute)},
		{AgentID: "later", ExpiresAt: time.Now().Add(15 * time.Minute)},
	} {
		if err := tracker.Register(rec); err != nil {
			t.Fatal(err)
		}
	}
	r := chi.NewRouter()
	registerMetricsRoute(r, true, tracker, "test-version", "127.0.0.1:8080", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("enabled status = %d, want 200", res.Code)
	}
	body := res.Body.String()
	want := []string{"bunker_info", "bunker_uptime_seconds", "bunker_agents_current", "bunker_agents_max", "bunker_ttl_expiring_soon"}
	for _, name := range want {
		if !strings.Contains(body, "# HELP "+name+" ") || (!strings.Contains(body, name+"{") && !strings.Contains(body, name+" ")) {
			t.Errorf("metrics output missing HELP or sample for %s:\n%s", name, body)
		}
	}
	for _, sample := range []string{"bunker_agents_current 3", "bunker_agents_max 7", "bunker_ttl_expiring_soon 2", `bunker_info{rest_addr="127.0.0.1:8080",version="test-version"} 1`} {
		if !strings.Contains(body, sample) {
			t.Errorf("metrics output missing %q:\n%s", sample, body)
		}
	}
	sampleLines := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "bunker_") {
			sampleLines++
		}
	}
	if sampleLines < 5 {
		t.Errorf("expected at least five bunker_ sample lines, got %d", sampleLines)
	}

	disabled := chi.NewRouter()
	registerMetricsRoute(disabled, false, tracker, "test-version", "127.0.0.1:8080", nil)
	res = httptest.NewRecorder()
	disabled.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if res.Code != http.StatusNotFound {
		t.Errorf("disabled status = %d, want 404", res.Code)
	}
}
