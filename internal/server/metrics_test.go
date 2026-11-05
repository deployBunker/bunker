package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
	"github.com/go-chi/chi/v5"
)

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
	registerMetricsRoute(r, true, tracker, "test-version", "127.0.0.1:8080")
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
	registerMetricsRoute(disabled, false, tracker, "test-version", "127.0.0.1:8080")
	res = httptest.NewRecorder()
	disabled.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if res.Code != http.StatusNotFound {
		t.Errorf("disabled status = %d, want 404", res.Code)
	}
}
