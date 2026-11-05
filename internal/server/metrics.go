package server

import (
	"net/http"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ttlExpiringSoonWindow = 10 * time.Minute

func registerMetricsRoute(r interface {
	Get(string, http.HandlerFunc)
}, enabled bool, tracker *resource.Tracker, buildVersion, restAddr string) {
	if enabled {
		r.Get("/metrics", newMetricsHandler(tracker, buildVersion, restAddr).ServeHTTP)
	}
}

func newMetricsHandler(tracker *resource.Tracker, buildVersion, restAddr string) http.Handler {
	reg := prometheus.NewRegistry()
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
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
