package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMetricsEndpoint(t *testing.T) {
	addr := "127.0.0.1:19092"
	srv := StartMetricsServer(addr)
	defer srv.Close()

	time.Sleep(50 * time.Millisecond)

	BackendHealthy.WithLabelValues("http://test-backend").Set(1)
	XdpPacketsTotal.WithLabelValues("forwarded").Inc()

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("failed to scrape metrics endpoint: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "loadbalancer_backend_healthy") {
		t.Errorf("metrics response missing loadbalancer_backend_healthy")
	}
	if !strings.Contains(bodyStr, "loadbalancer_xdp_packets_total") {
		t.Errorf("metrics response missing loadbalancer_xdp_packets_total")
	}

	_ = srv.Shutdown(context.Background())
}
