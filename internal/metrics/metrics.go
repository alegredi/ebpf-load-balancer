package metrics

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"proxy/pkg/bpf"
)

var (
	XdpPacketsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loadbalancer",
			Subsystem: "xdp",
			Name:      "packets_total",
			Help:      "Total number of packets processed by XDP hook.",
		},
		[]string{"action"},
	)

	XdpBytesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "loadbalancer",
			Subsystem: "xdp",
			Name:      "bytes_total",
			Help:      "Total bytes processed by XDP hook.",
		},
	)

	BackendHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "loadbalancer",
			Subsystem: "backend",
			Name:      "healthy",
			Help:      "Backend health status (1 = healthy, 0 = unhealthy).",
		},
		[]string{"backend"},
	)

	BackendActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "loadbalancer",
			Subsystem: "backend",
			Name:      "active_connections",
			Help:      "Current active connections handled by the backend.",
		},
		[]string{"backend"},
	)

	HealthcheckFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loadbalancer",
			Subsystem: "healthcheck",
			Name:      "failures_total",
			Help:      "Total number of failed health checks.",
		},
		[]string{"backend"},
	)
)

func init() {
	prometheus.MustRegister(
		XdpPacketsTotal,
		XdpBytesTotal,
		BackendHealthy,
		BackendActiveConnections,
		HealthcheckFailuresTotal,
	)
}

// StartTelemetryCollector periodically scrapes per-CPU statistics from the eBPF kernel maps.
func StartTelemetryCollector(ctx context.Context, mgr *bpf.Manager, interval time.Duration) {
	if mgr == nil {
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var lastFwd, lastDrop, lastPass, lastBytes uint64

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats, err := mgr.GetStats()
				if err != nil {
					log.Printf("[METRICS] Error reading BPF stats: %v", err)
					continue
				}

				if stats.ForwardedPackets >= lastFwd {
					XdpPacketsTotal.WithLabelValues("forwarded").Add(float64(stats.ForwardedPackets - lastFwd))
					lastFwd = stats.ForwardedPackets
				}
				if stats.DroppedPackets >= lastDrop {
					XdpPacketsTotal.WithLabelValues("dropped").Add(float64(stats.DroppedPackets - lastDrop))
					lastDrop = stats.DroppedPackets
				}
				if stats.PassPackets >= lastPass {
					XdpPacketsTotal.WithLabelValues("pass").Add(float64(stats.PassPackets - lastPass))
					lastPass = stats.PassPackets
				}
				if stats.RxBytes >= lastBytes {
					XdpBytesTotal.Add(float64(stats.RxBytes - lastBytes))
					lastBytes = stats.RxBytes
				}
			}
		}
	}()
}

// StartMetricsServer starts an HTTP listener exposing Prometheus metrics at /metrics.
func StartMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("[INFO] Prometheus metrics exporter listening on %s/metrics", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[ERROR] Metrics server error: %v", err)
		}
	}()

	return server
}
