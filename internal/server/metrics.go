package server

import (
	"net/http"
	"time"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ttlExpiringSoonWindow = 10 * time.Minute

type webdavMetricsSnapshot struct {
	OverflowsTotal, UnvouchedTotal, RescansTotal uint64
	InstallFailuresTotal, BackendErrorsTotal    uint64
	WatchedChangesTotal, HeartbeatsTotal        uint64
	LoopTicksTotal                              uint64
	HeartbeatAgeSeconds                        float64
	PushActive, PushActiveMax, PushDropsTotal   int64
	PushGapsTotal                               int64
}

func registerMetricsRoute(r interface {
	Get(string, http.HandlerFunc)
}, enabled bool, tracker *resource.Tracker, buildVersion, restAddr string, lifecycle func() agent.LifecycleMetrics, webdavSnapshot ...func() webdavMetricsSnapshot) {
	if enabled {
		r.Get("/metrics", newMetricsHandler(tracker, buildVersion, restAddr, lifecycle, webdavSnapshot...).ServeHTTP)
	}
}

func newMetricsHandler(tracker *resource.Tracker, buildVersion, restAddr string, lifecycle func() agent.LifecycleMetrics, webdavSnapshot ...func() webdavMetricsSnapshot) http.Handler {
	reg := prometheus.NewRegistry()
	readWebDAV := func() webdavMetricsSnapshot {
		if len(webdavSnapshot) == 0 || webdavSnapshot[0] == nil {
			return webdavMetricsSnapshot{}
		}
		return webdavSnapshot[0]()
	}
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "bunker_info",
			Help:        "Build and REST listener identity of this bunkerd instance.",
			ConstLabels: prometheus.Labels{"version": buildVersion, "rest_addr": restAddr},
		}, func() float64 { return 1 }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "bunker_uptime_seconds",
			Help: "Seconds since this bunkerd process started.",
		}, func() float64 { return time.Since(serverStartTime).Seconds() }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "bunker_agents_current",
			Help: "Number of agents currently tracked by bunkerd.",
		}, func() float64 { return float64(tracker.Count()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "bunker_agents_max",
			Help: "Maximum number of agents configured for this bunkerd instance.",
		}, func() float64 { return float64(tracker.MaxAgents()) }),
	)
	requests, duration := audit.RPCMetricCollectors()
	reg.MustRegister(requests, duration)
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "bunker_ttl_expiring_soon",
			Help: "Number of tracked agents expiring within the next ten minutes.",
		}, func() float64 {
			cutoff := time.Now().Add(ttlExpiringSoonWindow)
			var count int
			for _, rec := range tracker.List() {
				if rec.ExpiresAt.After(time.Now()) && !rec.ExpiresAt.After(cutoff) {
					count++
				}
			}
			return float64(count)
		}),
	)
	read := func() agent.LifecycleMetrics {
		if lifecycle == nil {
			return agent.LifecycleMetrics{RegistryDestroyRefused: map[string]uint64{}}
		}
		return lifecycle()
	}
	counter := func(name, help, label, value string, get func(agent.LifecycleMetrics) uint64) prometheus.Collector {
		return prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help, ConstLabels: prometheus.Labels{label: value}}, func() float64 {
			return float64(get(read()))
		})
	}
	for _, outcome := range []string{"ok", "error"} {
		outcome := outcome
		var get func(agent.LifecycleMetrics) uint64
		if outcome == "ok" {
			get = func(m agent.LifecycleMetrics) uint64 { return m.SpawnOK }
		} else {
			get = func(m agent.LifecycleMetrics) uint64 { return m.SpawnError }
		}
		reg.MustRegister(counter("bunker_spawn_total", "Agent spawn attempts by outcome.", "outcome", outcome, get))
	}
	for _, outcome := range []string{"ok", "refused", "error", "archived", "skipped_archive"} {
		outcome := outcome
		var get func(agent.LifecycleMetrics) uint64
		switch outcome {
		case "ok":
			get = func(m agent.LifecycleMetrics) uint64 { return m.DestroyOK }
		case "refused":
			get = func(m agent.LifecycleMetrics) uint64 { return m.DestroyRefused }
		case "error":
			get = func(m agent.LifecycleMetrics) uint64 { return m.DestroyError }
		case "archived":
			get = func(m agent.LifecycleMetrics) uint64 { return m.DestroyArchived }
		case "skipped_archive":
			get = func(m agent.LifecycleMetrics) uint64 { return m.DestroySkippedArchive }
		}
		reg.MustRegister(counter("bunker_destroy_total", "Agent destroy attempts by outcome.", "outcome", outcome, get))
	}
	for _, reason := range []string{"live_processes", "home_retained", "userdel_failed"} {
		reason := reason
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "bunker_destroy_refused_total", Help: "Agent destroy refusals by bounded reason.", ConstLabels: prometheus.Labels{"reason": reason}}, func() float64 {
			m := read()
			switch reason {
			case "live_processes":
				return float64(m.RefusedLiveProcesses)
			case "home_retained":
				return float64(m.RefusedHomeRetained)
			default:
				return float64(m.RefusedUserdelFailed)
			}
		}))
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "bunker_registry_destroy_refused", Help: "Live agents currently in destroy-refusal backoff by reason.", ConstLabels: prometheus.Labels{"reason": reason}}, func() float64 {
			return float64(read().RegistryDestroyRefused[reason])
		}))
	}
	for _, reason := range []string{"archive", "prune", "verify"} {
		reason := reason
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "bunker_archive_failures_total", Help: "Archive lifecycle failures by bounded reason.", ConstLabels: prometheus.Labels{"reason": reason}}, func() float64 {
			m := read()
			switch reason {
			case "archive":
				return float64(m.ArchiveFailuresArchive)
			case "prune":
				return float64(m.ArchiveFailuresPrune)
			default:
				return float64(m.ArchiveFailuresVerify)
			}
		}))
	}
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "bunker_archive_bytes", Help: "Bytes currently held by agent home archives."}, func() float64 { return float64(read().ArchiveBytes) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "bunker_registry_events", Help: "Current event count in the agent lifecycle registry."}, func() float64 { return float64(read().RegistryEvents) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "bunker_portpool_available", Help: "Free agent port ranges in the allocator."}, func() float64 { return float64(read().PortAvailable) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "bunker_portpool_drift_released_total", Help: "Port reservations released by the drift sweep."}, func() float64 { return float64(read().PortpoolDriftReleased) }),
	)
	webdavCounter := func(name, help string, get func(webdavMetricsSnapshot) uint64) prometheus.Collector {
		return prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, func() float64 { return float64(get(readWebDAV())) })
	}
	webdavGauge := func(name, help string, get func(webdavMetricsSnapshot) float64) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 { return get(readWebDAV()) })
	}
	for _, metric := range []struct {
		name, help string
		get        func(webdavMetricsSnapshot) uint64
	}{
		{"bunker_webdav_watch_overflows_total", "WebDAV watcher overflows.", func(m webdavMetricsSnapshot) uint64 { return m.OverflowsTotal }},
		{"bunker_webdav_watch_unvouched_total", "WebDAV watcher unvouched intervals.", func(m webdavMetricsSnapshot) uint64 { return m.UnvouchedTotal }},
		{"bunker_webdav_watch_rescans_total", "WebDAV watcher rescans.", func(m webdavMetricsSnapshot) uint64 { return m.RescansTotal }},
		{"bunker_webdav_watch_install_failures_total", "WebDAV watcher install failures.", func(m webdavMetricsSnapshot) uint64 { return m.InstallFailuresTotal }},
		{"bunker_webdav_watch_backend_errors_total", "WebDAV watcher backend errors.", func(m webdavMetricsSnapshot) uint64 { return m.BackendErrorsTotal }},
		{"bunker_webdav_watch_watched_changes_total", "WebDAV changes observed by the watcher.", func(m webdavMetricsSnapshot) uint64 { return m.WatchedChangesTotal }},
		{"bunker_webdav_watch_heartbeats_total", "WebDAV watcher heartbeats.", func(m webdavMetricsSnapshot) uint64 { return m.HeartbeatsTotal }},
		{"bunker_webdav_watch_loop_ticks_total", "WebDAV watcher event loop ticks.", func(m webdavMetricsSnapshot) uint64 { return m.LoopTicksTotal }},
	} {
		reg.MustRegister(webdavCounter(metric.name, metric.help, metric.get))
	}
	reg.MustRegister(
		webdavGauge("bunker_webdav_watch_heartbeat_age_seconds", "Seconds since the WebDAV watcher heartbeat.", func(m webdavMetricsSnapshot) float64 { return m.HeartbeatAgeSeconds }),
		webdavGauge("bunker_webdav_push_active", "Active WebDAV push subscribers.", func(m webdavMetricsSnapshot) float64 { return float64(m.PushActive) }),
		webdavGauge("bunker_webdav_push_active_max", "Maximum concurrent WebDAV push subscribers.", func(m webdavMetricsSnapshot) float64 { return float64(m.PushActiveMax) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "bunker_webdav_push_drops_total", Help: "WebDAV push subscriber event drops."}, func() float64 { return float64(readWebDAV().PushDropsTotal) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "bunker_webdav_push_gaps_total", Help: "WebDAV push subscriber gaps."}, func() float64 { return float64(readWebDAV().PushGapsTotal) }),
	)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
